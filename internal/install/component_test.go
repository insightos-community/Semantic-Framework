package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const modelManifest = "schema_version: 1\nkind: model\nname: test-model\nversion: 1.0.0\nrobot_models: [franka_panda]\nmodel_config: model.json\n"

func TestComponentManifestKeepsSingleComponentContract(t *testing.T) {
	for _, manifest := range []string{
		"schema_version: 1\nkind: bundle\nname: all\nversion: 1.0.0\n",
		modelManifest + "components: [{file: other.zip}]\n",
	} {
		if _, err := ParseComponent([]byte(manifest)); err == nil {
			t.Fatal("组件清单只描述自身，多组件导入使用 semantic-package.yaml")
		}
	}
}

func TestRemoveChecksBindingsAndAllowsReinstall(t *testing.T) {
	root := t.TempDir()
	s := &ComponentStore{Root: filepath.Join(root, "installed")}
	archive, id := componentArchive(t, root, `{}`)
	if _, err := s.Install(context.Background(), archive, id, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind(id, "robot", "franka_panda"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), id, nil); err == nil {
		t.Fatal("仍绑定的组件被删除")
	}
	next, nextID := componentArchive(t, root, `{"next":true}`)
	if _, err := s.Install(context.Background(), next, nextID, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind(nextID, "robot", "franka_panda"); err != nil {
		t.Fatal(err)
	}
	blocked := errors.New("活动执行仍在使用")
	if err := s.Remove(context.Background(), id, func(context.Context, InstalledComponent) error { return blocked }); !errors.Is(err, blocked) {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); !os.IsNotExist(err) {
		t.Fatal("卸载后仍有安装收据", err)
	}
	if _, err := s.Install(context.Background(), archive, id, func(string) {}); err != nil {
		t.Fatal("重新安装失败", err)
	}
}

func TestCancelledArchiveDoesNotExtract(t *testing.T) {
	root := t.TempDir()
	archive, _ := componentArchive(t, root, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ExtractZipContext(ctx, archive, filepath.Join(root, "unpacked")); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后仍然解包: %v", err)
	}
}

func componentArchive(t *testing.T, root, content string) (string, string) {
	t.Helper()
	data, err := packFiles(map[string][]byte{"semantic-component.yaml": []byte(modelManifest), "model.json": []byte(content)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, Digest(data)+".zip")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path, Digest(data)
}

func TestComponentInstallBindRollbackPreservesVersions(t *testing.T) {
	root := t.TempDir()
	store := &ComponentStore{Root: filepath.Join(root, "installed")}
	archive, id := componentArchive(t, root, `{"revision":"first"}`)
	first, err := store.Install(context.Background(), archive, id, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.Bind(id, "robot:franka", "franka_panda")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(id, "robot:r1", "r1_pro_chassis"); err == nil {
		t.Fatal("错误型号不能绑定")
	}
	other, nextID := componentArchive(t, root, `{"revision":"second"}`)
	if _, err := store.Install(context.Background(), other, nextID, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Bind(nextID, "robot:franka", "franka_panda"); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Rollback("robot:franka", binding.Revision)
	if err != nil || restored.Model.ComponentID != id {
		t.Fatalf("%+v %v", restored, err)
	}
	body, err := os.ReadFile(filepath.Join(first.Root, "model.json"))
	if err != nil || string(body) != `{"revision":"first"}` {
		t.Fatal("原组件被覆盖", err)
	}
	if _, err := store.Install(context.Background(), other, id, func(string) {}); err == nil {
		t.Fatal("替换同摘要归档必须失败")
	}
}

func TestComponentSourceBuildCachesAndDetectsChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SEMANTIC_BUILD_CACHE", filepath.Join(t.TempDir(), "cache"))
	recipe := modelManifest + "build:\n  files:\n    model.json: config.json\n"
	if err := os.WriteFile(filepath.Join(root, "semantic-source.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "model.zip")
	second := filepath.Join(t.TempDir(), "model.zip")
	if err := BuildSource(context.Background(), root, first, func(string) {}); err != nil {
		t.Fatal(err)
	}
	cached := false
	if err := BuildSource(context.Background(), root, second, func(message string) {
		if message == "源码与依赖未变，复用已构建组件" {
			cached = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if !cached || !bytes.Equal(a, b) {
		t.Fatal("未复用相同源码制品")
	}
	pkg, err := InspectArchive(first)
	if err != nil || len(pkg.SourceRevision) != 64 {
		t.Fatalf("%+v %v", pkg, err)
	}
	_ = os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"new":true}`), 0600)
	if err := BuildSource(context.Background(), root, second, func(string) {}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(second)
	if bytes.Equal(a, b) {
		t.Fatal("源码更新必须重建")
	}
}

func TestComponentInboxRequiresExplicitInstallAndCancels(t *testing.T) {
	root := t.TempDir()
	archive, _ := componentArchive(t, root, `{}`)
	inbox := NewInbox(projectFixture{root}, func(context.Context, string, Package, []byte) ([]string, error) {
		t.Fatal("组件不能调用旧 Skill 导入器")
		return nil, nil
	})
	started := make(chan struct{})
	inbox.SetInstaller(func(ctx context.Context, _ string, _ Record, _ string, _ Options, progress func(string)) ([]string, error) {
		close(started)
		progress("检查模型")
		<-ctx.Done()
		return nil, ctx.Err()
	})
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	item, err := inbox.UploadReader(context.Background(), "p", "model.zip", file)
	if err != nil || item.InstallationStatus != "pending" {
		t.Fatalf("%+v %v", item, err)
	}
	select {
	case <-started:
		t.Fatal("导入不应运行安装")
	default:
	}
	if _, err := inbox.Install("p", item.ID, Options{}); err == nil {
		t.Fatal("安装必须确认代码来源")
	}
	if _, err := inbox.Install("p", item.ID, Options{ConfirmCode: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("未开始安装")
	}
	if _, err := inbox.Install("p", item.ID, Options{ConfirmCode: true}); err != nil {
		t.Fatal(err)
	}
	if err := inbox.CancelInstall("p", item.ID); err != nil {
		t.Fatal(err)
	}
	inbox.jobs.Wait()
	items, err := inbox.List("p")
	if err != nil || items[0].InstallationStatus != "failed" || items[0].Error == "" {
		t.Fatalf("%+v %v", items, err)
	}
}

func TestComponentArchiveRejectsUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../escape", "/escape", "a/../../escape"} {
		data, _ := packFiles(map[string][]byte{"semantic-component.yaml": []byte(modelManifest), name: []byte("bad")})
		archive := filepath.Join(root, "bad.zip")
		_ = os.WriteFile(archive, data, 0600)
		if err := ExtractZip(archive, filepath.Join(root, "target")); err == nil {
			t.Fatal("危险路径应拒绝", name)
		}
	}
}

func TestProjectDefaultsDoNotReplaceExistingRobotBinding(t *testing.T) {
	root := t.TempDir()
	s := &ComponentStore{Root: filepath.Join(root, "installed")}
	first, id := componentArchive(t, root, `{"revision":"one"}`)
	if _, err := s.Install(context.Background(), first, id, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectDefault("p", id); err != nil {
		t.Fatal(err)
	}
	path, err := s.EnsureRobotBinding("p", "robot1", "franka_panda")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	second, next := componentArchive(t, root, `{"revision":"two"}`)
	if _, err := s.Install(context.Background(), second, next, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectDefault("p", next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureRobotBinding("p", "robot1", "franka_panda"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("默认绑定更新不应改动已有 Robot")
	}
	other, err := s.EnsureRobotBinding("other-project", "robot2", "franka_panda")
	if err != nil || other != "" {
		t.Fatal("项目默认绑定不可跨项目生效", err)
	}
}

func TestFailedArchiveCannotForgeInstallationReceipt(t *testing.T) {
	root := t.TempDir()
	data, _ := packFiles(map[string][]byte{"semantic-component.yaml": []byte(modelManifest), "installed.json": []byte(`{"kind":"model"}`)})
	path := filepath.Join(root, "bad.zip")
	_ = os.WriteFile(path, data, 0600)
	s := &ComponentStore{Root: filepath.Join(root, "store")}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := s.Install(context.Background(), path, Digest(data), func(string) {}); err == nil {
			t.Fatal("缺少模型文件应持续报安装失败")
		}
	}
}

func TestDirectoryReloadFailureRestoresOldDirectory(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "source"), filepath.Join(root, "catalog", "test")
	_ = os.MkdirAll(source, 0750)
	_ = os.MkdirAll(target, 0750)
	_ = os.WriteFile(filepath.Join(source, "new"), []byte("new"), 0600)
	_ = os.WriteFile(filepath.Join(target, "old"), []byte("old"), 0600)
	if err := ReplaceDirectoryAndApply(source, target, func() error { return context.Canceled }); err == nil {
		t.Fatal("应报告刷新失败")
	}
	if _, err := os.Stat(filepath.Join(target, "old")); err != nil {
		t.Fatal("应恢复原目录", err)
	}
}

func TestSourceArchiveIsRecognizedWithoutBuilding(t *testing.T) {
	data, _ := packFiles(map[string][]byte{"semantic-source.yaml": []byte("schema_version: 1\nkind: robot_ability\nname: test\nversion: 1.0\nbuild:\n  python_projects: ['../host']\n")})
	root := t.TempDir()
	path := filepath.Join(root, "source.zip")
	_ = os.WriteFile(path, data, 0600)
	pkg, err := InspectArchive(path)
	if err != nil || pkg.Kind != "robot_ability_source" {
		t.Fatal(pkg, err)
	}
	if err := BuildUploadedSource(context.Background(), path, filepath.Join(root, "src"), filepath.Join(root, "result.zip"), func(string) {}); err == nil {
		t.Fatal("上传源码不得引用包外依赖")
	}
}

func TestReplaceDirectoryArchivesOldContent(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "catalogs", "model")
	_ = os.MkdirAll(source, 0750)
	_ = os.MkdirAll(target, 0750)
	_ = os.WriteFile(filepath.Join(source, "new"), []byte("new"), 0600)
	_ = os.WriteFile(filepath.Join(target, "old"), []byte("old"), 0600)
	if err := ReplaceDirectory(source, target); err != nil {
		t.Fatal(err)
	}
	archives, err := os.ReadDir(filepath.Join(root, "installation-history"))
	if err != nil || len(archives) != 1 {
		t.Fatal("旧版本没有归档", err)
	}
	if _, err := os.Stat(filepath.Join(root, "installation-history", archives[0].Name(), "old")); err != nil {
		t.Fatal(err)
	}
}
