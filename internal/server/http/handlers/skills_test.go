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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/pkg/log"
)

// 技能夹具：三个技能跨两个 category（含扩展字段透传与缺省 when_to_use），
// 验证清单的分组排序与详情的字段完整性。
var skillFixtures = map[string]string{
	"general/echo-guide": `---
name: echo-guide
description: 使用 system.echo 与 system.time 完成链路探测与时间查询的规范
category: general
when_to_use: 需要回显文本验证工具链路，或需要获取当前时间时
---

# 链路探测与时间查询

正文标记 ECHO-BODY。
`,
	"general/artifact-usage": `---
name: artifact-usage
description: 产物（artifact）的存取规范
category: general
when_to_use: 需要保存报告/导出结果，或读取已保存产物时
---

# 产物存取规范
`,
	"embodied/pick-place": `---
name: pick-place
description: 抓取放置任务的执行流程
category: embodied
when_to_use: 需要控制机械臂抓取并放置物体时
tags: [grasp, pick-and-place]
goal: 将目标物体从 A 点移动到 B 点
safety_rules:
  - 夹爪力度不超过 10N
---

# 抓取放置
`,
}

// newSkillsTestStore 把技能夹具写入临时目录并加载为真实 store。
func newSkillsTestStore(t *testing.T) *skill.Store {
	t.Helper()
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})
	root := t.TempDir()
	for rel, content := range skillFixtures {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("创建技能目录失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s/SKILL.md 失败: %v", rel, err)
		}
	}
	// echo-guide 附带标准 scripts/references/assets，验证详情清单与按需读取。
	echoDir := filepath.Join(root, "general", "echo-guide")
	for rel, content := range map[string]string{
		"scripts/check.py":    "print('ok')\n",
		"references/usage.md": "# 使用说明\n",
		"assets/example.json": "{\"ok\":true}\n",
	} {
		path := filepath.Join(echoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("创建技能资源目录失败: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写入技能资源 %s 失败: %v", rel, err)
		}
	}
	binaryPath := filepath.Join(echoDir, "assets", "binary.bin")
	if err := os.WriteFile(binaryPath, []byte{0xff, 0xfe, 0xfd}, 0o644); err != nil {
		t.Fatalf("写入二进制技能资源失败: %v", err)
	}
	st, err := skill.NewStore(root, logger)
	if err != nil {
		t.Fatalf("加载技能 store 失败: %v", err)
	}
	return st
}

// newSkillsTestRouter 装配技能库测试路由（provider 直接给固定 store）。
func newSkillsTestRouter(t *testing.T) http.Handler {
	t.Helper()
	st := newSkillsTestStore(t)
	h := NewSkillsHandler(func() *skill.Store { return st })
	r := chi.NewRouter()
	r.Get("/api/v1/skills", h.HandleListSkills)
	r.Get("/api/v1/skills/{name}/resources/*", h.HandleGetSkillResource)
	r.Get("/api/v1/skills/{name}", h.HandleGetSkill)
	return r
}

func getSkills(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestListSkills 验证清单端点：category 升序分组、组内 name 升序；
// 条目含 frontmatter 四标准字段与可选 tags（不返回正文）。
func TestListSkills(t *testing.T) {
	router := newSkillsTestRouter(t)
	rec := getSkills(t, router, "/api/v1/skills")

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Skills) != 3 {
		t.Fatalf("应返回 3 个技能，实际: %s", rec.Body.String())
	}
	// embodied < general；general 组内 artifact-usage < echo-guide。
	wantOrder := []string{"pick-place", "artifact-usage", "echo-guide"}
	for i, name := range wantOrder {
		if body.Skills[i]["name"] != name {
			t.Errorf("第 %d 个技能应为 %s: %s", i, name, rec.Body.String())
		}
	}
	first := body.Skills[0]
	if first["category"] != "embodied" || first["when_to_use"] == "" || first["description"] == "" {
		t.Errorf("清单条目字段不符: %+v", first)
	}
	if _, hasBody := first["body"]; hasBody {
		t.Errorf("清单条目不应含正文 body: %+v", first)
	}
	if _, hasExt := first["extensions"]; hasExt {
		t.Errorf("清单条目不应含扩展字段: %+v", first)
	}
	tags, ok := first["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "grasp" || tags[1] != "pick-and-place" {
		t.Errorf("清单条目的 tags 不符: %+v", first)
	}
	if _, hasTags := body.Skills[1]["tags"]; hasTags {
		t.Errorf("未配置 tags 时清单不应输出空字段: %+v", body.Skills[1])
	}
}

func TestSkillTags(t *testing.T) {
	tags := skillTags(map[string]any{
		"tags": []any{" vision ", "camera", "vision", 42, ""},
	})
	if strings.Join(tags, ",") != "vision,camera" {
		t.Fatalf("tags 应清理空白、非法项并去重，实际: %#v", tags)
	}
	if got := skillTags(map[string]any{"tags": "debug, time"}); strings.Join(got, ",") != "debug,time" {
		t.Fatalf("逗号字符串兼容解析不符: %#v", got)
	}
	if got := skillTags(nil); got != nil {
		t.Fatalf("无 tags 应返回 nil，实际: %#v", got)
	}
}

// TestListSkillsNoStore 验证无技能形态（provider 返回 nil）：200 且为空
// 数组而非 null/错误（与 agents 未配置 Team 同语义）。
func TestListSkillsNoStore(t *testing.T) {
	h := NewSkillsHandler(func() *skill.Store { return nil })
	r := chi.NewRouter()
	r.Get("/api/v1/skills", h.HandleListSkills)
	rec := getSkills(t, r, "/api/v1/skills")

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Skills == nil || len(body.Skills) != 0 {
		t.Errorf("无技能形态应返回空数组: %s", rec.Body.String())
	}
}

