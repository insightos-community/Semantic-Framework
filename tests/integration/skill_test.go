package integration

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// 技能系统的集成测试：真实装配（bootstrap.Wire，含种子技能目录）验证
// "种子技能加载 → skill 工具经完整 middleware 栈执行 → 文件热更自动生效"。
//
// 断言路径说明：mock 模型实例在 runtime
// 内部构建，集成层拿不到 CallInputs，故"清单进工具描述、正文 inline 注入、
// S1 不被 skill 指引挡住"的内容级断言在 kernel 单测（TestSkillMiddlewareListing /
// TestSkillBodyInjection / TestContextMiddlewareForeignSystemMessage）；
// 集成层验证装配链与数据流：种子技能经 Wire 加载进 store、脚本化的 skill
// 工具调用经完整栈（含 safety 豁免）跑通 run、写入新技能文件经 fsnotify
// 热更进快照。

// copySkillsDir 把仓库的种子技能复制到临时目录并返回路径：热更用例要向
// 目录写入新技能，不能污染真实 configs/skills。
func copySkillsDir(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "configs", "skills"))
	if err != nil {
		t.Fatalf("解析种子技能目录失败: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "skills")
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("复制种子技能失败: %v", err)
	}
	return dst
}

// startSkillApp 以指定技能目录启动装配后的服务（mock 模型 + 真实 leader
// profile，单 leader 模式之外的形态与 startChatApp 一致）。
func startSkillApp(t *testing.T, skillsDir string) (httpBase, wsBase string, app *bootstrap.App, stop func()) {
	t.Helper()
	t.Setenv("SEMANTIC_ADMIN_PASSWORD", "test-admin-pass")

	profilesDir, err := filepath.Abs(filepath.Join("..", "..", "configs", "agents"))
	if err != nil {
		t.Fatalf("解析 profile 目录失败: %v", err)
	}
	cfg := config.Default()
	cfg.Server.HTTPAddr = freeAddr(t)
	cfg.Server.WSAddr = freeAddr(t)
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "skill.db")
	storetest.SeedMigratedAt(t, cfg.Store.SQLitePath)
	cfg.LLM.Default = "mock"
	cfg.Agents.ProfilesDir = profilesDir
	cfg.Agents.TeamsDir = filepath.Join(t.TempDir(), "no-teams")
	cfg.Skills.Dir = skillsDir
	logger := log.New(log.Options{Level: log.LevelError, Writer: io.Discard})

	app, err = bootstrap.Wire(cfg, logger)
	if err != nil {
		t.Fatalf("Wire 装配失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- app.Run(ctx)
	}()

	httpBase = "http://" + cfg.Server.HTTPAddr
	wsBase = "ws://" + cfg.Server.WSAddr
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(httpBase + "/api/v1/system/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("服务在 30s 内未就绪，最后一次错误: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stop = func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("App.Run 应随 ctx 取消正常退出，实际返回: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("App.Run 未在 15s 内退出")
		}
	}
	return httpBase, wsBase, app, stop
}

// TestSkillSeedLoadedAndHotReload 验证种子技能经 Wire 装配进 skill store
// （两个 general 种子技能可读、Summary 渲染清单），且目录内新增技能文件
// 经 fsnotify 监听自动热更进快照（500ms 去抖，轮询等待）。
func TestSkillSeedLoadedAndHotReload(t *testing.T) {
	skillsDir := copySkillsDir(t)
	_, _, app, stop := startSkillApp(t, skillsDir)
	defer stop()

	st := app.SkillStore()
	if st == nil {
		t.Fatalf("技能存储应已装配（种子技能目录存在）")
	}
	for _, name := range []string{"echo-guide", "artifact-usage"} {
		sk, ok := st.Get(name)
		if !ok {
			t.Errorf("种子技能 %q 应已加载", name)
			continue
		}
		if sk.Category != "general" || sk.Body == "" {
			t.Errorf("种子技能 %q 字段不全: %+v", name, sk)
		}
	}
	summary := st.Summary()
	if !strings.Contains(summary, "### general") ||
		!strings.Contains(summary, "- artifact-usage: ") ||
		!strings.Contains(summary, "- echo-guide: ") {
		t.Errorf("Summary 应含按 general 分组的两个种子技能，实际:\n%s", summary)
	}

	// 热更：写入新技能文件 → 监听去抖后自动 Reload（新会话/下轮 run 生效）。
	hotDir := filepath.Join(skillsDir, "general", "hot-skill")
	if err := os.MkdirAll(hotDir, 0o755); err != nil {
		t.Fatalf("创建热更技能目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hotDir, "SKILL.md"),
		[]byte("---\nname: hot-skill\ndescription: 热更写入的技能\n---\n\n热更正文。\n"), 0o644); err != nil {
		t.Fatalf("写入热更技能失败: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if sk, ok := st.Get("hot-skill"); ok && strings.Contains(sk.Body, "热更正文") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("热更技能 30s 内未进入快照")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// skillCallScript 生成技能调用场景的 mock 脚本：第一轮调 skill 工具加载
// echo-guide（种子技能），第二轮总结。
func skillCallScript(t *testing.T) string {
	t.Helper()
	script, err := json.Marshal([]map[string]any{
		{"tool_calls": []map[string]string{{
			"id":        "call-1",
			"name":      "skill",
			"arguments": `{"skill":"echo-guide"}`,
		}}},
		{"content": "已按技能指导使用 system.time 回答。"},
	})
	if err != nil {
		t.Fatalf("构造 mock 脚本失败: %v", err)
	}
	return string(script)
}

// TestSkillToolCallRun 验证脚本化的 skill 工具调用经完整 middleware 栈跑通：
// 技能加载工具由 skill middleware 注入、经 safety 豁免集放行（门禁清单只
// 覆盖注册表工具），run 正常完成（2 轮，无错误）。
func TestSkillToolCallRun(t *testing.T) {
	t.Setenv("SEMANTIC_MOCK_SCRIPT", skillCallScript(t))
	skillsDir := copySkillsDir(t)
	httpBase, wsBase, _, stop := startSkillApp(t, skillsDir)
	defer stop()

	token := login(t, httpBase)
	sessionID, conn := createSessionAndDial(t, httpBase, wsBase, token)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	sendChatMessage(t, conn, sessionID, "现在几点了？")
	done := awaitMessageDone(t, conn)
	if done.Error != "" {
		t.Fatalf("run 不应失败，实际错误: %q", done.Error)
	}
	if done.Turns != 2 {
		t.Errorf("应为 2 轮（skill 调用 + 总结），实际: %d", done.Turns)
	}
	if !strings.Contains(done.Text, "已按技能指导") {
		t.Errorf("最终文本不符: %q", done.Text)
	}
}
