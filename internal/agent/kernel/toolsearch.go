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

package kernel

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
)

// toolSearchToolName 是 ToolSearch 元工具在模型侧的名字：与 eino toolsearch
// middleware 的工具名一致，这里显式固定——safety 门禁的豁免集与本名同源
// （同 skillToolName 先例），防两处漂移。
const toolSearchToolName = "tool_search"

// buildToolSearchMiddleware 构建 eino toolsearch middleware（工具三级选择的
// 第二级"动态检索"，架构文档 05 §6）：动态工具集（DynamicTools）经 middleware
// 的 BeforeAgent 注入 tools 节点（可执行），但首轮模型调用前从 ToolInfos
// 剥离（不可见）；模型调用 tool_search 元工具按关键词检索后，命中工具追加
// 回后续轮次的 ToolInfos——同一 run 内多次检索累积可见（命中清单以工具
// 结果消息留在历史里，middleware 每轮重放扫描）。
//
// 检索累积与前缀缓存的关系（为什么挂接判定要设预算）：client 侧检索模式下
// ToolInfos 在 run 内会变（首轮剥离 + 检索后追加），工具清单是多数厂商
// 请求前缀的一部分，每次检索命中都可能使后续轮次的前缀缓存失效。累积语义
// 把这个代价收敛到最小——工具只追加不重排（追加在清单尾部，已命中部分
// 的前缀保持稳定），且一个 run 内检索次数有限。但代价依然真实存在：
// 动态工具很少时，隐藏 schema 省下的 token 抵不过一次额外检索轮与缓存
// 失效，因此 runtime.buildToolchain 只在 schema 估算超过上下文 10% 时
// 切分 DynamicTools；无法取得上下文预算时才使用“动态工具超过 10 个”的
// 后备阈值。kernel 只认“开关 + 非空动态集”，预算判定不在本层。
//
// cfg.ToolSearch 为 false 或 DynamicTools 为空时不挂接（返回 nil, nil）——
// 关闭/无动态集形态与存量行为一致（全部工具经 Tools 直通注入）。
func buildToolSearchMiddleware(ctx context.Context, cfg AgentConfig) (adk.ChatModelAgentMiddleware, error) {
	if !cfg.ToolSearch || len(cfg.DynamicTools) == 0 {
		return nil, nil
	}
	mw, err := toolsearch.New(ctx, &toolsearch.Config{DynamicTools: cfg.DynamicTools})
	if err != nil {
		return nil, fmt.Errorf("构建 toolsearch middleware 失败: %w", err)
	}
	return mw, nil
}
