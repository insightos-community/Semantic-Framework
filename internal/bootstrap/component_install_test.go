package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/install"
	"insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

// 使用真实 Inbox、数据库和生产组件安装器，确认集合包选装后仍能独立更新组件。
// 本测试不启动仿真或策略进程，模型文件仅作为安装/绑定协议的最小样本。
func TestPackageInstallThroughInboxPreservesComponentIdentity(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(config.StoreConfig{Driver: "sqlite", SQLitePath: filepath.Join(root, "test.db")}, log.New(log.Options{Level: log.LevelError, Writer: io.Discard}))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	project, err := st.CreateProject("owner", "安装测试")
	if err != nil {
		t.Fatal(err)
	}
	components := &install.ComponentStore{Root: filepath.Join(root, "installations")}
	app := &App{components: components}
	app.imports = install.NewInbox(st, nil)
	app.imports.SetInstaller(componentInstaller(app, &config.Config{}, st, robot.NewService(st, nil), nil))
	source := filepath.Join(root, "model")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "schema_version: 1\nkind: model\nname: test-model\nversion: 1.0.0\nrobot_models: [franka_panda]\nmodel_config: model.json\n"
	for path, body := range map[string]string{"semantic-component.yaml": manifest, "model.json": `{}`} {
		if err := os.WriteFile(filepath.Join(source, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	child := filepath.Join(root, "model.zip")
	if err := install.ZipDirectory(source, child); err != nil {
		t.Fatal(err)
	}
	id, err := install.ArchiveDigest(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "semantic-package.yaml"), []byte("schema_version: 1\nname: project-test\nversion: 1.0.0\ncomponents:\n  - id: model\n    file: model.zip\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "project.zip")
	if err := install.BuildInstallPackage(root, archive); err != nil {
		t.Fatal(err)
	}
	upload := func(path string) install.Record {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		record, err := app.imports.UploadReader(context.Background(), project.ID, filepath.Base(path), f)
		if err != nil || record.Status != "imported" {
			t.Fatalf("%+v %v", record, err)
		}
		return record
	}
	startAndWait := func(record install.Record, options install.Options) install.Record {
		t.Helper()
		if _, err := app.imports.Install(project.ID, record.ID, options); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			items, err := app.imports.List(project.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.ID != record.ID {
					continue
				}
				if item.InstallationStatus == "failed" {
					t.Fatal(item.Error)
				}
				if item.InstallationStatus == "installed" {
					return item
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("安装未收敛")
		return install.Record{}
	}
	record := upload(archive)
	items, _ := components.List()
	if len(items) != 0 {
		t.Fatal("上传时提前安装了代码")
	}
	completed := startAndWait(record, install.Options{ConfirmCode: true, Components: []string{"model"}, ProjectDefault: true})
	if len(completed.SelectedComponents) != 1 || completed.SelectedComponents[0] != "model" {
		t.Fatal("选装结果未持久化", completed)
	}
	startAndWait(upload(child), install.Options{ConfirmCode: true})
	items, err = components.List()
	if err != nil || len(items) != 1 || items[0].ID != id {
		t.Fatalf("重复创建组件版本: %+v %v", items, err)
	}
	path, err := components.EnsureRobotBinding(project.ID, "franka-test", "franka_panda")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var binding install.RobotBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Model == nil || binding.Model.ComponentID != id {
		t.Fatalf("首次 Robot 未继承项目默认模型: %+v", binding)
	}
}
