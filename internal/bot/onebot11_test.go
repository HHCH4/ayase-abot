package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOneBotReverseWebSocketReceivesAndSends(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	actionReceived := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("升级 OneBot WebSocket 失败: %v", err)
			return
		}
		defer conn.Close()
		message := map[string]any{
			"post_type": "message", "message_type": "group", "user_id": 1001, "group_id": 2002,
			"message": []map[string]any{{"type": "text", "data": map[string]any{"text": "你好"}}},
		}
		if err := conn.WriteJSON(message); err != nil {
			t.Errorf("写入 OneBot 测试消息失败: %v", err)
			return
		}
		var action map[string]any
		if err := conn.ReadJSON(&action); err == nil {
			actionReceived <- action
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint := strings.Replace(server.URL, "http://", "ws://", 1)
	platform := &oneBotPlatform{
		bot:    Bot{ID: "qq", Name: "QQ", Type: TypeOneBot11, OneBotMode: OneBotModeClient, Endpoint: endpoint},
		dialer: &websocket.Dialer{HandshakeTimeout: 3 * time.Second}, client: server.Client(),
	}
	messageReceived := make(chan Message, 1)
	runDone := make(chan error, 1)
	go func() {
		runDone <- platform.Run(ctx, func(handlerCtx context.Context, message Message) error {
			messageReceived <- message
			return platform.Send(handlerCtx, message, "收到")
		})
	}()

	select {
	case message := <-messageReceived:
		if message.ChatID != "2002" || message.UserID != "1001" || message.Text != "你好" || message.ChatType != "group" {
			t.Fatalf("OneBot 入站消息解析错误: %#v", message)
		}
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("等待 OneBot 入站消息超时")
	}
	select {
	case action := <-actionReceived:
		params, ok := action["params"].(map[string]any)
		if !ok || action["action"] != "send_msg" || params["message_type"] != "group" || params["message"] != "收到" {
			raw, _ := json.Marshal(action)
			t.Fatalf("OneBot 出站动作错误: %s", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待 OneBot 出站动作超时")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("OneBot 读取循环退出错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待 OneBot 读取循环退出超时")
	}
}

func TestOneBotReverseWebSocketServer(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("获取测试端口失败: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatalf("释放测试端口失败: %v", err)
	}

	platform, err := newOneBotPlatform(Bot{
		ID: "qq-server", Name: "QQ 服务端", Type: TypeOneBot11,
		OneBotMode: OneBotModeReverseServer, ListenHost: "127.0.0.1", ListenPort: port, ListenPath: "/ws",
	})
	if err != nil {
		t.Fatalf("创建反向 WS 服务端失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messageReceived := make(chan Message, 1)
	runDone := make(chan error, 1)
	go func() {
		runDone <- platform.Run(ctx, func(handlerCtx context.Context, message Message) error {
			messageReceived <- message
			return platform.Send(handlerCtx, message, "收到")
		})
	}()

	endpoint := fmt.Sprintf("ws://127.0.0.1:%d/ws", port)
	var conn *websocket.Conn
	var dialErr error
	for attempt := 0; attempt < 30; attempt++ {
		conn, _, dialErr = (&websocket.Dialer{HandshakeTimeout: time.Second}).Dial(endpoint, nil)
		if dialErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("NapCat 客户端连接反向 WS 服务端失败: %v", dialErr)
	}
	defer conn.Close()

	event := map[string]any{
		"post_type": "message", "message_type": "private", "user_id": 1001,
		"message": []map[string]any{{"type": "text", "data": map[string]any{"text": "你好"}}},
	}
	if err := conn.WriteJSON(event); err != nil {
		t.Fatalf("写入 OneBot 事件失败: %v", err)
	}
	select {
	case message := <-messageReceived:
		if message.ChatID != "1001" || message.UserID != "1001" || message.Text != "你好" || message.ChatType != "private" {
			t.Fatalf("反向 WS 入站消息解析错误: %#v", message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待反向 WS 入站消息超时")
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("设置 WS 读取超时失败: %v", err)
	}
	var action map[string]any
	if err := conn.ReadJSON(&action); err != nil {
		t.Fatalf("读取反向 WS 出站动作失败: %v", err)
	}
	params, ok := action["params"].(map[string]any)
	if !ok || action["action"] != "send_msg" || params["message_type"] != "private" || params["message"] != "收到" {
		raw, _ := json.Marshal(action)
		t.Fatalf("反向 WS 出站动作错误: %s", raw)
	}
	cancel()
	_ = platform.Close()
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Fatalf("反向 WS 服务端退出错误: %v", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待反向 WS 服务端退出超时")
	}
}
