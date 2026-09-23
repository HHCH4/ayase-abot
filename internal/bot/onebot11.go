package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"Abot/internal/agent"
	"github.com/gorilla/websocket"
)

const oneBotMessageLimit = 5000

// oneBotPlatform 连接 NapCat/Lagrange 等 OneBot v11 反向 WebSocket 服务端。
type oneBotPlatform struct {
	bot      Bot
	client   *http.Client
	dialer   *websocket.Dialer
	upgrader websocket.Upgrader

	mu            sync.Mutex
	conn          *websocket.Conn
	selfID        string
	server        *http.Server
	listener      net.Listener
	connections   chan *websocket.Conn
	actionWaiters map[string]oneBotActionWaiter
}

type oneBotEvent struct {
	PostType    string          `json:"post_type"`
	SelfID      json.RawMessage `json:"self_id"`
	MessageType string          `json:"message_type"`
	SubType     string          `json:"sub_type"`
	MessageID   json.RawMessage `json:"message_id"`
	UserID      json.RawMessage `json:"user_id"`
	GroupID     json.RawMessage `json:"group_id"`
	Sender      *oneBotSender   `json:"sender"`
	Message     json.RawMessage `json:"message"`
}

// oneBotSender 是 OneBot v11 事件中可选的用户展示信息；不同实现可能只
// 填 nickname、card 其中一个，因此这里全部按可选字段处理。
type oneBotSender struct {
	Nickname string `json:"nickname"`
	Card     string `json:"card"`
}

type oneBotSegment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// oneBotActionWaiter 保存一次带 echo 的 OneBot 动作请求，读取循环收到响应后
// 会按 echo 唤醒对应等待者，避免附件下载逻辑直接抢占 WebSocket 读端。
type oneBotActionWaiter struct {
	conn   *websocket.Conn
	result chan oneBotActionResult
}

// oneBotActionResult 是动作响应或连接关闭错误的统一返回值。
type oneBotActionResult struct {
	payload []byte
	err     error
}

// oneBotActionResponse 是 OneBot v11 动作响应的最小公共结构；具体 data 在
// get_file 中再按文件响应结构解码，保持其他动作响应的兼容性。
type oneBotActionResponse struct {
	Status  string          `json:"status"`
	RetCode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
	Echo    json.RawMessage `json:"echo"`
}

