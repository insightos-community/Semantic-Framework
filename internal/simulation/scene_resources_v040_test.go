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

package simulation

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedRuntimeBinding string

func (r fixedRuntimeBinding) RuntimePreference(string) (string, string, error) {
	return "native-mujoco", string(r), nil
}

func (fixedRuntimeBinding) RememberRuntimePreference(string, string, string) error { return nil }

type memoryRuntimePreference struct {
	profileID      string
	installationID string
}

func (r *memoryRuntimePreference) RuntimePreference(string) (string, string, error) {
	return r.profileID, r.installationID, nil
}

func (r *memoryRuntimePreference) RememberRuntimePreference(
	_, profileID, installationID string,
) error {
	r.profileID = profileID
	r.installationID = installationID
	return nil
}

type delayedStartRuntimeClient struct {
	*fakeRuntimeClient
	polls int
}

func (c *delayedStartRuntimeClient) StartScene(
	ctx context.Context, sceneKey string, request SceneStartRequest,
) (SceneInstance, error) {
	instance, err := c.fakeRuntimeClient.StartScene(ctx, sceneKey, request)
	if err != nil {
		return SceneInstance{}, err
	}
	instance.State = "starting"
	c.instance = instance
	return instance, nil
}

func (c *delayedStartRuntimeClient) Scene(
	context.Context, string,
) (SceneInstance, error) {
	c.polls++
	if c.polls >= 2 {
		c.instance.State = "running"
	}
	return c.instance, nil
}

type checkpointChannelSink chan SceneSnapshot

func (s checkpointChannelSink) ApplySimulationSnapshot(
	_ context.Context, _ string, snapshot SceneSnapshot,
) error {
	s <- snapshot
	return nil
}

func TestStartingSceneReturnsImmediatelyAndSyncsMapAfterRuntimeIsReadable(t *testing.T) {
	client := &delayedStartRuntimeClient{fakeRuntimeClient: &fakeRuntimeClient{
		healthy: true,
		info:    RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1"},
	}}
	profile := readyRuntimeProfile(
		"native-mujoco", "native",
		RuntimeCapability{EditableScene: true, SceneReset: true},
	)
	registry, err := NewRuntimeRegistry(RuntimeBinding{
		Profile: profile, Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := make(checkpointChannelSink, 1)
	service := NewServiceWithRegistry(
		registry,
		&memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)},
		checkpoints,
	)
	instance, err := service.StartScene(
		context.Background(), "project-async", "scene",
		SceneStartRequest{
			RequestID: "request-async", RuntimeProfileID: "native-mujoco",
		},
	)
	if err != nil || instance.State != "starting" {
		t.Fatalf("异步启动应立即返回 starting: instance=%+v err=%v", instance, err)
	}
	go service.observeCatalogSceneStart("project-async", instance)
	select {
	case snapshot := <-checkpoints:
		if snapshot.InstanceID != instance.InstanceID ||
			snapshot.Generation != instance.Generation {
			t.Fatalf("初始地图检查点指向错误实例: %+v", snapshot)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Runtime running 后没有生成初始地图检查点")
	}
}

func TestProjectRuntimeInstallationSelectionUsesProfileAndStartPreference(t *testing.T) {
	profile := readyRuntimeProfile(
		"native-mujoco", "native", RuntimeCapability{Viewer: true},
	)
	clientA := &fakeRuntimeClient{healthy: true}
	clientB := &fakeRuntimeClient{healthy: true}
	registry, err := NewRuntimeRegistry(
		RuntimeBinding{InstallationID: "native-a", Profile: profile, Client: clientA},
		RuntimeBinding{InstallationID: "native-b", Profile: profile, Client: clientB},
	)
	if err != nil {
		t.Fatal(err)
	}
	installations := &RuntimeInstallationCatalog{
		items: map[string]RuntimeInstallation{
			"native-a": {
				SchemaVersion: 2, InstallationID: "native-a", Profile: profile,
				LaunchMode: "remote", Endpoint: "http://runtime-a.test", Enabled: true,
			},
			"native-b": {
				SchemaVersion: 2, InstallationID: "native-b", Profile: profile,
				LaunchMode: "remote", Endpoint: "http://runtime-b.test", Enabled: true,
			},
		},
		order: []string{"native-a", "native-b"},
	}
	preference := &memoryRuntimePreference{profileID: "native-mujoco"}
	service := NewServiceWithRegistry(
		registry, &memoryRuntimeStateStore{values: make(map[string]ProjectRuntimeState)}, nil,
	)
	service.ConfigureProjectResources(installations, nil, preference)
	installations.ObserveStatus("native-a", "offline", "上一次 Probe 失败")
	profileID, preferredID, candidates, err := service.ProjectRuntimePreference("project-runtime")
	if err != nil || profileID != "native-mujoco" || preferredID != "" || len(candidates) != 2 {
		t.Fatalf("动态离线安装仍应作为可重新启动的候选: profile=%s preferred=%s candidates=%+v err=%v",
			profileID, preferredID, candidates, err)
	}

	if _, err := service.SelectProjectRuntimeInstallation(
		"project-runtime", "native-mujoco", "",
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("多个兼容安装且无偏好时必须由启动面板选择: %v", err)
	}
	selected, err := service.SelectProjectRuntimeInstallation(
		"project-runtime", "native-mujoco", "native-b",
	)
	if err != nil || selected.InstallationID != "native-b" {
		t.Fatalf("显式启动选择未生效: selected=%+v err=%v", selected, err)
	}

	preference.installationID = "native-a"
	selected, err = service.SelectProjectRuntimeInstallation(
		"project-runtime", "native-mujoco", "",
	)
	if err != nil || selected.InstallationID != "native-a" {
		t.Fatalf("可用的 Project 偏好未被复用: selected=%+v err=%v", selected, err)
	}

	disabled := installations.items["native-a"]
	disabled.Enabled = false
	installations.items["native-a"] = disabled
	selected, err = service.SelectProjectRuntimeInstallation(
		"project-runtime", "native-mujoco", "",
	)
	if err != nil || selected.InstallationID != "native-b" {
		t.Fatalf("偏好不可用且只剩一个候选时应自动回退: selected=%+v err=%v", selected, err)
	}
	if _, err := service.SelectProjectRuntimeInstallation(
		"project-runtime", "native-mujoco", "native-a",
	); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("启动面板显式选择不可用安装时必须返回明确错误: %v", err)
	}
}

