package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"insightos.cn/semantic-framework/internal/install"
	"insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

func TestProjectImportsHTTPUsesPublishedSkillAndOwnership(t *testing.T) {
	st, _, _, _ := newProjectsTestRouter(t)
	project, err := st.CreateProject("usr-project", "导入测试")
	if err != nil {
		t.Fatal(err)
	}
	service := robot.NewService(st, nil)
	inbox := install.NewInbox(st, func(_ context.Context, _ string, _ install.Package, data []byte) ([]string, error) {
		item, err := service.PublishSkillArchive(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return []string{item.Name + "@" + item.Version}, nil
	})
	h := NewProjectsHandler(st, nil, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	h.SetImports(inbox)
	router := chi.NewRouter()
	router.Get("/projects/{id}/imports", h.HandleListImports)
	router.Post("/projects/{id}/imports", h.HandleUploadImport)
	request := func(user, method, path string, data []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req = req.WithContext(auth.ContextWithUserID(req.Context(), user))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	root := t.TempDir()
	manifest := `---
name: http-import
description: 接口导入测试
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
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	archive, err := install.PackSkillSource(root)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "/projects/" + project.ID + "/imports"
	if result := request("other-user", http.MethodPost, endpoint+"?filename=test.zip", archive); result.Code != http.StatusNotFound {
		t.Fatalf("跨用户访问: %d", result.Code)
	}
	result := request("usr-project", http.MethodPost, endpoint+"?filename=test.zip", archive)
	if result.Code != http.StatusOK {
		t.Fatal(result.Code, result.Body.String())
	}
	var imported struct {
		Item install.Record `json:"item"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Item.Status != "imported" {
		t.Fatalf("%+v", imported)
	}
	installed, err := st.GetRobotSkillPackage(imported.Item.Name, imported.Item.Version)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(installed.PackagePath)
	if err != nil || !bytes.Equal(content, archive) {
		t.Fatal("真实发布包与上传快照不一致", err)
	}
	if result := request("usr-project", http.MethodGet, endpoint, nil); result.Code != http.StatusOK {
		t.Fatal(result.Body.String())
	}
	projects, err := st.ListDevelopmentProjects()
	if err != nil || len(projects) == 0 {
		t.Fatal("开发项目扫描失败", err)
	}
	project, err = st.SetProjectMode(project.ID, store.ProjectModeRunning, project.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if result := request("usr-project", http.MethodPost, endpoint+"?filename=test.zip", archive); result.Code != http.StatusConflict {
		t.Fatalf("运行模式导入应阻止: %d", result.Code)
	}
	if result := request("usr-project", http.MethodGet, endpoint, nil); result.Code != http.StatusOK {
		t.Fatal("运行模式仍应支持查看")
	}
}