// oneBotFileResult 是 get_file 在 NapCat 等 OneBot 实现中返回的文件来源。
// file 可能是本地路径，url 可能是平台可访问的下载地址，base64 兼容少数实现。
type oneBotFileResult struct {
	File     string `json:"file"`
	URL      string `json:"url"`
	Base64   string `json:"base64"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
}

// oneBotAttachmentResolver 只负责把平台 file_id 解析成实际文件来源。
type oneBotAttachmentResolver func(context.Context, string) (oneBotFileResult, error)

// newOneBotPlatform 创建 OneBot v11 适配器；默认使用 NapCat 需要的反向 WS 服务端。
func newOneBotPlatform(item Bot) (*oneBotPlatform, error) {
	if err := item.Validate(); err != nil {
		return nil, err
	}
	return &oneBotPlatform{
		bot: item, client: &http.Client{Timeout: 30 * time.Second},
		dialer:        &websocket.Dialer{HandshakeTimeout: 10 * time.Second},
		upgrader:      websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		actionWaiters: make(map[string]oneBotActionWaiter),
	}, nil
}

// Run 根据连接模式启动正向客户端或反向服务端，并只消费 OneBot 的 message 事件。
func (p *oneBotPlatform) Run(ctx context.Context, handler Handler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.bot.effectiveOneBotMode() == OneBotModeClient {
		return p.runClient(ctx, handler)
	}
	return p.runReverseServer(ctx, handler)
}

// runClient 保留正向 WebSocket 客户端模式，兼容连接 NapCat WS 服务端的旧配置。
func (p *oneBotPlatform) runClient(ctx context.Context, handler Handler) error {
	header := http.Header{}
	if token := strings.TrimSpace(p.bot.OneBotAccessToken); token != "" {
		header.Set("Authorization", "Bearer "+token)
		header.Set("X-Client-Access-Token", token)
	}
	conn, _, err := p.dialer.DialContext(ctx, p.bot.Endpoint, header)
	if err != nil {
		return fmt.Errorf("OneBot WebSocket 连接失败: %w", err)
	}
	p.setConn(conn)
	defer func() {
		p.closeConn()
	}()
	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			p.closeConn()
		case <-stopCloser:
		}
	}()
	defer close(stopCloser)

	return p.consumeConnection(ctx, conn, handler)
}

// runReverseServer 启动反向 WebSocket 服务端，等待 NapCat 的 WebSocket 客户端连入。
func (p *oneBotPlatform) runReverseServer(ctx context.Context, handler Handler) error {
	host, address, path, err := p.bot.oneBotListenConfig()
	if err != nil {
		return err
	}
	listener, err := net.Listen(oneBotListenNetwork(host), address)
	if err != nil {
		return fmt.Errorf("OneBot 反向 WebSocket 监听失败（%s）: %w", address, err)
	}

	connections := make(chan *websocket.Conn, 2)
	mux := http.NewServeMux()
	mux.HandleFunc(path, p.acceptReverseConnection)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	p.mu.Lock()
	p.listener = listener
	p.server = server
	p.connections = connections
	p.mu.Unlock()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	stopCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// 仅取消 Run 的上下文时也要主动打断连接读取和 HTTP 监听。
			p.closeConn()
			_ = server.Close()
		case <-stopCloser:
		}
	}()
	defer func() {
		close(stopCloser)
		p.closeConn()
		_ = server.Shutdown(context.Background())
		p.mu.Lock()
		if p.server == server {
			p.server = nil
			p.listener = nil
			p.connections = nil
		}
		p.mu.Unlock()
	}()

	slog.Info("OneBot 反向 WebSocket 服务已监听", "adapter_id", p.bot.ID, "address", address, "path", path)
	for {
		select {
		case <-ctx.Done():
			return nil
		case conn := <-connections:
			if conn == nil {
				continue
			}
			if err := p.consumeConnection(ctx, conn, handler); err != nil && ctx.Err() == nil {
				// NapCat 重连是正常行为，单次断开不能让监听服务退出。
				slog.Warn("OneBot 反向 WebSocket 连接断开", "adapter_id", p.bot.ID, "error", err)
			}
			p.clearConn(conn)
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("OneBot 反向 WebSocket 服务异常: %w", err)
		}
	}
}

// acceptReverseConnection 完成 NapCat 客户端的鉴权和升级，并替换旧连接。
func (p *oneBotPlatform) acceptReverseConnection(writer http.ResponseWriter, request *http.Request) {
	if !p.authorizeReverseRequest(request) {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="abot-onebot"`)
		http.Error(writer, "OneBot WebSocket Token 无效", http.StatusUnauthorized)
		return
	}
	conn, err := p.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		slog.Warn("OneBot WebSocket 握手失败", "adapter_id", p.bot.ID, "error", err)
		return
	}
	p.mu.Lock()
	old := p.conn
	p.conn = conn
	connections := p.connections
	p.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if connections == nil {
		_ = conn.Close()
		return
	}
	select {
	case connections <- conn:
	default:
		_ = conn.Close()
		p.clearConn(conn)
		slog.Warn("OneBot WebSocket 连接等待队列已满", "adapter_id", p.bot.ID)
	}
}

// authorizeReverseRequest 支持 NapCat 常见的 Bearer、X-Client-Access-Token 和查询参数鉴权。
func (p *oneBotPlatform) authorizeReverseRequest(request *http.Request) bool {
	expected := strings.TrimSpace(p.bot.OneBotAccessToken)
	if expected == "" {
		return true
	}
	actual := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(actual) >= len("Bearer ") && strings.EqualFold(actual[:len("Bearer ")], "Bearer ") {
		actual = strings.TrimSpace(actual[len("Bearer "):])
	}
	if actual == "" {
		actual = strings.TrimSpace(request.Header.Get("X-Client-Access-Token"))
	}
	if actual == "" {
		actual = strings.TrimSpace(request.URL.Query().Get("access_token"))
	}
	return actual == expected
}

// consumeConnection 读取一个 OneBot 连接；心跳、生命周期事件和动作响应不会进入 Agent。
// 消息事件交给独立 goroutine 处理，保证处理 file_id 时等待 get_file 响应不会阻塞
// 唯一的 WebSocket 读循环。
func (p *oneBotPlatform) consumeConnection(ctx context.Context, conn *websocket.Conn, handler Handler) (returnErr error) {
	// 连接断开时唤醒所有等待中的动作请求，避免文件下载协程永久等待。
	defer func() {
		p.failActionWaiters(conn, returnErr)
	}()
	for {
		// 先读取原始 WebSocket 帧并打印，再解析事件，确保消息和动作响应不会因结构体字段不足而丢失。
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		slog.Info("OneBot WebSocket 响应", "adapter_id", p.bot.ID, "payload", string(payload))
		// 带 echo 的动作响应必须先交给等待者，不能被当作普通事件丢弃。
		if echo := oneBotResponseEcho(payload); echo != "" && p.deliverActionResponse(conn, echo, payload) {
			continue
		}
		var event oneBotEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			slog.Error("OneBot WebSocket 响应解析失败", "adapter_id", p.bot.ID, "payload", string(payload), "error", err)
			continue
		}
		if event.PostType != "message" {
			continue
		}
		if value := rawID(event.SelfID); value != "" {
			p.mu.Lock()
			p.selfID = value
			p.mu.Unlock()
		}
		// 文件消息需要通过同一条连接请求 get_file；异步处理可让读循环继续接收该响应。
		go p.handleMessageEvent(ctx, conn, event, handler)
	}
}

