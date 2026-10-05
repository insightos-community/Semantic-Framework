package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

const testManifest = `---
name: test-grasp
description: 测试抓取
category: robot_skill
version: 0.1.0
required_actions:
  - type: robot.state
    schema_version: 1
stop_actions:
  - type: robot.hold
    schema_version: 1
---
# 测试
`

type projectFixture struct{ root string }

func (p projectFixture) GetProject(id string) (store.Project, error) {
	return store.Project{ID: id, WorkspaceRoot: p.root, Mode: store.ProjectModeDevelopment}, nil
}
func (p projectFixture) ListDevelopmentProjects() ([]store.Project, error) {
	project, _ := p.GetProject("p")
	return []store.Project{project}, nil
}

func TestInspectPackages(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string][]byte
		kind  string
	}{
		{"skill", map[string][]byte{"SKILL.md": []byte(testManifest)}, "robot_skill"},
		{"legacy-folder", map[string][]byte{"test-grasp/SKILL.md": []byte(testManifest)}, "robot_skill"},
		{"scene", map[string][]byte{"semantic-scene.yaml": []byte("schema_version: 1\nname: 测试场景\n")}, "scene"},
		{"ambiguous", map[string][]byte{"SKILL.md": []byte(testManifest), "semantic-scene.yaml": {}}, ""},
		{"traversal", map[string][]byte{"SKILL.md": []byte(testManifest), "../escape": {}}, ""},
		{"agent-skill", map[string][]byte{"SKILL.md": []byte(strings.Replace(testManifest, "robot_skill", "agent_skill", 1))}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive, err := packFiles(test.files)
			if err != nil {
				t.Fatal(err)
			}
			pkg, err := Inspect(archive)
			if test.kind == "" {
				if err == nil {
					t.Fatal("应拒绝无效包")
				}
				return
			}
			if err != nil || pkg.Kind != test.kind {
				t.Fatalf("%+v %v", pkg, err)
			}
		})
	}
}

func TestInboxUploadScanDedupAndRetry(t *testing.T) {
	ctx := context.Background()
	p := projectFixture{t.TempDir()}
	calls := 0
	fail := false
	apply := func(context.Context, string, Package, []byte) ([]string, error) {
		calls++
		if fail {
			return nil, errors.New("依赖尚未安装")
		}
		return []string{"test-grasp@0.1.0"}, nil
	}
	inbox := NewInbox(p, apply)
	archive, _ := packFiles(map[string][]byte{"SKILL.md": []byte(testManifest)})
	first, err := inbox.Upload(ctx, "p", "skill.zip", archive)
	if err != nil || first.Status != "imported" {
		t.Fatalf("%+v %v", first, err)
	}
	root, _ := inbox.Directory("p")
	path := filepath.Join(root, "renamed.zip")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Second)
	_ = os.Chtimes(path, old, old)
	// 重建 Inbox 模拟 Server 重启；上传与目录扫描仍使用持久化内容身份去重。
	inbox = NewInbox(p, apply)
	if err := inbox.Scan(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("同一内容重复导入 %d 次", calls)
	}
	fail = true
	other, _ := packFiles(map[string][]byte{"SKILL.md": []byte(testManifest + "更新内容")})
	failed, err := inbox.Upload(ctx, "p", "other.zip", other)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("%+v %v", failed, err)
	}
	fail = false
	_, _ = inbox.Upload(ctx, "p", "other.zip", other)
	if calls != 2 {
		t.Fatal("失败包不应自动重试")
	}
	retried, err := inbox.Retry(ctx, "p", failed.ID)
	if err != nil || retried.Status != "imported" || calls != 3 {
		t.Fatalf("%+v %v calls=%d", retried, err, calls)
	}
	items, err := inbox.List("p")
	if err != nil || len(items) != 2 {
		t.Fatalf("%+v %v", items, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("投递源文件应保留", err)
	}
}

func TestPackSkillSourceImmutableRevision(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(testManifest), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=private"), 0600)
	first, err := PackSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PackSkillSource(root)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("相同源码必须产生相同包", err)
	}
	pkg, err := Inspect(first)
	if err != nil || !strings.HasPrefix(pkg.Version, "0.1.0-dev.") {
		t.Fatalf("%+v %v", pkg, err)
	}
	manifest, _ := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if string(manifest) != testManifest {
		t.Fatal("源码被修改")
	}
	_ = os.WriteFile(filepath.Join(root, "skill.py"), []byte("# 修改实现"), 0600)
	changed, err := PackSkillSource(root)
	if err != nil || bytes.Equal(first, changed) {
		t.Fatal("源码更新应改变包", err)
	}
	changedPkg, _ := Inspect(changed)
	if changedPkg.Version == pkg.Version {
		t.Fatal("开发版本未变化")
	}
}

func TestInboxRunInitialScanAndStop(t *testing.T) {
	p := projectFixture{t.TempDir()}
	done := make(chan struct{})
	imported := make(chan struct{}, 1)
	inbox := NewInbox(p, func(context.Context, string, Package, []byte) ([]string, error) {
		imported <- struct{}{}
		return nil, nil
	})
	root, _ := inbox.Directory("p")
	_ = os.MkdirAll(root, 0700)
	archive, _ := packFiles(map[string][]byte{"SKILL.md": []byte(testManifest)})
	path := filepath.Join(root, "skill.zip")
	_ = os.WriteFile(path, archive, 0600)
	old := time.Now().Add(-3 * time.Second)
	_ = os.Chtimes(path, old, old)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { defer close(done); inbox.Run(ctx, func(err error) { t.Error(err) }) }()
	select {
	case <-imported:
	case <-time.After(3 * time.Second):
		t.Fatal("后台未自动导入")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("扫描未随 Server 停止")
	}
}

func TestInboxOversizedFileHasVisibleRejection(t *testing.T) {
	inbox := NewInbox(projectFixture{t.TempDir()}, func(context.Context, string, Package, []byte) ([]string, error) {
		t.Error("超限包不应进入导入器")
		return nil, nil
	})
	root, _ := inbox.Directory("p")
	_ = os.MkdirAll(root, 0700)
	file, err := os.Create(filepath.Join(root, "too-large.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxUploadBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	old := time.Now().Add(-3 * time.Second)
	_ = os.Chtimes(file.Name(), old, old)
	if err := inbox.Scan(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	items, err := inbox.List("p")
	if err != nil || len(items) != 1 || items[0].Status != "rejected" {
		t.Fatalf("拒绝原因应可见: %+v %v", items, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".packages")); !os.IsNotExist(err) {
		t.Fatal("不应复制超限文件")
	}
}