// TestGetSkill 验证详情端点：frontmatter 标准字段 + 正文 + 扩展字段透传。
func TestGetSkill(t *testing.T) {
	router := newSkillsTestRouter(t)
	rec := getSkills(t, router, "/api/v1/skills/pick-place")

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Skill map[string]any `json:"skill"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	sk := body.Skill
	if sk["name"] != "pick-place" || sk["category"] != "embodied" ||
		sk["description"] == "" || sk["when_to_use"] == "" {
		t.Errorf("详情标准字段不符: %+v", sk)
	}
	bodyStr, _ := sk["body"].(string)
	if !strings.Contains(bodyStr, "# 抓取放置") {
		t.Errorf("详情应含正文原文: %+v", sk)
	}
	ext, ok := sk["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("详情应含扩展字段透传: %+v", sk)
	}
	if ext["goal"] != "将目标物体从 A 点移动到 B 点" {
		t.Errorf("扩展字段 goal 透传不符: %+v", ext)
	}
	rules, ok := ext["safety_rules"].([]any)
	if !ok || len(rules) != 1 || rules[0] != "夹爪力度不超过 10N" {
		t.Errorf("扩展字段 safety_rules 透传不符: %+v", ext)
	}
}

// TestGetSkillEchoGuide 验证无扩展字段的技能：详情不含 extensions 键
// （omitempty），正文原样返回。
func TestGetSkillEchoGuide(t *testing.T) {
	router := newSkillsTestRouter(t)
	rec := getSkills(t, router, "/api/v1/skills/echo-guide")

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Skill map[string]any `json:"skill"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if _, hasExt := body.Skill["extensions"]; hasExt {
		t.Errorf("无扩展字段的技能不应输出 extensions: %+v", body.Skill)
	}
	if body.Skill["body"] == "" {
		t.Errorf("详情应含正文: %+v", body.Skill)
	}
	resources, ok := body.Skill["resources"].([]any)
	if !ok || len(resources) != 4 {
		t.Fatalf("详情应列出 scripts/references/assets 资源: %+v", body.Skill)
	}
	first, _ := resources[0].(map[string]any)
	if first["path"] != "assets/binary.bin" || first["kind"] != "assets" {
		t.Fatalf("资源应按相对路径排序并标记类型: %+v", resources)
	}
}

// TestGetSkillResource 验证资源内容按需读取，详情 API 不需要内联脚本正文。
func TestGetSkillResource(t *testing.T) {
	router := newSkillsTestRouter(t)
	rec := getSkills(t, router, "/api/v1/skills/echo-guide/resources/scripts/check.py")
	if rec.Code != http.StatusOK {
		t.Fatalf("文本资源应返回 200，实际: %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Resource skill.ResourceInfo `json:"resource"`
		Content  string             `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("资源响应不是合法 JSON: %v", err)
	}
	if body.Resource.Path != "scripts/check.py" || body.Resource.Kind != "scripts" ||
		body.Content != "print('ok')\n" {
		t.Fatalf("资源响应不符: %+v", body)
	}
}

func TestGetSkillResourceRejectsBinary(t *testing.T) {
	router := newSkillsTestRouter(t)
	rec := getSkills(t, router, "/api/v1/skills/echo-guide/resources/assets/binary.bin")
	if rec.Code != http.StatusUnsupportedMediaType ||
		!strings.Contains(rec.Body.String(), "SKILL_RESOURCE_NOT_TEXT") {
		t.Fatalf("二进制资源应拒绝在线文本预览: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestGetSkillNotFound 验证未知技能名：404 SKILL_NOT_FOUND（统一错误格式）。
func TestGetSkillNotFound(t *testing.T) {
	router := newSkillsTestRouter(t)
	rec := getSkills(t, router, "/api/v1/skills/no-such-skill")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("应返回 404，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Error.Code != "SKILL_NOT_FOUND" {
		t.Errorf("错误码应为 SKILL_NOT_FOUND: %s", rec.Body.String())
	}
}

// TestGetSkillNoStore 验证无技能形态下详情端点同样 404。
func TestGetSkillNoStore(t *testing.T) {
	h := NewSkillsHandler(func() *skill.Store { return nil })
	r := chi.NewRouter()
	r.Get("/api/v1/skills/{name}", h.HandleGetSkill)
	rec := getSkills(t, r, "/api/v1/skills/echo-guide")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("无技能形态应返回 404，实际: %d（body: %s）", rec.Code, rec.Body.String())
	}
}

// TestSkillsProviderSwap 验证 provider 间接取值：运行中把 nil 换成真实
// store（skills.dir 热重载补建路径的缩影），端点无需重装配即读到新快照。
func TestSkillsProviderSwap(t *testing.T) {
	var st *skill.Store
	h := NewSkillsHandler(func() *skill.Store { return st })
	r := chi.NewRouter()
	r.Get("/api/v1/skills", h.HandleListSkills)

	rec := getSkills(t, r, "/api/v1/skills")
	var before struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(before.Skills) != 0 {
		t.Fatalf("补建前应为空清单: %s", rec.Body.String())
	}

	st = newSkillsTestStore(t)
	rec = getSkills(t, r, "/api/v1/skills")
	var after struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(after.Skills) != 3 {
		t.Fatalf("补建后应读到 3 个技能: %s", rec.Body.String())
	}
}