// handleMessageEvent 在独立 goroutine 中解析并投递单条消息，保留解析失败原因，
// 这样平台文件协议异常不会再被静默吞掉。
func (p *oneBotPlatform) handleMessageEvent(ctx context.Context, conn *websocket.Conn, event oneBotEvent, handler Handler) {
	message, err := p.messageFromEventOnConn(ctx, conn, event)
	if err != nil {
		slog.Warn("OneBot 入站消息处理失败", "adapter_id", p.bot.ID, "message_id", rawID(event.MessageID), "error", err)
		return
	}
	if handlerErr := handler(ctx, message); handlerErr != nil {
		slog.Warn("OneBot 入站消息交给机器人处理失败", "adapter_id", p.bot.ID, "message_id", message.ID, "error", handlerErr)
	}
}

// Test 在服务端模式检查监听地址是否可用；正在运行时由 Manager 直接报告监听正常。
func (p *oneBotPlatform) Test(ctx context.Context) error {
	if p.bot.effectiveOneBotMode() != OneBotModeClient {
		host, address, _, err := p.bot.oneBotListenConfig()
		if err != nil {
			return err
		}
		listener, err := net.Listen(oneBotListenNetwork(host), address)
		if err != nil {
			return fmt.Errorf("OneBot 反向 WebSocket 监听地址不可用（%s）: %w", address, err)
		}
		return listener.Close()
	}
	header := http.Header{}
	if token := strings.TrimSpace(p.bot.OneBotAccessToken); token != "" {
		header.Set("Authorization", "Bearer "+token)
		header.Set("X-Client-Access-Token", token)
	}
	conn, _, err := p.dialer.DialContext(ctx, p.bot.Endpoint, header)
	if err != nil {
		return fmt.Errorf("OneBot WebSocket 测试失败: %w", err)
	}
	return conn.Close()
}

// Send 使用 OneBot v11 send_msg 动作发回私聊或群聊消息。
func (p *oneBotPlatform) Send(ctx context.Context, message Message, text string) error {
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return ErrNotRunning
	}
	for index, chunk := range splitText(text, oneBotMessageLimit) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		params := map[string]any{
			"message_type": message.ChatType,
			"message":      chunk,
		}
		// OneBot 使用结构化消息段实现引用与 @，不拼接可被平台误解析的 CQ 文本。
		if index == 0 && (message.ReplyQuote || message.ReplyMention && isGroupChat(message.ChatType)) {
			segments := make([]map[string]any, 0, 3)
			if message.ReplyQuote && message.ReplyMessageID != "" {
				segments = append(segments, map[string]any{"type": "reply", "data": map[string]any{"id": oneBotIDValue(message.ReplyMessageID)}})
			}
			if message.ReplyMention && isGroupChat(message.ChatType) && message.UserID != "" {
				segments = append(segments, map[string]any{"type": "at", "data": map[string]any{"qq": oneBotIDValue(message.UserID)}})
			}
			segments = append(segments, map[string]any{"type": "text", "data": map[string]any{"text": chunk}})
			params["message"] = segments
		}
		if params["message_type"] == "group" {
			params["group_id"] = oneBotIDValue(message.ChatID)
		} else {
			params["message_type"] = "private"
			params["user_id"] = oneBotIDValue(message.ChatID)
		}
		action := map[string]any{"action": "send_msg", "params": params, "echo": fmt.Sprintf("abot-%d", time.Now().UnixNano())}
		// 记录发给 OneBot 的完整动作；对应的异步响应由 consumeConnection 原文记录。
		slog.Info("OneBot WebSocket 请求", "adapter_id", p.bot.ID, "action", action)
		p.mu.Lock()
		if p.conn == nil {
			p.mu.Unlock()
			slog.Error("OneBot WebSocket 请求失败", "adapter_id", p.bot.ID, "action", action, "error", ErrNotRunning)
			return ErrNotRunning
		}
		conn = p.conn
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := conn.WriteJSON(action)
		p.mu.Unlock()
		if err != nil {
			slog.Error("OneBot WebSocket 请求失败", "adapter_id", p.bot.ID, "action", action, "error", err)
			return fmt.Errorf("OneBot 发送消息失败: %w", err)
		}
	}
	return nil
}

