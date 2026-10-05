// Package ws 是 semantic-server 的 WebSocket 网关（接入层）。
//
// 对外提供两类通道（架构文档 02 §3.3）：
//   - /ws/agent-events：事件通道，多 Agent 协同事件经 Hub 按 session_id
//     投递或广播下行；
//   - /ws/chat：对话通道，上行 chat.message 经 MessageHandler（AgentRuntime）
//     执行，下行 dialogue 事件（message.delta/done）复用 Hub 投递。
//
// 本包只做协议适配与连接管理（升级/心跳/读写泵），事件的业务语义由
// 上游（runtime/聚合器）负责；MessageHandler 接口定义在本包以保持
// 依赖单向（runtime → ws 引用 Envelope，ws 不引用 runtime）。
package ws
