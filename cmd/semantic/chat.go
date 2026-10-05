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

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// writeUplinkTimeout 是单条上行消息写操作的最长时间。
const writeUplinkTimeout = 10 * time.Second

// chatUI 持有 REPL 的共享状态：待应答审批（下行 goroutine 设置、
// REPL 主循环消费）与 stdout 写锁（两个 goroutine 都会打印）。
type chatUI struct {
	// mu 保护 pending* 字段。
	mu sync.Mutex

	// printMu 串行化 stdout 写入，避免流式输出与提示语交错成乱码。
	printMu sync.Mutex

	// pendingID 待应答的交互 ID；空表示无待答审批。
	pendingID string

	// pendingRisk 待答审批的风险等级（提示用）。
	pendingRisk string
}

// setPending 登记一个待应答审批。
func (u *chatUI) setPending(id, risk string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pendingID, u.pendingRisk = id, risk
}

// pending 返回待应答审批（ok=false 表示无）。
func (u *chatUI) pending() (id string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pendingID, u.pendingID != ""
}

// clearPending 清除待应答审批。
func (u *chatUI) clearPending() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pendingID, u.pendingRisk = "", ""
}

// printf 串行化打印（与 fmt.Printf 语义一致）。
func (u *chatUI) printf(format string, args ...any) {
	u.printMu.Lock()
	defer u.printMu.Unlock()
	fmt.Printf(format, args...)
}

// runChat 执行 semantic chat：解析会话（--session 或新建）→ 连接 /ws/chat
// → REPL（输入消息 → 流式打印回复；收到审批请求时提示 (y/n) 应答；
// /quit 退出）。
func runChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	server := serverFlag(fs)
	wsAddr := fs.String("ws", "", "WS 地址（默认由 server 推导：同主机端口 +1）")
	sessionID := fs.String("session", "", "会话 ID（缺省新建会话）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	creds, err := loadValidCredentials()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client, err := newClient(resolveServer(*server, creds), creds.Token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *wsAddr != "" {
		client.wsBase = *wsAddr
	}

	// 会话解析：显式 --session 直接使用；否则 REST 新建（标题由服务端
	// 取首条消息生成，这里先用默认标题占位）。
	sessID := *sessionID
	if sessID == "" {
		sess, err := client.createSession("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "创建会话失败:", err)
			return 1
		}
		sessID = sess.ID
		fmt.Printf("已创建会话 %s\n", sessID)
	}

	conn, err := client.dialChat(sessID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ui := &chatUI{}
	readDone := make(chan struct{})
	go readDownlinkLoop(conn, ui, readDone)
	fmt.Printf("已连接会话 %s（输入消息发送，/quit 退出）\n", sessID)

	code := repl(conn, ui, sessID)
	_ = conn.Close(websocket.StatusNormalClosure, "")
	<-readDone
	return code
}

// repl 是 REPL 主循环：读一行输入 → 按当前状态分发（待答审批时输入
// 解析为 y/n，否则作为对话消息发送）。
func repl(conn *websocket.Conn, ui *chatUI, sessionID string) int {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		if _, ok := ui.pending(); ok {
			ui.printf("[审批] 请输入 y 批准 / n 拒绝: ")
		} else {
			ui.printf("you> ")
		}
		if !scanner.Scan() { // EOF（Ctrl-D）
			ui.printf("\n")
			return 0
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/quit" {
			return 0
		}

		if id, ok := ui.pending(); ok {
			approved, valid := parseApproval(line)
			if !valid {
				ui.printf("输入无效：只接受 y 或 n（/quit 退出）\n")
				continue
			}
			if err := writeUplink(conn, uplinkMessage{
				Type: uplinkInteractionReply, InteractionID: id, Approved: &approved,
			}); err != nil {
				ui.printf("[发送失败] %v\n", err)
				return 1
			}
			ui.clearPending()
			if approved {
				ui.printf("[审批] 已批准，等待执行…\n")
			} else {
				ui.printf("[审批] 已拒绝，等待继续…\n")
			}
			continue
		}

		if err := writeUplink(conn, uplinkMessage{
			Type: uplinkChatMessage, SessionID: sessionID, Text: line,
		}); err != nil {
			ui.printf("[发送失败] %v\n", err)
			return 1
		}
	}
}

// parseApproval 把审批输入解析为布尔值：y/yes 批准，n/no 拒绝，其余无效。
func parseApproval(line string) (approved, valid bool) {
	switch strings.ToLower(line) {
	case "y", "yes":
		return true, true
	case "n", "no":
		return false, true
	}
	return false, false
}

// writeUplink 编码并发送一条上行消息。
func writeUplink(conn *websocket.Conn, msg uplinkMessage) error {
	data, err := encodeUplink(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeUplinkTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

// readDownlinkLoop 是下行消费循环：delta 流式打印，done 收尾，
// interaction.request 登记待答审批，error 应答直接展示。
func readDownlinkLoop(conn *websocket.Conn, ui *chatUI, done chan<- struct{}) {
	defer close(done)
	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			ui.printf("\n[连接已断开]\n")
			return
		}
		env, reply, err := decodeDownlink(data)
		if err != nil {
			ui.printf("\n[协议错误] %v\n", err)
			continue
		}
		switch {
		case env.Channel == channelDialogue && env.Type == eventMessageDelta:
			var p deltaPayload
			if err := json.Unmarshal(env.Payload, &p); err == nil {
				ui.printf("%s", p.Text)
			}
		case env.Channel == channelDialogue && env.Type == eventMessageDone:
			var p donePayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			if p.Error != "" {
				ui.printf("\n[run 失败] %s\n", p.Error)
			} else if p.Usage != nil {
				ui.printf("\n[done] turns=%d total_tokens=%d\n", p.Turns, p.Usage.TotalTokens)
			} else {
				ui.printf("\n[done] turns=%d\n", p.Turns)
			}
		case env.Channel == channelInteraction && env.Type == eventInteractionRequest:
			var p interactionRequestPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				continue
			}
			ui.setPending(p.InteractionID, p.Risk)
			ui.printf("\n[审批] %s（risk=%s，超时未答按拒绝处理）\n", p.Question, p.Risk)
		case reply.Type == "error":
			ui.printf("\n[error] %s: %s\n", reply.Code, reply.Message)
		case env.Channel != "":
			// 其余频道事件（progress/artifact 等）给一行可见性，不展开。
			ui.printf("\n[event] %s/%s\n", env.Channel, env.Type)
		}
	}
}
