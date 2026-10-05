// Package team 是 Team 定义的加载层（架构文档 04 §4）。
//
// Team 是多 Agent 协同的容器：1 个 leader（协调者）+ N 个成员
// （工作者/观察者/服务者）+ 1 个共享黑板。定义是声明式 yaml
// （configs/agents/teams/*.yaml），本包只做"加载 + 校验 + 默认值"，
// 组建（按成员角色的交互模式装配）在 internal/agent/runtime 完成。
package team
