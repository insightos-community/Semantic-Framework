package simulation

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestDisableRuntimePreservesActiveScene(t *testing.T) {
	profile := readyRuntimeProfile("native-mujoco", "native", RuntimeCapability{})
	client := &recoveryRuntimeClient{fakeRuntimeClient: &fakeRuntimeClient{healthy: true,
		info: RuntimeInfo{State: "ready", Engine: "mujoco", APIVersion: "v1", ActiveInstanceID: "active-scene"}},
		profiles: []RuntimeProfile{profile}}
	registry, err := NewRuntimeRegistry(RuntimeBinding{InstallationID: "local-native", Profile: profile, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &RuntimeInstallationCatalog{items: map[string]RuntimeInstallation{"local-native": {
		InstallationID: "local-native", Profile: profile, Enabled: true, SourceFile: "fixture.yaml"}}}
	service := &Service{installations: catalog, supervisor: NewRuntimeSupervisor(registry)}
	if _, _, err := service.SetRuntimeInstallationEnabled(context.Background(), "local-native", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("活动场景应先走停止流程: %v", err)
	}
	if item, _ := catalog.Get("local-native"); !item.Enabled {
		t.Fatal("不应修改安装状态")
	}
}

func TestInstalledRuntimeLaunchUsesProfileAndPackSettings(t *testing.T) {
	item := RuntimeInstallation{SchemaVersion: runtimeInstallationSchemaVersion,
		InstallationID: "local-libero", Profile: RuntimeProfile{RuntimeProfileID: "libero-robosuite-1.4"},
		Enabled: true, LaunchMode: "process", Command: []string{"/installed/bin/semantic-sim-runtime"},
		Endpoint: "http://127.0.0.1:2092", SettingsPath: "/installed/runtime-settings.json"}
	launcher := item.Binding().Launcher.(ExecLauncher)
	for _, expected := range []string{"SEMANTIC_SIM_PROFILE=libero-robosuite-1.4",
		"SEMANTIC_RUNTIME_CONFIG=/installed/runtime-settings.json", "PLUGIN_MUJOCO_PORT=2092"} {
		if !slices.Contains(launcher.Env, expected) {
			t.Fatalf("正式启动缺少包声明的环境 %s: %v", expected, launcher.Env)
		}
	}
}
