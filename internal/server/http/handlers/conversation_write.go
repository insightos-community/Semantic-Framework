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

package handlers

import (
	"errors"
	"net/http"

	"insightos.cn/semantic-framework/internal/agent/runtime"
	"insightos.cn/semantic-framework/internal/store"
)

// writeConversationWriteError 把所有 Conversation 写入口共用的门禁错误映射
// 成稳定 HTTP 结果。读取历史、工具目录和配置视图不调用本函数，因此归档或
// 非活动 Project 仍可复盘，只禁止继续产生新业务状态。
func writeConversationWriteError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, CodeSessionNotFound, "会话不存在")
	case errors.Is(err, store.ErrConversationArchived):
		writeError(w, http.StatusConflict, "CONVERSATION_ARCHIVED",
			"Conversation 已归档，只能查看历史")
	case errors.Is(err, store.ErrProjectArchived):
		writeError(w, http.StatusConflict, "PROJECT_ARCHIVED",
			"Project 已归档，不能修改 Conversation")
	case errors.Is(err, store.ErrProjectInactive):
		writeError(w, http.StatusConflict, "PROJECT_INACTIVE",
			"请先激活 Conversation 所属 Project")
	default:
		return false
	}
	return true
}
