package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"insightos.cn/semantic-framework/internal/install"
)

func TestCLIBuildsSelectablePackage(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "source")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: test-skill\ndescription: 打包测试\ncategory: robot_skill\nversion: 1.0.0\n---\n# 测试\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := install.PackSkillSource(skillDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skill.zip"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "semantic-package.yaml"), []byte("schema_version: 1\nname: test-project\nversion: 1.0.0\ncomponents: [{id: skill, file: skill.zip}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "project.zip")
	if code := runBuild([]string{root, "--output", output}); code != 0 {
		t.Fatalf("构建退出码 %d", code)
	}
	pkg, err := install.InspectArchive(output)
	if err != nil || pkg.Kind != "package" || len(pkg.Components) != 1 {
		t.Fatalf("%+v %v", pkg, err)
	}
	if code := runInstall([]string{output, "--project", "p", "--component", "missing"}); code != 1 {
		t.Fatalf("未知组件应在上传前拒绝，退出码 %d", code)
	}
}

func TestCLIImportUsesAuthenticatedProjectUpload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/p/imports" || r.URL.Query().Get("filename") != "中文包.zip" {
			t.Errorf("请求不正确: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Content-Type") != "application/zip" {
			t.Error("缺少上传身份或类型")
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != "archive" {
			t.Error("上传内容发生变化")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"item": install.Record{ID: "digest", Status: "imported", Package: install.Package{Kind: "scene", Name: "测试场景"}}})
	}))
	defer server.Close()
	client, err := newClient(server.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	item, err := client.importPackage("p", "中文包.zip", []byte("archive"))
	if err != nil || item.Name != "测试场景" {
		t.Fatalf("%+v %v", item, err)
	}
}

func TestCLIImportReportsRecordedFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"item": install.Record{ID: "digest", Status: "failed", Error: "包版本冲突"}})
	}))
	defer server.Close()
	client, _ := newClient(server.URL, "token")
	if _, err := client.importPackage("p", "test.zip", nil); err == nil {
		t.Fatal("不能把失败记录报告为安装成功")
	}
}
