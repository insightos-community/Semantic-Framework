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
	"encoding/json"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/reduction"

	"insightos.cn/semantic-framework/internal/store"
)

// 本文件是 eino reduction middleware（上下文瘦身，架构文档 10 §5）的
// 外置后端适配：超限的工具结果不丢，全文经 store 的 artifact 机制持久化
// （元数据落库、本体落产物目录），上下文中只留 引用+预览——与架构文档
// 05 §9"tool result 只返回引用"的产物语义一致，模型可用 artifact.get
// 按 ID 取回全文。

const (
	// reductionMaxLengthForTrunc 是单条工具结果的外置截断长度（字符）：
	// 超过即把全文外置 store，上下文留 引用+预览（10 §4 S8 的超限处置）。
	reductionMaxLengthForTrunc = 20000

	// reductionReadFileTool 是外置引用的读回工具名（模型侧净化名
	// artifact.get → artifact_get）：截断提示引导模型按产物 ID 取回全文。
	reductionReadFileTool = "artifact_get"

	// offloadSummaryRunes 是外置产物摘要的截取长度（rune）。
	offloadSummaryRunes = 80
)

// genArtifactOffloadPath 生成外置路径：直接用产物 ID（art-<uuid>）。
// reduction 的截断提示把路径原样展示给模型，产物 ID 形态让模型可以
// 不经转换地调用 artifact.get 取回全文——路径即引用，无需另建映射。
// 每次生成新 uuid（不用 call_id）：同一次工具调用在截断/清理两个阶段
// 可能各外置一次，互不相同才覆盖。
func genArtifactOffloadPath(_ context.Context, _ *reduction.ToolDetail) (string, error) {
	return store.NewArtifactID(), nil
}

// reductionBackend 实现 reduction.Backend（Write 单方法接口）：把外置
// 内容写入 store 的 artifact 存储（路径即产物 ID，由 genArtifactOffloadPath
// 生成）。
type reductionBackend struct {
	// st 元数据存储（产物元数据与内容本体）。
	st *store.Store
}

// Write 以路径为产物 ID 持久化外置内容：摘要取内容开头（产物清单展示用），
// 元数据标记来源为 reduction 外置（与 artifact.put 写入的产物区分，追溯用）。
func (b reductionBackend) Write(_ context.Context, req *filesystem.WriteRequest) error {
	metadata, err := json.Marshal(map[string]string{"source": "reduction"})
	if err != nil {
		return err
	}
	_, err = b.st.PutArtifactWithID(req.FilePath, "text/plain",
		offloadSummary(req.Content), string(metadata), []byte(req.Content))
	return err
}

// offloadSummary 从外置内容开头截取摘要（前 offloadSummaryRunes runes）。
func offloadSummary(content string) string {
	runes := []rune(content)
	if len(runes) > offloadSummaryRunes {
		return string(runes[:offloadSummaryRunes]) + "…"
	}
	return content
}
