// Package security 是安全体系的最小实现（架构文档 09）：safety middleware
// v1——包住每一次工具调用的统一安全入口。
//
// v1 落地两层强制（L0 提示词约束由 AGENT.md/SAFETY 注入，不走本包）：
//   - L2 参数校验：jsonschema 结构校验（物理约束随设备体系后期落地）；
//   - L4 人工审批：annotations.risk ≥ high 或命名空间命中
//     profile.interrupt.approval_required → 内核中断（interrupt），
//     等待审批结果后放行或结构化拒绝。
//
// L3 规则引擎、L5 资源锁、L6 安全停止是 Phase 2 的层，落地时扩展本包
// （同一挂接点 WrapToolCall）。
package security
