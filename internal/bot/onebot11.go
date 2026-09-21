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

	mu          sync.Mutex
	conn        *websocket.Conn
	selfID      string
	server      *http.Server
	listener    net.Listener
	connections chan *websocket.Conn
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

// newOneBotPlatform 创建 OneBot v11 适配器；默认使用 NapCat 需要的反向 WS 服务端。
func newOneBotPlatform(item Bot) (*oneBotPlatform, error) {
	if err := item.Validate(); err != nil {
		return nil, err
	}
	return &oneBotPlatform{
		bot: item, client: &http.Client{Timeout: 30 * time.Second},
		dialer:   &websocket.Dialer{HandshakeTimeout: 10 * time.Second},
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
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
func (p *oneBotPlatform) consumeConnection(ctx context.Context, conn *websocket.Conn, handler Handler) error {
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
		message, err := p.messageFromEvent(ctx, event)
		if err != nil {
			continue
		}
		if handlerErr := handler(ctx, message); handlerErr != nil {
			continue
		}
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

func (p *oneBotPlatform) messageFromEvent(ctx context.Context, event oneBotEvent) (Message, error) {
	userID := rawID(event.UserID)
	chatType := strings.TrimSpace(event.MessageType)
	chatID := userID
	if chatType == "group" {
		chatID = rawID(event.GroupID)
	}
	if chatID == "" {
		return Message{}, errors.New("OneBot 消息目标为空")
	}
	text, attachments := parseOneBotMessage(ctx, p.client, event.Message)
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
			if attachment, ok := oneBotAttachment(ctx, client, segment); ok {
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
	value := stringValue(segment.Data["url"])
	if value == "" {
		value = stringValue(segment.Data["file"])
	}
	var data []byte
	var err error
	if strings.HasPrefix(value, "base64://") {
		data, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "base64://"))
	} else if strings.HasPrefix(value, "data:") {
		data, mimeType, err = decodeDataURL(value, mimeType)
	} else if parsed, parseErr := url.Parse(value); parseErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		data, err = downloadHTTPAttachment(ctx, client, value)
	}
	if err != nil || int64(len(data)) > platformAttachmentMax || len(data) == 0 {
		return agent.Attachment{}, false
	}
	return agent.Attachment{Name: name, MIMEType: mimeType, Data: data}, true
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
