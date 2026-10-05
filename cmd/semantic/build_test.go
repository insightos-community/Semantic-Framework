package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/install"
)

func TestBuildPreservesSkillVersionAndSourceInstallUsesSnapshot(t *testing.T) {
	source := t.TempDir()
	manifest := []byte("---\nname: sample\ndescription: 构建测试\ncategory: robot_skill\nversion: 0.1.2\n---\n# 测试\n")
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "nested", "skill.zip")
	if code := runBuild([]string{source, "--output", output}); code != 0 {
		t.Fatalf("无需 Server 或登录即可构建，实际退出码 %d", code)
	}
	pkg, err := install.InspectArchive(output)
	if err != nil || pkg.Version != "0.1.2" {
		t.Fatalf("交付版本变化: %+v %v", pkg, err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.zip")
	if err := buildSourcePackage(context.Background(), source, snapshot, true); err != nil {
		t.Fatal(err)
	}
	pkg, err = install.InspectArchive(snapshot)
	if err != nil || !strings.HasPrefix(pkg.Version, "0.1.2-dev.") {
		t.Fatalf("源码安装未保留开发快照: %+v %v", pkg, err)
	}
	body, err := os.ReadFile(filepath.Join(source, "SKILL.md"))
	if err != nil || string(body) != string(manifest) {
		t.Fatal("构建修改了原始源码", err)
	}
}

func TestBuildAndInstallHaveSeparateFlags(t *testing.T) {
	output := filepath.Join(t.TempDir(), "must-not-exist.zip")
	// 旧参数必须由参数解析直接拒绝，不能暗中继续构建或进入网络安装。
	if code := runInstall([]string{t.TempDir(), "--output", output}); code != 2 {
		t.Fatalf("旧构建入口仍被接受: %d", code)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("旧参数产生了文件", err)
	}
	for _, args := range [][]string{nil, {t.TempDir()}, {t.TempDir(), "--output", output, "--project", "p"}, {t.TempDir(), "--output", output, "extra"}} {
		if code := runBuild(args); code != 2 {
			t.Fatalf("无效 build 参数被接受: %v %d", args, code)
		}
	}
}

func TestBuildComponentRecipe(t *testing.T) {
	source := t.TempDir()
	for name, content := range map[string]string{
		"semantic-component.yaml": "schema_version: 1\nkind: model\nname: sample\nversion: 1.0.0\nrobot_models: [franka_panda]\nmodel_config: model.json\n",
		"model.json":              "{}",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(t.TempDir(), "model.zip")
	if code := runBuild([]string{source, "--output", output}); code != 0 {
		t.Fatal(code)
	}
	pkg, err := install.InspectArchive(output)
	if err != nil || pkg.Kind != "model" {
		t.Fatalf("%+v %v", pkg, err)
	}
}
