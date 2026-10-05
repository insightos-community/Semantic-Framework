// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/pkg/log"
)

const (
	// pingInterval 是心跳 ping 的发送间隔。
	pingInterval = 30 * time.Second

	// pongTimeout 是等待 pong 的最长时间，超时判定连接死亡并断开。
	pongTimeout = 90 * time.Second

	// writeTimeout 是单次下行写操作的最长时间。
	writeTimeout = 10 * time.Second

	// sendBuffer 是每连接下行缓冲的容量。
	sendBuffer = 64

	// readLimit 是上行消息的最大字节数，超限连接被关闭（防内存滥用）。
	readLimit = 1 << 20

	// bearerSubProtocolPrefix 是 Sec-WebSocket-Protocol 携带 token 的约定前缀：
	// 浏览器 WebSocket 无法自定义请求头，token 以 "bearer.<token>" 形式
	// 放进子协议字段，握手时服务端原样回选该子协议。
	bearerSubProtocolPrefix = "bearer."
)

// Gateway 是 /ws/agent-events 的接入处理器：升级连接、鉴权、注册到 Hub，
// 并为每条连接拉起读泵/写泵/心跳三个 goroutine。
type Gateway struct {
	// hub 连接中枢，负责按会话投递事件。
	hub *Hub

	// auth 认证服务，校验连接携带的 token。
	auth *auth.Service

	// syncer 断连续传补发器；nil 时 sync 上行回错误（服务未装配）。
	syncer EventReplayer

	// logger 结构化日志器。
	logger *log.Logger
}

// NewGateway 创建 WS 网关。
func NewGateway(hub *Hub, authSvc *auth.Service, syncer EventReplayer, logger *log.Logger) *Gateway {
	return &Gateway{hub: hub, auth: authSvc, syncer: syncer, logger: logger}
}

// ServeHTTP 处理 GET /ws/agent-events：鉴权 → 升级 → 注册 → 泵循环。
// token 来源优先级：query 参数 token= > Sec-WebSocket-Protocol（bearer. 前缀）。
// query 参数 session_id 声明订阅的会话（可空，空则只收广播）。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, echoProtocol := extractToken(r)
	userID, err := g.auth.ValidateToken(token)
	if err != nil {
		var aerr *auth.Error
		if errors.As(err, &aerr) {
			writeError(w, http.StatusUnauthorized, aerr.Code, aerr.Message)
			return
		}
		g.logger.WithError(err).Error("WS 鉴权内部错误")
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
		return
	}

	acceptOpts := &websocket.AcceptOptions{}
	if echoProtocol != "" {
		// 回选客户端声明的子协议，否则浏览器侧握手失败。
		acceptOpts.Subprotocols = []string{echoProtocol}
	}
	ws, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		g.logger.WithError(err).Warn("WS 升级失败", "remote", r.RemoteAddr)
		return
	}
	ws.SetReadLimit(readLimit)

	sessionID := r.URL.Query().Get("session_id")
	c := newConn(ws, userID, g.logger)
	g.hub.Subscribe(sessionID, c)
	g.logger.Info("WS 连接已建立",
		"conn_id", c.id, "user_id", userID, "session_id", sessionID, "remote", r.RemoteAddr)
	defer func() {
		c.close()
		g.hub.Unsubscribe(sessionID, c)
		g.logger.Info("WS 连接已断开", "conn_id", c.id, "user_id", userID, "session_id", sessionID)
	}()

	go c.writePump()
	go c.heartbeat()
	c.readPumpWith(func(data []byte) {
		g.handleUplink(c, sessionID, data)
	})
}

// handleUplink 处理 /ws/agent-events 的一条上行消息：本通道是下行通道，
// 上行仅支持 sync（断连续传）；其余类型回 WS_UNKNOWN_TYPE 错误应答
// （显式拒绝优于静默丢弃——调用方能立刻发现协议用错）。
func (g *Gateway) handleUplink(c *conn, sessionID string, data []byte) {
	var msg uplinkMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.reply(errorReply{Type: "error", Code: CodeWSBadMessage, Message: "消息不是合法 JSON"})
		return
	}
	if msg.Type == uplinkTypeSync {
		handleSync(c, g.syncer, sessionID, msg.LastEventID, g.logger)
		return
	}
	c.reply(errorReply{Type: "error", Code: CodeWSUnknownType,
		Message: "本通道仅支持 sync 上行，收到未知类型: " + msg.Type})
}