// Close 关闭当前 WebSocket 和反向监听服务，使阻塞中的读取或监听立即返回。
func (p *oneBotPlatform) Close() error {
	p.closeConn()
	p.mu.Lock()
	server := p.server
	listener := p.listener
	p.mu.Unlock()
	if server != nil {
		return server.Close()
	}
	if listener != nil {
		return listener.Close()
	}
	return nil
}

func (p *oneBotPlatform) setConn(conn *websocket.Conn) {
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
}

func (p *oneBotPlatform) clearConn(conn *websocket.Conn) {
	p.mu.Lock()
	if p.conn == conn {
		p.conn = nil
	}
	p.mu.Unlock()
}

func (p *oneBotPlatform) closeConn() {
	p.mu.Lock()
	conn := p.conn
	p.conn = nil
	p.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// requestAction 通过当前 WebSocket 发起带 echo 的动作，并等待读循环分发响应。
// 写入、注册等待者和连接检查放在同一把锁内，避免响应先到导致竞态丢失。
func (p *oneBotPlatform) requestAction(ctx context.Context, conn *websocket.Conn, action map[string]any, echo string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if conn == nil {
		return nil, ErrNotRunning
	}
	waiter := oneBotActionWaiter{conn: conn, result: make(chan oneBotActionResult, 1)}
	p.mu.Lock()
	if p.conn != conn {
		p.mu.Unlock()
		return nil, ErrNotRunning
	}
	if p.actionWaiters == nil {
		p.actionWaiters = make(map[string]oneBotActionWaiter)
	}
	p.actionWaiters[echo] = waiter
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	writeErr := conn.WriteJSON(action)
	_ = conn.SetWriteDeadline(time.Time{})
	p.mu.Unlock()
	if writeErr != nil {
		p.removeActionWaiter(conn, echo)
		return nil, fmt.Errorf("OneBot 动作请求发送失败: %w", writeErr)
	}

	select {
	case result := <-waiter.result:
		return result.payload, result.err
	case <-ctx.Done():
		p.removeActionWaiter(conn, echo)
		return nil, ctx.Err()
	}
}

// deliverActionResponse 将读循环收到的动作响应按 echo 投递给等待者。
func (p *oneBotPlatform) deliverActionResponse(conn *websocket.Conn, echo string, payload []byte) bool {
	p.mu.Lock()
	waiter, ok := p.actionWaiters[echo]
	if !ok || waiter.conn != conn {
		p.mu.Unlock()
		return false
	}
	delete(p.actionWaiters, echo)
	p.mu.Unlock()
	waiter.result <- oneBotActionResult{payload: append([]byte(nil), payload...)}
	return true
}

// removeActionWaiter 清理超时或写入失败的动作请求，避免连接重用后误配响应。
func (p *oneBotPlatform) removeActionWaiter(conn *websocket.Conn, echo string) {
	p.mu.Lock()
	if waiter, ok := p.actionWaiters[echo]; ok && waiter.conn == conn {
		delete(p.actionWaiters, echo)
	}
	p.mu.Unlock()
}

// failActionWaiters 在连接关闭时让对应文件请求立即失败；其他连接的请求不受影响。
func (p *oneBotPlatform) failActionWaiters(conn *websocket.Conn, connectionErr error) {
	if connectionErr == nil {
		connectionErr = ErrNotRunning
	}
	p.mu.Lock()
	waiters := make([]oneBotActionWaiter, 0)
	for echo, waiter := range p.actionWaiters {
		if waiter.conn != conn {
			continue
		}
		delete(p.actionWaiters, echo)
		waiters = append(waiters, waiter)
	}
	p.mu.Unlock()
	for _, waiter := range waiters {
		waiter.result <- oneBotActionResult{err: connectionErr}
	}
}

// oneBotResponseEcho 提取动作响应的 echo；普通事件没有 echo，因此会返回空字符串。
func oneBotResponseEcho(payload []byte) string {
	var response struct {
		Echo json.RawMessage `json:"echo"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return ""
	}
	return rawID(response.Echo)
}

// getOneBotFile 调用 OneBot get_file，把事件中的 file_id 解析成实际文件来源。
func (p *oneBotPlatform) getOneBotFile(ctx context.Context, conn *websocket.Conn, fileID string) (oneBotFileResult, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return oneBotFileResult{}, errors.New("OneBot file_id 为空")
	}
	echo := fmt.Sprintf("abot-get-file-%d", time.Now().UnixNano())
	action := map[string]any{
		"action": "get_file",
		"params": map[string]any{"file_id": fileID},
		"echo":   echo,
	}
	// 记录完整动作参数，便于核对 NapCat 是否收到文件获取请求。
	slog.Info("OneBot WebSocket 请求", "adapter_id", p.bot.ID, "action", action)
	payload, err := p.requestAction(ctx, conn, action, echo)
	if err != nil {
		return oneBotFileResult{}, err
	}
	var response oneBotActionResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return oneBotFileResult{}, fmt.Errorf("OneBot get_file 响应解析失败: %w", err)
	}
	// 保留平台响应原文，方便定位 file_id 失效、权限不足和 NapCat 返回格式差异。
	slog.Info("OneBot get_file 响应", "adapter_id", p.bot.ID, "file_id", fileID, "payload", string(payload))
	if response.RetCode != 0 || strings.EqualFold(strings.TrimSpace(response.Status), "failed") {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = strings.TrimSpace(response.Wording)
		}
		if message == "" {
			message = "平台未返回具体原因"
		}
		return oneBotFileResult{}, fmt.Errorf("OneBot get_file 失败（retcode=%d）: %s", response.RetCode, message)
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return oneBotFileResult{}, errors.New("OneBot get_file 未返回文件数据")
	}
	var result oneBotFileResult
	if err := json.Unmarshal(response.Data, &result); err != nil {
		return oneBotFileResult{}, fmt.Errorf("OneBot get_file 文件数据解析失败: %w", err)
	}
	if strings.TrimSpace(result.File) == "" && strings.TrimSpace(result.URL) == "" && strings.TrimSpace(result.Base64) == "" {
		return oneBotFileResult{}, errors.New("OneBot get_file 未返回文件路径、下载地址或 base64 数据")
	}
	return result, nil
}

func (p *oneBotPlatform) messageFromEvent(ctx context.Context, event oneBotEvent) (Message, error) {
	return p.messageFromEventOnConn(ctx, p.currentConn(), event)
}

// currentConn 读取当前连接，兼容直接调用 messageFromEvent 的旧测试和辅助代码。
func (p *oneBotPlatform) currentConn() *websocket.Conn {
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	return conn
}

// messageFromEventOnConn 绑定产生该事件的连接，避免重连时把 file_id 请求发到新连接。
func (p *oneBotPlatform) messageFromEventOnConn(ctx context.Context, conn *websocket.Conn, event oneBotEvent) (Message, error) {
	userID := rawID(event.UserID)
	chatType := strings.TrimSpace(event.MessageType)
	chatID := userID
	if chatType == "group" {
		chatID = rawID(event.GroupID)
	}
	if chatID == "" {
		return Message{}, errors.New("OneBot 消息目标为空")
	}
	var resolver oneBotAttachmentResolver
	if conn != nil {
		resolver = func(resolveCtx context.Context, fileID string) (oneBotFileResult, error) {
			return p.getOneBotFile(resolveCtx, conn, fileID)
		}
	}
	// 每个 OneBot 实例可以连接不同的 NapCat 容器，因此附件路径映射必须读取
	// 当前机器人自己的配置，不能再把宿主机目录做成全局设置。
	text, attachments := parseOneBotMessageWithResolverAndRoot(ctx, p.client, resolver, strings.TrimSpace(p.bot.OneBotFileRoot), event.Message)
	if strings.TrimSpace(text) == "" && len(attachments) == 0 && !p.oneBotMessageMentioned(event) {
		return Message{}, errors.New("OneBot 消息不包含文本或支持的附件")
	}
	return Message{
		ID:             rawID(event.MessageID),
		Platform:       TypeOneBot11,
		UserID:         userID,
		ChatID:         chatID,
		ChatType:       chatType,
		AutoName:       oneBotMessageAutoName(event, chatID, userID),
		Text:           strings.TrimSpace(text),
		Attachments:    attachments,
		Mentioned:      !isGroupChat(chatType) || p.oneBotMessageMentioned(event),
		ReplyMessageID: rawID(event.MessageID),
		IsSelf:         userID != "" && userID == rawID(event.SelfID),
		AtAll:          oneBotAtAll(event.Message),
	}, nil
}

// oneBotAtAll 只识别结构化 @全体 消息，避免普通文字误触发过滤。
func oneBotAtAll(raw json.RawMessage) bool {
	var segments []oneBotSegment
	if json.Unmarshal(raw, &segments) != nil {
		return false
	}
	for _, segment := range segments {
		if segment.Type == "at" && strings.EqualFold(stringValue(segment.Data["qq"]), "all") {
			return true
		}
	}
	return false
}

// oneBotMessageAutoName 将群名缺失时可获得的群号和用户昵称组合成可读名称；
// 该名称只用于来源目录展示，不能替代稳定的 UMO 来源键。
func oneBotMessageAutoName(event oneBotEvent, chatID, userID string) string {
	name := ""
	if event.Sender != nil {
		name = strings.TrimSpace(event.Sender.Card)
		if name == "" {
			name = strings.TrimSpace(event.Sender.Nickname)
		}
	}
	if strings.EqualFold(strings.TrimSpace(event.MessageType), "group") {
		if name != "" {
			return "群聊 " + chatID + " · " + name
		}
		return "群聊 " + chatID
	}
	if name != "" {
		return name
	}
	if userID != "" {
		return "用户 " + userID
	}
	return ""
}

func (p *oneBotPlatform) oneBotMessageMentioned(event oneBotEvent) bool {
	p.mu.Lock()
	selfID := p.selfID
	p.mu.Unlock()
	if selfID == "" {
		return false
	}
	var segments []oneBotSegment
	if json.Unmarshal(event.Message, &segments) != nil {
		return false
	}
	for _, segment := range segments {
		if segment.Type != "at" {
			continue
		}
		if value := stringValue(segment.Data["qq"]); value != "" && value == selfID {
			return true
		}
	}
	return false
}

func parseOneBotMessage(ctx context.Context, client *http.Client, raw json.RawMessage) (string, []agent.Attachment) {
	return parseOneBotMessageWithResolver(ctx, client, nil, raw)
}

// parseOneBotMessageWithResolver 解析文本、图片、语音和文件消息；file_id 由
// resolver 交给 OneBot get_file 解析，已有 URL/base64 消息仍沿用原有路径。
func parseOneBotMessageWithResolver(ctx context.Context, client *http.Client, resolver oneBotAttachmentResolver, raw json.RawMessage) (string, []agent.Attachment) {
	return parseOneBotMessageWithResolverAndRoot(ctx, client, resolver, "", raw)
}

// parseOneBotMessageWithResolverAndRoot 在保留旧解析入口的基础上注入当前机器人的
// 附件根目录，确保 file_id 返回的容器路径能按实例配置映射到宿主机。
func parseOneBotMessageWithResolverAndRoot(ctx context.Context, client *http.Client, resolver oneBotAttachmentResolver, localFileRoot string, raw json.RawMessage) (string, []agent.Attachment) {
	var text string
	var segments []oneBotSegment
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	if json.Unmarshal(raw, &segments) != nil {
		return "", nil
	}
	var attachments []agent.Attachment
	var totalBytes int64
	for _, segment := range segments {
		switch segment.Type {
		case "text":
			text += stringValue(segment.Data["text"])
		case "image", "file", "record":
			if len(attachments) >= 5 {
				continue
			}
			if attachment, ok := oneBotAttachmentWithResolverAndRoot(ctx, client, resolver, localFileRoot, segment); ok {
				if totalBytes+int64(len(attachment.Data)) > platformAttachmentSum {
					continue
				}
				attachments = append(attachments, attachment)
				totalBytes += int64(len(attachment.Data))
			}
		}
	}
	return text, attachments
}

func oneBotAttachment(ctx context.Context, client *http.Client, segment oneBotSegment) (agent.Attachment, bool) {
	return oneBotAttachmentWithResolver(ctx, client, nil, segment)
}

// oneBotAttachmentWithResolver 将 OneBot 消息段转换为内存附件，并在只有
// file_id 时先请求平台返回真实文件；所有失败都记录原因，避免再次静默丢弃。
func oneBotAttachmentWithResolver(ctx context.Context, client *http.Client, resolver oneBotAttachmentResolver, segment oneBotSegment) (agent.Attachment, bool) {
	return oneBotAttachmentWithResolverAndRoot(ctx, client, resolver, "", segment)
}

// oneBotAttachmentWithResolverAndRoot 解析当前机器人收到的附件，并把实例级路径
// 配置继续传到本地文件读取层；旧入口通过空路径保留原有行为。
func oneBotAttachmentWithResolverAndRoot(ctx context.Context, client *http.Client, resolver oneBotAttachmentResolver, localFileRoot string, segment oneBotSegment) (agent.Attachment, bool) {
	name := cleanFileName(stringValue(segment.Data["file"]))
	if name == "" {
		name = "onebot-attachment"
		if segment.Type == "record" {
			name = "onebot-record"
		}
	}
	mimeType := normalizeFileMIME(name, stringValue(segment.Data["type"]))
	if segment.Type == "image" && mimeType == "application/octet-stream" {
		mimeType = "image/jpeg"
	}
	if segment.Type == "record" && mimeType == "application/octet-stream" {
		mimeType = "audio/ogg"
	}
	fileID := strings.TrimSpace(stringValue(segment.Data["file_id"]))
	var resolved *oneBotFileResult
	if fileID != "" {
		if resolver == nil {
			slog.Warn("OneBot 文件附件缺少 get_file 解析器", "file_id", fileID, "name", name)
			return agent.Attachment{}, false
		}
		fileResult, resolveErr := resolver(ctx, fileID)
		if resolveErr != nil {
			slog.Warn("OneBot 文件附件获取失败", "file_id", fileID, "name", name, "error", resolveErr)
			return agent.Attachment{}, false
		}
		resolved = &fileResult
		// 某些实现把 file 字段填成内部 ID，优先使用 get_file 返回的真实文件名。
		resolvedName := cleanFileName(fileResult.FileName)
		if resolvedName == "" {
			resolvedName = cleanFileName(fileResult.File)
		}
		if (name == "" || name == "onebot-attachment" || (filepath.Ext(name) == "" && filepath.Ext(resolvedName) != "")) && resolvedName != "" {
			name = resolvedName
			mimeType = normalizeFileMIME(name, stringValue(segment.Data["type"]))
		}
		if mimeType == "application/octet-stream" {
			mimeType = normalizeFileMIME(name, fileResult.MIMEType)
		}
	}
	value := stringValue(segment.Data["url"])
	allowLocalPath := false
	if resolved != nil {
		allowLocalPath = true
		// get_file 的来源优先级是 base64、URL、平台返回的本地路径。
		if strings.TrimSpace(resolved.Base64) != "" {
			value = strings.TrimSpace(resolved.Base64)
			if !strings.HasPrefix(value, "base64://") && !strings.HasPrefix(value, "data:") {
				value = "base64://" + value
			}
		} else if strings.TrimSpace(resolved.URL) != "" {
			value = resolved.URL
		} else {
			value = resolved.File
		}
	} else if value == "" {
		value = stringValue(segment.Data["file"])
	}
	data, resolvedMIME, err := readOneBotAttachmentValueWithRoot(ctx, client, value, mimeType, allowLocalPath, localFileRoot)
	if resolvedMIME != "" {
		mimeType = resolvedMIME
	}
	if err != nil || int64(len(data)) > platformAttachmentMax || len(data) == 0 {
		if err == nil {
			err = errors.New("附件没有可读取的数据")
		}
		slog.Warn("OneBot 附件解析失败", "type", segment.Type, "file_id", fileID, "name", name, "source", value, "error", err)
		return agent.Attachment{}, false
	}
	return agent.Attachment{Name: name, MIMEType: mimeType, Data: data}, true
}

// readOneBotAttachmentValue 统一处理 base64、Data URL、HTTP URL 和 get_file
// 返回的本地路径；allowLocalPath 只对平台明确返回的文件来源开放。
func readOneBotAttachmentValue(ctx context.Context, client *http.Client, value, fallbackMIME string, allowLocalPath bool) ([]byte, string, error) {
	return readOneBotAttachmentValueWithRoot(ctx, client, value, fallbackMIME, allowLocalPath, "")
}

// readOneBotAttachmentValueWithRoot 统一处理附件来源，并将机器人配置的宿主机根
// 目录交给本地路径映射逻辑；网络地址和 base64 不受该配置影响。
func readOneBotAttachmentValueWithRoot(ctx context.Context, client *http.Client, value, fallbackMIME string, allowLocalPath bool, localFileRoot string) ([]byte, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fallbackMIME, errors.New("附件来源为空")
	}
	if strings.HasPrefix(value, "base64://") {
		data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "base64://"))
		return data, fallbackMIME, err
	}
	if strings.HasPrefix(value, "data:") {
		data, mimeType, err := decodeDataURL(value, fallbackMIME)
		return data, mimeType, err
	}
	if parsed, parseErr := url.Parse(value); parseErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		data, err := downloadHTTPAttachment(ctx, client, value)
		return data, fallbackMIME, err
	}
	if !allowLocalPath {
		return nil, fallbackMIME, errors.New("附件缺少可下载地址")
	}
	// NapCat 常在同一台机器返回本地路径；file:// URL 先转换为普通路径再读取。
	if parsed, parseErr := url.Parse(value); parseErr == nil && parsed.Scheme == "file" {
		value = parsed.Path
	}
	data, err := readOneBotLocalFileWithRoot(value, localFileRoot)
	return data, fallbackMIME, err
}

// readOneBotLocalFile 受平台附件上限约束读取 get_file 返回的本地文件，避免
// 错误路径或异常文件导致进程一次性分配不受控内存。
func readOneBotLocalFile(name string) ([]byte, error) {
	return readOneBotLocalFileWithRoot(name, "")
}

// readOneBotLocalFileWithRoot 只读取受控的 OneBot 返回路径，并优先使用当前
// 机器人配置的宿主机目录，避免不同机器人之间共享错误的全局映射。
func readOneBotLocalFileWithRoot(name, localFileRoot string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("本地附件路径为空")
	}
	// NapCat 容器把宿主机目录挂载为 /app/.config/QQ；当 Abot 在宿主机运行时，
	// 先把这个受限容器前缀映射到用户目录下的 ntqq，避免把任意路径暴露给读取层。
	candidates := oneBotLocalFileCandidatesWithRoot(name, localFileRoot)
	var file *os.File
	var openErr error
	var resolvedName string
	for _, candidate := range candidates {
		file, openErr = os.Open(candidate)
		if openErr == nil {
			resolvedName = candidate
			break
		}
	}
	if file == nil {
		return nil, fmt.Errorf("打开本地附件失败: %w", openErr)
	}
	if resolvedName != name {
		slog.Info("OneBot 本地附件路径已映射", "source", name, "path", resolvedName)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, platformAttachmentMax+1))
	if err != nil {
		return nil, fmt.Errorf("读取本地附件失败: %w", err)
	}
	if int64(len(data)) > platformAttachmentMax {
		return nil, fmt.Errorf("本地附件超过 %d MB", platformAttachmentMax>>20)
	}
	return data, nil
}

// oneBotLocalFileCandidates 只为 NapCat 的已知容器路径生成候选宿主机路径；
// 环境变量优先，默认值适配常见的 ~/napcat/ntqq 挂载目录。
func oneBotLocalFileCandidates(name string) []string {
	return oneBotLocalFileCandidatesWithRoot(name, "")
}

// oneBotLocalFileCandidatesWithRoot 仅为已知的 NapCat 容器前缀生成宿主机候选
// 路径；实例配置优先，环境变量和默认目录只用于兼容未迁移的旧配置。
func oneBotLocalFileCandidatesWithRoot(name, localFileRoot string) []string {
	candidates := []string{name}
	const containerRoot = "/app/.config/QQ"
	prefix := containerRoot + "/"
	if !strings.HasPrefix(name, prefix) {
		return candidates
	}
	relative := strings.TrimPrefix(name, prefix)
	roots := make([]string, 0, 3)
	if root := strings.TrimSpace(localFileRoot); root != "" {
		roots = append(roots, root)
	}
	if root := strings.TrimSpace(os.Getenv("ABOT_ONEBOT_FILE_ROOT")); root != "" {
		roots = append(roots, root)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		roots = append(roots, filepath.Join(home, "napcat", "ntqq"))
	}
	for _, root := range roots {
		candidate := filepath.Join(root, filepath.FromSlash(relative))
		if candidate == name {
			continue
		}
		alreadyAdded := false
		for _, item := range candidates {
			if item == candidate {
				alreadyAdded = true
				break
			}
		}
		if !alreadyAdded {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func downloadHTTPAttachment(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("附件下载返回 HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, platformAttachmentMax+1))
}

func decodeDataURL(raw, fallbackMIME string) ([]byte, string, error) {
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return nil, fallbackMIME, errors.New("Data URL 格式无效")
	}
	meta := strings.TrimPrefix(raw[:comma], "data:")
	value := raw[comma+1:]
	mimeType := fallbackMIME
	if index := strings.IndexByte(meta, ';'); index >= 0 {
		if meta[:index] != "" {
			mimeType = meta[:index]
		}
		meta = meta[index+1:]
	} else if meta != "" {
		mimeType = meta
	}
	if meta != "base64" && !strings.HasSuffix(meta, ";base64") {
		return nil, mimeType, errors.New("只支持 base64 Data URL")
	}
	data, err := base64.StdEncoding.DecodeString(value)
	return data, mimeType, err
}

func rawID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value json.Number
	if json.Unmarshal(raw, &value) == nil {
		return value.String()
	}
	var stringValue string
	if json.Unmarshal(raw, &stringValue) == nil {
		return strings.TrimSpace(stringValue)
	}
	return ""
}

func stringValue(value any) string {
	switch item := value.(type) {
	case string:
		return item
	case json.Number:
		return item.String()
	case float64:
		return strconv.FormatInt(int64(item), 10)
	default:
		return ""
	}
}

func oneBotIDValue(value string) any {
	if number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		return number
	}
	return strings.TrimSpace(value)
}
