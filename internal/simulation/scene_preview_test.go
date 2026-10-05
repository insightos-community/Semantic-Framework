package simulation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func previewTestService(t *testing.T) (*Service, SceneCatalogEntry) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := SceneCatalogEntry{SceneID: "any-task", Name: "任意任务", ContentRoot: filepath.Join(root, "content"),
		CompatibleRuntimeProfile: "test", Versions: []SceneCatalogVersion{{Version: "1", RuntimeSceneKey: "any:7",
			Variants: []SceneCatalogVariant{{VariantID: "init-0"}, {VariantID: "init-9"}}}}}
	s := &Service{sceneCatalog: &SceneCatalogService{directory: root, items: map[string]SceneCatalogEntry{e.SceneID: e}, order: []string{e.SceneID}}}
	return s, e
}

func TestScenePreviewOverlayPreservesOriginalAndCacheIdentity(t *testing.T) {
	s, e := previewTestService(t)
	key, root := s.previewLocation(e, e.Versions[0])
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	writePreviewJSON(filepath.Join(root, "result.json"), map[string]any{"description": "官方任务", "variants": map[string]any{
		"init-9": map[string]string{"preview": "9.jpg", "description": "杯子位置变化 3 cm"}}})
	writePreviewJSON(filepath.Join(root, "status.json"), ScenePreviewStatus{"ready", "已完成"})
	for i := 0; i < 2; i++ {
		got := s.SceneCatalog("")[0]
		if got.Description != "官方任务" || got.Versions[0].Variants[1].Preview != "/simulation/scene-previews/"+key+"/9.jpg" {
			t.Fatalf("overlay: %+v", got)
		}
		if got.PreviewPreparation.State != "ready" {
			t.Fatal(got.PreviewPreparation)
		}
	}
	original, _ := s.sceneCatalog.Get(e.SceneID)
	if original.Versions[0].Variants[1].Preview != "" {
		t.Fatal("目录被预览读取修改")
	}
	next, _ := s.previewLocation(original, original.Versions[0])
	if next != key {
		t.Fatal("缓存身份被展示修改")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(e.ContentRoot), ".component-id"), []byte("new-content"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, _ := s.previewLocation(e, e.Versions[0])
	if changed == key {
		t.Fatal("组件变化必须使缓存失效")
	}
}

func TestScenePreviewMissingRuntimeDoesNotRemoveScene(t *testing.T) {
	s, e := previewTestService(t)
	s.PrepareScenePreviews(context.Background(), []string{e.SceneID}, func(string) {})
	got := s.SceneCatalog("")
	if len(got) != 1 || got[0].PreviewPreparation.State != "unavailable" {
		t.Fatalf("%+v", got)
	}
}

func TestScenePreviewFilesRejectTraversal(t *testing.T) {
	s, _ := previewTestService(t)
	for _, pair := range [][2]string{{"../secret", "a.jpg"}, {"ab", "a.jpg"}, {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "../a.jpg"}} {
		if _, err := s.ScenePreviewFile(pair[0], pair[1]); err == nil {
			t.Fatal("accepted", pair)
		}
	}
}

func TestBundledPreviewsWorkWithoutRuntime(t *testing.T) {
	s, e := previewTestService(t)
	if err := os.MkdirAll(e.ContentRoot, 0700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(e.ContentRoot, "preview.jpg")
	if err := os.WriteFile(image, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	e.Preview = "preview.jpg"
	for i := range e.Versions[0].Variants {
		e.Versions[0].Variants[i].Preview = "preview.jpg"
	}
	s.sceneCatalog.items[e.SceneID] = e
	s.PrepareScenePreviews(context.Background(), []string{e.SceneID}, func(string) {})
	got := s.SceneCatalog("")[0]
	if got.PreviewPreparation.State != "ready" || got.Preview != "/simulation/scene-preview-assets/any-task" {
		t.Fatalf("%+v", got)
	}
	path, err := s.BundledScenePreviewFile(e.SceneID, "1", "init-9")
	if err != nil || path != image {
		t.Fatalf("%s: %v", path, err)
	}
	outside := filepath.Join(filepath.Dir(e.ContentRoot), "outside.jpg")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.ContentRoot, "escape.jpg")); err != nil {
		t.Fatal(err)
	}
	e.Preview = "escape.jpg"
	s.sceneCatalog.items[e.SceneID] = e
	if _, err := s.BundledScenePreviewFile(e.SceneID, "", ""); err == nil {
		t.Fatal("读取了内容目录外的文件")
	}
}

func TestCancelQueuedPreviewClosesStatus(t *testing.T) {
	s, e := previewTestService(t)
	s.previewMu.Lock()
	if err := s.StartScenePreviews(e.SceneID); err != nil {
		s.previewMu.Unlock()
		t.Fatal(err)
	}
	s.CancelScenePreviews(e.SceneID)
	s.previewMu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, running := s.previewJobs.Load(e.SceneID); !running {
			if s.SceneCatalog("")[0].PreviewPreparation.State != "cancelled" {
				t.Fatal("排队任务未结束")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("取消没有收敛")
}