// extractToken 提取连接 token；经子协议携带时同时返回需回选的协议名。
func extractToken(r *http.Request) (token string, echoProtocol string) {
	if q := r.URL.Query().Get("token"); q != "" {
		return q, ""
	}
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, bearerSubProtocolPrefix) {
			return strings.TrimPrefix(p, bearerSubProtocolPrefix), p
		}
	}
	return "", ""
}

// writeError 以统一格式 {"error":{"code","message"}} 写出错误响应。
// 与 internal/server/http.WriteError 的线上格式保持一致；ws 不引用
// http 包是因为 http 路由装配依赖 handlers、handlers 依赖 runtime、
// runtime 依赖本包——引用即形成包循环（与 auth 包的先例一致）。
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

// conn 是一条已升级的 WS 连接，持有下行缓冲与生命周期信号。
type conn struct {
	// id 连接唯一标识，用于日志关联。
	id string

	// userID 连接所属用户。
	userID string

	// ws 底层 websocket 连接。
	ws *websocket.Conn

	// send 下行缓冲（Envelope 或上行错误应答），写泵唯一消费者。
	// 该通道从不 close（关闭语义由 done 表达），避免 Hub 投递与连接
	// 关闭竞争时发生 send-on-closed panic。
	send chan any

	// done 关闭信号：close 后读写泵与心跳全部退出。
	done chan struct{}

	// closeOnce 保证 close 只执行一次（读写泵、心跳都可能触发关闭）。
	closeOnce sync.Once

	// logger 结构化日志器。
	logger *log.Logger
}

// newConn 创建连接对象。
func newConn(ws *websocket.Conn, userID string, logger *log.Logger) *conn {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &conn{
		id:     "conn-" + hex.EncodeToString(b),
		userID: userID,
		ws:     ws,
		send:   make(chan any, sendBuffer),
		done:   make(chan struct{}),
		logger: logger,
	}
}

// close 关闭连接：置位 done 信号并关闭底层 websocket，幂等。
func (c *conn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
	})
}

// enqueue 将事件放入下行缓冲，背压策略按通道分级（架构 §3.4）：
// trace 事件可丢——缓冲满时丢弃并 DEBUG 记录；
// 其余事件不丢——阻塞等待缓冲位（连接关闭则放弃），
// 保证 dialogue/alert 等关键事件不因瞬时拥塞静默丢失。
func (c *conn) enqueue(env Envelope) {
	if env.Channel == ChannelTrace {
		select {
		case c.send <- env:
		case <-c.done:
		default:
			c.logger.Debug("下行缓冲已满，丢弃 trace 事件",
				"conn_id", c.id, "event_id", env.ID)
		}
		return
	}
	select {
	case c.send <- env:
	case <-c.done:
	}
}

// reply 将上行消息的直接应答（非 envelope 的协议应答）放入下行缓冲。
// 应答不可丢——阻塞等待缓冲位（连接关闭则放弃）。
func (c *conn) reply(msg any) {
	select {
	case c.send <- msg:
	case <-c.done:
	}
}

// writePump 是下行写泵：串行消费 send 缓冲并写入 websocket。
// 缓冲内是 Envelope（hub 投递）或上行错误应答（readPump 直接回复），
// 统一 json.Marshal 后写出。写失败（对端断开等）触发连接关闭。
func (c *conn) writePump() {
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			data, err := json.Marshal(msg)
			if err != nil {
				c.logger.WithError(err).Error("下行消息序列化失败，已跳过", "conn_id", c.id)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err = c.ws.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.logger.Debug("WS 写失败，关闭连接", "conn_id", c.id)
				c.close()
				return
			}
		}
	}
}

// readPumpWith 是携带上行处理器的读泵：持续读取以驱动协议层（pong 处理
// 依赖并发 Reader），每条上行消息交给 handle（/ws/chat 的对话上行与两个
// 通道的 sync 上行由此进入）。
func (c *conn) readPumpWith(handle func(data []byte)) {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			// 对端关闭、心跳超时触发 close、网络错误都会使 Read 返回错误。
			c.close()
			return
		}
		handle(data)
	}
}

// heartbeat 每 30s 发送一次 ping 并等待 pong（Ping 语义上等待 pong 返回），
// 90s 未收到 pong 判定连接死亡，主动断开并触发读写泵退出。
func (c *conn) heartbeat() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), pongTimeout)
		err := c.ws.Ping(ctx)
		cancel()
		if err != nil {
			c.logger.Warn("WS 心跳超时，断开连接", "conn_id", c.id, "user_id", c.userID)
			c.close()
			return
		}
	}
}
