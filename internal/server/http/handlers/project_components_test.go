package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"insightos.cn/semantic-framework/internal/install"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/pkg/log"
)

func TestInstalledComponentSelectionIsSeparateFromActivation(t *testing.T) {
	st, _, _, _ := newProjectsTestRouter(t)
	p, err := st.CreateProject("usr-project", "绑定测试")
	if err != nil {
		t.Fatal(err)
	}
	components := &install.ComponentStore{Root: t.TempDir()}
	id := install.Digest([]byte("ability"))
	item := install.InstalledComponent{ID: id, Root: filepath.Join(components.Root, "components", id), Component: install.Component{Kind: "robot_ability", Name: "arm-ability", RobotModels: []string{"arm"}, Abilities: []install.AbilityComponent{{Role: "motion", Template: "motion", AbilityName: "Motion.V2", Package: "motion.zip"}}}}
	body, _ := json.Marshal(item)
	if err := os.MkdirAll(filepath.Join(components.Root, "receipts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(components.Root, "receipts", id+".json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	instance := robotruntime.RuntimeInstance{InstanceID: "instance", RobotID: "robot", RobotModel: "arm", Backend: "mujoco", BackendProfile: "sim", ProjectID: p.ID, Status: robotruntime.StateFailed, UpdatedAt: time.Now()}
	if err := st.SaveRuntimeInstance(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	h := NewProjectsHandler(st, nil, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	h.SetImports(install.NewInbox(st, nil))
	h.SetComponents(components)
	applied := 0
	h.SetComponentApply(func(_ context.Context, project, robot string) error {
		applied++
		if project != p.ID || robot != "robot" {
			t.Fatal("生效对象错误")
		}
		return errors.New("Robot 仍有任务占用")
	})
	router := chi.NewRouter()
	router.Post("/projects/{id}/components/bind", h.HandleBindComponents)
	router.Post("/projects/{id}/components/apply", h.HandleApplyComponents)
	router.Get("/projects/{id}/components", h.HandleComponentVersions)
	request := func(user, method, operation string, payload any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(payload)
		req := httptest.NewRequest(method, "/projects/"+p.ID+"/components"+operation, bytes.NewReader(data))
		req = req.WithContext(auth.ContextWithUserID(req.Context(), user))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	payload := map[string]any{"robot_id": "robot", "robot_model": "forged", "component_ids": []string{id}}
	if r := request("other-user", http.MethodPost, "/bind", payload); r.Code != 404 {
		t.Fatal("未拒绝跨用户绑定", r.Code)
	}
	r := request("usr-project", http.MethodPost, "/bind", payload)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if applied != 0 {
		t.Fatal("保存绑定触发了 Robot 重启")
	}
	before, err := os.ReadFile(install.BindingPath(components.Root, "robot"))
	if err != nil {
		t.Fatal(err)
	}
	var b install.RobotBinding
	if err := json.Unmarshal(before, &b); err != nil {
		t.Fatal(err)
	}
	if b.RobotModel != "arm" {
		t.Fatal("使用了客户端伪造型号")
	}
	if r := request("other-user", http.MethodPost, "/apply", map[string]string{"robot_id": "robot"}); r.Code != 404 {
		t.Fatal("未拒绝跨用户生效")
	}
	if applied != 0 {
		t.Fatal("跨用户调用了生效")
	}
	r = request("usr-project", http.MethodPost, "/apply", map[string]string{"robot_id": "robot"})
	if r.Code != 409 || applied != 1 {
		t.Fatal(r.Code, r.Body.String())
	}
	after, _ := os.ReadFile(install.BindingPath(components.Root, "robot"))
	if !bytes.Equal(before, after) {
		t.Fatal("生效失败丢失了保存的绑定")
	}
	r = request("usr-project", http.MethodGet, "", nil)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	var listing struct {
		Available []install.InstalledComponent `json:"available"`
		Installed []install.InstalledComponent `json:"installed"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Available) != 1 || len(listing.Installed) != 0 {
		t.Fatal("跨项目复用被错误绑定到导入记录")
	}
}