func saveDocumentBundle(
	t *testing.T, store *FileStore, document SceneDocument, bundleID string,
) RuntimeBundle {
	t.Helper()
	bundle := RuntimeBundle{
		RuntimeBundleID:  bundleID,
		DocumentID:       document.ID,
		Revision:         document.Revision,
		SceneVersion:     document.Version + 1,
		SceneKey:         document.ID,
		RuntimeProfileID: "native-mujoco",
		Document:         NewRuntimeSceneDocument(document),
		Validation:       ValidationResult{Valid: true},
	}
	if err := store.SaveRuntimeBundle(document.ProjectID, bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestSceneLayoutsAndPackageRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	store := NewFileStore(fixedWorkspace{root: workspace})
	authoring := NewSceneAuthoringService(store)
	document, err := authoring.Create("project-layouts", "多布局场景")
	if err != nil {
		t.Fatal(err)
	}
	if document.PreviewArtifact == nil ||
		document.PreviewArtifact.Generator != layoutPreviewGeneratorVersion ||
		document.PreviewArtifact.Revision != document.Revision {
		t.Fatalf("Layout 预览 Artifact 元数据缺失: %+v", document.PreviewArtifact)
	}
	previewPath := filepath.Join(
		workspace, ".semantic", "simulation",
		filepath.FromSlash(document.PreviewArtifact.Path),
	)
	if data, err := os.ReadFile(previewPath); err != nil ||
		!strings.Contains(string(data), document.LayoutName) {
		t.Fatalf("Layout 预览 Artifact 未持久化: path=%s err=%v", previewPath, err)
	}
	second, err := authoring.CreateLayout(document.ProjectID, document.ID, "Layout 002")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.CreateLayout(document.ProjectID, document.ID, "layout 002"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Layout 名称应忽略大小写去重: %v", err)
	}
	second, err = authoring.RenameLayout(
		document.ProjectID, second.ID, second.Revision, "Layout 003",
	)
	if err != nil {
		t.Fatal(err)
	}

	saveDocumentBundle(t, store, document, "runtime-bundle-layout-001")
	publishedFirst, err := authoring.Publish(
		document.ProjectID, document.ID, document.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	saveDocumentBundle(t, store, second, "runtime-bundle-layout-003")
	publishedSecond, err := authoring.Publish(
		second.ProjectID, second.ID, second.Revision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authoring.DeleteLayout(document.ProjectID, publishedFirst.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("已发布 Layout 不允许删除: %v", err)
	}

	data, filename, err := authoring.ExportScenePackage(document.ProjectID, document.SceneID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filename, ".semantic-scene.zip") {
		t.Fatalf("导出文件名不正确: %s", filename)
	}
	imported, err := authoring.ImportScenePackage(
		"project-import", "native-mujoco", data,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 2 || imported[0].SceneID != imported[1].SceneID ||
		imported[0].SceneID == document.SceneID ||
		imported[0].Status != "draft" || imported[1].Status != "draft" {
		t.Fatalf("Scene Package 未导入为同一逻辑场景的两个独立草稿: %+v", imported)
	}
	names := map[string]bool{
		imported[0].LayoutName: true,
		imported[1].LayoutName: true,
	}
	if !names[publishedFirst.LayoutName] || !names[publishedSecond.LayoutName] {
		t.Fatalf("导入后 Layout 名称丢失: %+v", names)
	}

	var malicious bytes.Buffer
	writer := zip.NewWriter(&malicious)
	file, err := writer.Create("generated_scene.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("print('unsafe')")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.ImportScenePackage(
		"project-import", "native-mujoco", malicious.Bytes(),
	); err == nil || !strings.Contains(err.Error(), "不支持的文件") {
		t.Fatalf("包含脚本的 Scene Package 必须被拒绝: %v", err)
	}
}

func TestProjectPublishedCatalogKeepsLayoutBundlesAndCanStart(t *testing.T) {
	store := NewFileStore(fixedWorkspace{root: t.TempDir()})
	authoring := NewSceneAuthoringService(store)
	document, err := authoring.Create("project-catalog", "可启动多布局")
	if err != nil {
		t.Fatal(err)
	}
	second, err := authoring.CreateLayout(document.ProjectID, document.ID, "Layout B")
	if err != nil {
		t.Fatal(err)
	}
	firstBundle := saveDocumentBundle(t, store, document, "runtime-bundle-a")
	first, err := authoring.Publish(document.ProjectID, document.ID, document.Revision)
	if err != nil {
		t.Fatal(err)
	}
	secondBundle := saveDocumentBundle(t, store, second, "runtime-bundle-b")
	second, err = authoring.Publish(second.ProjectID, second.ID, second.Revision)
	if err != nil {
		t.Fatal(err)
	}

	profile := readyRuntimeProfile(
		"native-mujoco", "native",
		RuntimeCapability{
			EditableScene: true, Viewer: true, SceneReset: true, SceneStep: true,
			RobotModels: []string{"r1pro"},
		},
	)
	client := &recoveryRuntimeClient{
		fakeRuntimeClient: &fakeRuntimeClient{
			healthy: true,
			info: RuntimeInfo{
				State: "ready", Engine: "mujoco", APIVersion: "v1",
			},
		},
		profiles: []RuntimeProfile{profile},
	}
	registry, err := NewRuntimeRegistry(RuntimeBinding{
		InstallationID: "native-local", Profile: profile, Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithRegistry(registry, store, nil)
	installations := &RuntimeInstallationCatalog{
		items: map[string]RuntimeInstallation{
			"native-local": {
				SchemaVersion: 1, InstallationID: "native-local",
				Profile: profile, LaunchMode: "remote", Endpoint: "http://runtime.test",
				Enabled: true, Status: "offline",
			},
		},
		order: []string{"native-local"},
	}
	catalog := &SceneCatalogService{
		version: "test-v1", items: make(map[string]SceneCatalogEntry),
	}
	service.ConfigureProjectResources(
		installations, catalog, fixedRuntimeBinding("native-local"),
	)

	entries, err := service.RefreshProjectPublishedSceneCatalog(
		document.ProjectID, authoring,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Versions) != 2 {
		t.Fatalf("发布历史未进入目录: %+v", entries)
	}
	latest := entries[0].Versions[0]
	if len(latest.Variants) != 2 {
		t.Fatalf("最新目录版本未包含两个 Layout: %+v", latest)
	}
	var secondVariant SceneCatalogVariant
	for _, variant := range latest.Variants {
		if variant.VariantID == second.LayoutID {
			secondVariant = variant
		}
	}
	if secondVariant.RuntimeBundleID != secondBundle.RuntimeBundleID ||
		secondVariant.RuntimeSceneKey != second.ID {
		t.Fatalf("Layout 未指向自己的 RuntimeBundle: %+v", secondVariant)
	}
	if firstBundle.RuntimeBundleID == secondVariant.RuntimeBundleID ||
		first.LayoutID == second.LayoutID {
		t.Fatal("两个 Layout 的身份没有隔离")
	}

	instance, err := service.StartCatalogScene(
		context.Background(), document.ProjectID, entries[0].SceneID,
		CatalogSceneStartRequest{
			RequestID:     "start-layout-b",
			SceneVersion:  latest.Version,
			VariantID:     second.LayoutID,
			Headless:      true,
			RenderBackend: "egl",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if instance.SceneKey != second.ID || instance.Layout != "version-1" ||
		instance.RuntimeBundleID != secondBundle.RuntimeBundleID {
		t.Fatalf("启动未使用 Layout B 的 Bundle: %+v", instance)
	}
	if len(client.builds) != 1 ||
		client.builds[0].RuntimeBundleID != secondBundle.RuntimeBundleID {
		t.Fatalf("启动前未精确重注册 Layout B Bundle: %+v", client.builds)
	}

	// 模拟 Server 重启后的新目录对象；工作区文件足以恢复同样的目录版本。
	reloaded := &SceneCatalogService{
		version: "test-v1", items: make(map[string]SceneCatalogEntry),
	}
	service.sceneCatalog = reloaded
	reloadedEntries, err := service.RefreshProjectPublishedSceneCatalog(
		document.ProjectID, authoring,
	)
	if err != nil || len(reloadedEntries) != 1 ||
		reloadedEntries[0].Versions[0].Version != latest.Version {
		t.Fatalf("Server 重启后目录未确定性恢复: entries=%+v err=%v", reloadedEntries, err)
	}
}

func TestSceneCatalogReplacementIsAtomicAndProjectScoped(t *testing.T) {
	static := SceneCatalogEntry{
		SceneID: "static-scene", Name: "Static", Engine: "mujoco", Loader: "native",
		Source: "installation:native", CompatibleRuntimeProfile: "native-mujoco",
		Versions: []SceneCatalogVersion{{
			Version: "1.0.0", RuntimeSceneKey: "static", Published: true,
			Variants: []SceneCatalogVariant{{
				VariantID: "default", Name: "Default", Kind: "layout",
			}},
		}},
	}
	catalog := &SceneCatalogService{
		version: "test-v1",
		items:   map[string]SceneCatalogEntry{"static-scene": static},
		order:   []string{"static-scene"},
	}
	dynamic := static
	dynamic.SceneID = "dynamic-scene"
	dynamic.Name = "Dynamic"
	if err := catalog.ReplaceSourceEntries(
		"project:one", []SceneCatalogEntry{dynamic},
	); err != nil {
		t.Fatal(err)
	}
	collision := dynamic
	collision.SceneID = "static-scene"
	if err := catalog.ReplaceSourceEntries(
		"project:one", []SceneCatalogEntry{collision},
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("静态目录 ID 冲突必须拒绝: %v", err)
	}
	if _, err := catalog.Get("dynamic-scene"); err != nil {
		t.Fatalf("冲突替换不应先删除旧 Project 目录: %v", err)
	}
	service := &Service{sceneCatalog: catalog}
	if got := service.SceneCatalogForProject("two", ""); len(got) != 1 ||
		got[0].SceneID != "static-scene" {
		t.Fatalf("其他 Project 不应看到动态目录: %+v", got)
	}
}
