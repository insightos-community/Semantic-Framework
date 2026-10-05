package ws

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/log"
)

// testEnv 聚合端到端测试所需的网关、Hub 与可用 token。
type testEnv struct {
	hub    *Hub
	server *httptest.Server
	wsBase string
	token  string
}

// newTestEnv 搭建真实 HTTP 服务（httptest）承载 WS 网关，并登录获得 token。
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	st := storetest.OpenMigrated(t, logger)

	svc := auth.NewService(st, logger)
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "s3cret")
	if err := svc.SeedAdmin(); err != nil {
		t.Fatalf("SeedAdmin 失败: %v", err)
	}
	token, err := svc.Login("admin", "s3cret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	hub := NewHub(logger)
	mux := http.NewServeMux()
	mux.Handle("/ws/agent-events", NewGateway(hub, svc, nil, logger))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &testEnv{
		hub:    hub,
		server: server,
		wsBase: "ws" + strings.TrimPrefix(server.URL, "http"),
		token:  token,
	}
}

// dial 建立一条已鉴权的 WS 连接，sessionID 为空表示只收广播。
func (e *testEnv) dial(t *testing.T, sessionID string) *websocket.Conn {
	t.Helper()
	url := e.wsBase + "/ws/agent-events?token=" + e.token
	if sessionID != "" {
		url += "&session_id=" + sessionID
	}
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("WS 连接失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// readEnvelope 在超时内读取一条下行事件并反序列化。
func readEnvelope(t *testing.T, c *websocket.Conn, timeout time.Duration) Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("读取下行事件失败: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("下行消息应为合法 envelope JSON: %v（data: %s）", err, data)
	}
	return env
}

// TestHubSessionDelivery 验证按 session_id 投递：只有订阅该会话的连接收到事件。
func TestHubSessionDelivery(t *testing.T) {
	env := newTestEnv(t)
	c1 := env.dial(t, "s1")
	c2 := env.dial(t, "s2")

	// 等待两条连接完成注册，避免 Publish 时连接尚未入表。
	waitConnCount(t, env.hub, 2)

	env.hub.Publish(NewEnvelope("s1", ChannelDialogue, "agent.message", ImportanceNormal,
		map[string]string{"text": "hi"}))

	got := readEnvelope(t, c1, 2*time.Second)
	if got.SessionID != "s1" || got.Channel != ChannelDialogue || got.Type != "agent.message" {
		t.Errorf("c1 收到的事件不符: %+v", got)
	}
	if got.ID == "" || got.Ts.IsZero() {
		t.Errorf("envelope 应补全 id 与 ts: %+v", got)
	}

	// c2 订阅的是 s2，不应收到 s1 的事件。
	assertNoMessage(t, c2)
}

// TestHubBroadcast 验证空 session_id 广播：全部在线连接都收到事件。
func TestHubBroadcast(t *testing.T) {
	env := newTestEnv(t)
	c1 := env.dial(t, "s1")
	c2 := env.dial(t, "") // 未订阅会话，只收广播

	waitConnCount(t, env.hub, 2)

	env.hub.Publish(NewEnvelope("", ChannelAlert, "system.alert", ImportanceCritical, nil))

	for i, c := range []*websocket.Conn{c1, c2} {
		got := readEnvelope(t, c, 2*time.Second)
		if got.Channel != ChannelAlert || got.Importance != ImportanceCritical {
			t.Errorf("连接 %d 收到的广播事件不符: %+v", i, got)
		}
	}
}

// TestHubUnsubscribe 验证连接断开后从 Hub 注销，不再占用订阅表。
func TestHubUnsubscribe(t *testing.T) {
	env := newTestEnv(t)
	c := env.dial(t, "s1")
	waitConnCount(t, env.hub, 1)

	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("客户端关闭连接失败: %v", err)
	}
	waitConnCount(t, env.hub, 0)

	// 注销后投递不应 panic 也不应有接收方。
	env.hub.Publish(NewEnvelope("s1", ChannelDialogue, "agent.message", ImportanceNormal, nil))
}

// TestGatewayAuthReject 验证无 token 的连接在升级前被拒绝（401）。
func TestGatewayAuthReject(t *testing.T) {
	env := newTestEnv(t)
	_, resp, err := websocket.Dial(context.Background(), env.wsBase+"/ws/agent-events", nil)
	if err == nil {
		t.Fatal("无 token 连接应被拒绝了")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("拒绝应返回 401，实际: %+v", resp)
	}
}

// TestGatewayUnknownUpstreamMessage 验证未知上行类型收到 WS_UNKNOWN_TYPE
// 错误应答，且连接保持存活（下行事件照常到达）。
func TestGatewayUnknownUpstreamMessage(t *testing.T) {
	env := newTestEnv(t)
	c := env.dial(t, "s1")
	waitConnCount(t, env.hub, 1)

	if err := c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"unknown.garbage"}`)); err != nil {
		t.Fatalf("上行写入失败: %v", err)
	}

	// 显式错误应答：调用方能立刻发现协议用错。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("读取错误应答失败: %v", err)
	}
	var reply struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatalf("错误应答应为 JSON: %v（data: %s）", err, data)
	}
	if reply.Type != "error" || reply.Code != CodeWSUnknownType {
		t.Errorf("应回复 WS_UNKNOWN_TYPE，实际: %+v", reply)
	}

	// 连接仍存活：下行事件照常到达。
	env.hub.Publish(NewEnvelope("s1", ChannelTrace, "tool.completed", ImportanceNormal, nil))
	readEnvelope(t, c, 2*time.Second)
}

// waitConnCount 轮询等待 Hub 在线连接数达到期望值。
func waitConnCount(t *testing.T, hub *Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ConnCount() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Hub 连接数未在 2s 内变为 %d，实际: %d", want, hub.ConnCount())
}

// assertNoMessage 断言连接在一小段时间内没有任何下行消息。
// coder/websocket 在 Read 的 ctx 到期时会关闭整条连接，因此这里用
// Background Read + 等待，超时后不 cancel，避免误杀连接。
func assertNoMessage(t *testing.T, c *websocket.Conn) {
	t.Helper()
	got := make(chan []byte, 1)
	go func() {
		_, data, err := c.Read(context.Background())
		if err == nil {
			select {
			case got <- data:
			default:
			}
		}
	}()
	select {
	case data := <-got:
		t.Errorf("不应收到下行消息，实际: %s", data)
	case <-time.After(200 * time.Millisecond):
	}
}
