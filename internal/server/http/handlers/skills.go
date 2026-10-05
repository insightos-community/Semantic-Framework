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
	"os"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/skill"
)

// SkillsHandler 是技能库的 REST 处理器（架构文档 06 §2 skill store 的
// 只读视图）：清单暴露 frontmatter 四标准字段（不含正文），详情含正文与
// 具身扩展字段透传（Extensions 原样输出，供前端展示）。
type SkillsHandler struct {
	// skillsFn 返回当前技能存储：经 provider 间接取值而非固定指针——
	// skills.dir 热重载的补建路径（启动时技能目录不可用 → 运行中补建）
	// 会替换 store 实例（见 bootstrap applySkillsDir），handler 须始终
	// 读到最新一份；返回 nil = 无技能形态。
	skillsFn func() *skill.Store
}

// NewSkillsHandler 创建技能库处理器。
func NewSkillsHandler(skillsFn func() *skill.Store) *SkillsHandler {
	return &SkillsHandler{skillsFn: skillsFn}
}

// skillSummary 是 GET /skills 的清单条目：frontmatter 四标准字段 +
// 可选 tags 扩展，不含正文（列表页摘要展示与筛选用）。
type skillSummary struct {
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	WhenToUse   string   `json:"when_to_use"`
	Tags        []string `json:"tags,omitempty"`
}

// skillsResponse 是 GET /skills 的响应体。
type skillsResponse struct {
	// Skills 全部技能摘要（category 升序分组、组内 name 升序；无技能为 []）。
	Skills []skillSummary `json:"skills"`
}

// skillDetail 是 GET /skills/{name} 的详情：frontmatter 标准字段 +
// markdown 正文 + 扩展字段透传（Dir 是服务端文件系统路径，不暴露）。
type skillDetail struct {
	Name        string               `json:"name"`
	Category    string               `json:"category"`
	Description string               `json:"description"`
	WhenToUse   string               `json:"when_to_use"`
	Body        string               `json:"body"`
	Extensions  map[string]any       `json:"extensions,omitempty"`
	Resources   []skill.ResourceInfo `json:"resources"`
}

// skillDetailResponse 是 GET /skills/{name} 的响应体。
type skillDetailResponse struct {
	Skill skillDetail `json:"skill"`
}

// HandleListSkills 处理 GET /api/v1/skills：返回技能清单摘要，
// 按 category 分组排序（组间 category 升序、组内 name 升序，渲染稳定）。
// 无技能形态（技能目录不可用）返回空清单而非错误——与 agents 端点
// 未配置 Team 时返回空清单同语义。
func (h *SkillsHandler) HandleListSkills(w http.ResponseWriter, _ *http.Request) {
	summaries := []skillSummary{}
	if st := h.skillsFn(); st != nil {
		for _, sk := range st.List() {
			summaries = append(summaries, skillSummary{
				Name:        sk.Name,
				Category:    sk.Category,
				Description: sk.Description,
				WhenToUse:   sk.WhenToUse,
				Tags:        skillTags(sk.Extensions),
			})
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Category != summaries[j].Category {
			return summaries[i].Category < summaries[j].Category
		}
		return summaries[i].Name < summaries[j].Name
	})
	writeJSON(w, http.StatusOK, skillsResponse{Skills: summaries})
}

// skillTags 从可选扩展字段中提取字符串标签。非法项忽略、去空白与去重，
// 保持 SKILL.md 声明顺序；不把 tags 提升为 loader 必填或框架执行语义。
func skillTags(extensions map[string]any) []string {
	raw, ok := extensions["tags"]
	if !ok {
		return nil
	}
	var values []any
	switch tags := raw.(type) {
	case []any:
		values = tags
	case []string:
		values = make([]any, len(tags))
		for i, tag := range tags {
			values[i] = tag
		}
	case string:
		for _, tag := range strings.Split(tags, ",") {
			values = append(values, tag)
		}
	default:
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		tag, ok := value.(string)
		if !ok {
			continue
		}
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		result = append(result, tag)
	}
	return result
}

// HandleGetSkill 处理 GET /api/v1/skills/{name}：返回技能详情
// （frontmatter 标准字段 + 正文 + 扩展字段透传）。未命中（含无技能
// 形态）返回 404 SKILL_NOT_FOUND。
func (h *SkillsHandler) HandleGetSkill(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var sk skill.Skill
	var ok bool
	if st := h.skillsFn(); st != nil {
		sk, ok = st.Get(name)
	}
	if !ok {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "技能不存在: "+name)
		return
	}
	resources, err := skill.ListResources(sk)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SKILL_RESOURCES_UNAVAILABLE", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, skillDetailResponse{Skill: skillDetail{
		Name:        sk.Name,
		Category:    sk.Category,
		Description: sk.Description,
		WhenToUse:   sk.WhenToUse,
		Body:        sk.Body,
		Extensions:  sk.Extensions,
		Resources:   resources,
	}})
}

// skillResourceResponse 是单个文本资源的按需读取结果。内容不进入技能清单
// 或详情首包，避免大型 reference 在用户未查看时占用网络和浏览器内存。
type skillResourceResponse struct {
	Resource skill.ResourceInfo `json:"resource"`
	Content  string             `json:"content"`
}

// HandleGetSkillResource 处理 GET /api/v1/skills/{name}/resources/*：仅允许
// 读取 scripts/references/assets 下不超过上限的 UTF-8 普通文件。Skill 根目录
// 绝对路径和服务器其他文件始终不出现在响应中。
func (h *SkillsHandler) HandleGetSkillResource(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	st := h.skillsFn()
	if st == nil {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "技能不存在: "+name)
		return
	}
	sk, ok := st.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "SKILL_NOT_FOUND", "技能不存在: "+name)
		return
	}
	resource, content, err := skill.ReadResource(sk, chi.URLParam(r, "*"))
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeError(w, http.StatusNotFound, "SKILL_RESOURCE_NOT_FOUND", "技能资源不存在")
		case errors.Is(err, skill.ErrInvalidResourcePath):
			writeError(w, http.StatusBadRequest, "SKILL_RESOURCE_PATH_INVALID", err.Error())
		case errors.Is(err, skill.ErrResourceTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "SKILL_RESOURCE_TOO_LARGE", err.Error())
		case errors.Is(err, skill.ErrResourceNotText):
			writeError(w, http.StatusUnsupportedMediaType, "SKILL_RESOURCE_NOT_TEXT", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "SKILL_RESOURCE_UNAVAILABLE", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, skillResourceResponse{Resource: resource, Content: content})
}
