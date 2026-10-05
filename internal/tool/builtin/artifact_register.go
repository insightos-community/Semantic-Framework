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

package builtin

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/tool"
)

const (
	// nameArtifactRegister 是工作区文件显式登记工具的稳定名称。
	nameArtifactRegister = "artifact.register"

	// maxRegisteredArtifactBytes 与 REST 登记接口保持相同的 100MB 上限。
	maxRegisteredArtifactBytes = 100 << 20
)

// artifactRegisterTool 把 Project workspace 文件复制登记到 Artifact Store。
type artifactRegisterTool struct {
	st *store.Store
}

// artifactRegisterArgs 是 artifact.register 的模型参数。
type artifactRegisterArgs struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Summary   string `json:"summary"`
}

// Def 返回 artifact.register 的契约。它只登记既有工作区文件，不负责执行
// 命令或隐式扫描输出。
func (t *artifactRegisterTool) Def() tool.Definition {
	return tool.Definition{
		Name: nameArtifactRegister, Namespace: "artifact",
		Description: "把当前 Project workspace 内的既有文件显式登记为 ArtifactRef。" +
			"仅在结果需要进入对话、跨 Agent/会话引用或长期保存时使用。",
		ParametersJSON: `{
			"type":"object",
			"properties":{
				"path":{"type":"string","minLength":1,"description":"Project workspace 内相对文件路径"},
				"media_type":{"type":"string","description":"可选媒体类型，缺省自动推断"},
				"summary":{"type":"string","description":"可选的一句话摘要"}
			},
			"required":["path"],
			"additionalProperties":false
		}`,
		Annotations: tool.Annotations{Risk: tool.RiskHigh},
	}
}

// Run 读取单次 Run 的用户和 Project 作用域，复制文件并返回稳定引用。
func (t *artifactRegisterTool) Run(ctx context.Context, argsJSON string) (string, error) {
	var args artifactRegisterArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "参数不是合法 JSON: " + err.Error()}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok {
		return "", &tool.Error{Code: "EXECUTION_SCOPE_MISSING", Message: "artifact.register 只能在已绑定 Project 的会话中运行"}
	}
	if scope.OwnerID == "" {
		return "", &tool.Error{Code: "ARTIFACT_OWNER_MISSING", Message: "当前会话缺少 Artifact 用户归属"}
	}
	absPath, relativePath, err := tool.ResolveWorkspacePath(scope.WorkspaceRoot, args.Path)
	if err != nil {
		return "", &tool.Error{Code: "WORKSPACE_PATH_INVALID", Message: err.Error()}
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", &tool.Error{Code: "ARTIFACT_FILE_INVALID", Message: "只能登记 Project workspace 内的普通文件"}
	}
	if info.Size() > maxRegisteredArtifactBytes {
		return "", &tool.Error{Code: "ARTIFACT_TOO_LARGE", Message: "Artifact 文件不能超过 100MB"}
	}
	content, err := os.ReadFile(absPath)
	if err != nil {
		return "", &tool.Error{Code: "ARTIFACT_READ_FAILED", Message: "读取工作区文件失败: " + err.Error()}
	}
	mediaType := strings.TrimSpace(args.MediaType)
	if mediaType == "" {
		mediaType = mime.TypeByExtension(filepath.Ext(absPath))
	}
	if mediaType == "" {
		mediaType = http.DetectContentType(content)
	}
	summary := strings.TrimSpace(args.Summary)
	if summary == "" {
		summary = filepath.Base(absPath)
	}
	metadata, _ := json.Marshal(map[string]string{
		"source": "project_workspace", "project_id": scope.ProjectID, "workspace_path": relativePath,
	})
	artifact, err := t.st.PutUserArtifact(scope.OwnerID, mediaType, summary, string(metadata), content)
	if err != nil {
		return "", &tool.Error{Code: "ARTIFACT_REGISTER_FAILED", Message: err.Error()}
	}
	return tool.OKResult(map[string]any{
		"artifact_id": artifact.ID, "uri": artifact.URI, "media_type": artifact.MediaType,
		"summary": artifact.Summary, "size": artifact.Size, "workspace_path": relativePath,
	})
}
