package simulation

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

const profileRuntimeURLVariable = "PLUGIN_MUJOCO_PROFILE_URL"

// TestHTTPRuntimeClientWithRealProfileRuntime 验证 Framework 可以直接连接真实的
// robosuite 或 LIBERO 隔离 Runtime。测试默认跳过，只有专项 Pipeline 明确提供
// PLUGIN_MUJOCO_PROFILE_URL 时才会创建场景，避免普通 go test 意外占用仿真进程。
//
// 真实环境加载可能包含模型编译和数据集读取，因此本测试使用独立的长超时客户端；
// 所有失败路径都会尝试停止由本测试创建的实例，但绝不会停止测试开始前已有的实例。
func TestHTTPRuntimeClientWithRealProfileRuntime(t *testing.T) {
	endpoint := os.Getenv(profileRuntimeURLVariable)
	if endpoint == "" {
		t.Skip(profileRuntimeURLVariable + " 未设置")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := NewHTTPRuntimeClient(endpoint, &http.Client{Timeout: 90 * time.Second})

	if err := client.Health(ctx); err != nil {
		t.Fatalf("Profile Runtime 健康检查失败: %v", err)
	}
	info, err := client.Runtime(ctx)
	if err != nil {
		t.Fatalf("读取 Profile Runtime 信息失败: %v", err)
	}
	if info.RuntimeProfileID != "robosuite-1.5" &&
		info.RuntimeProfileID != "libero-robosuite-1.4" {
		t.Fatalf("连接目标不是受支持的 Profile Runtime: %+v", info)
	}
	expectedSceneKind := "robosuite"
	if info.RuntimeProfileID == "libero-robosuite-1.4" {
		expectedSceneKind = "libero"
	}
	profiles, err := client.RuntimeProfiles(ctx)
	if err != nil {
		t.Fatalf("读取 Runtime Profile 能力失败: %v", err)
	}
	if len(profiles) != 1 ||
		profiles[0].RuntimeProfileID != info.RuntimeProfileID ||
		profiles[0].Engine != info.Engine ||
		!profiles[0].Capabilities.NativeEvaluator ||
		!slices.Contains(profiles[0].SceneKinds, expectedSceneKind) {
		t.Fatalf("Runtime Profile 与进程信息不一致: info=%+v profiles=%+v", info, profiles)
	}
	if info.Engine != "mujoco" || !info.Capabilities.NativeEvaluator {
		t.Fatalf("Profile Runtime 没有声明 MuJoCo 原生评测能力: %+v", info)
	}
	if info.ActiveInstanceID != "" {
		t.Fatalf("专项测试要求独占空闲 Runtime，当前活动实例为 %s", info.ActiveInstanceID)
	}

	// 不存在实例必须稳定映射为 ErrNotFound。Framework 依靠这一分类区分
	// Runtime 断连与实例已被释放，不能只比较服务端错误字符串。
	_, err = client.Scene(ctx, "missing-"+uuid.NewString())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在实例没有映射为 ErrNotFound: %v", err)
	}

	scenes, err := client.ListScenes(ctx)
	if err != nil {
		t.Fatalf("读取 Profile 场景目录失败: %v", err)
	}
	scene, layout := selectProfileScene(t, scenes, info.RuntimeProfileID)
	request := SceneStartRequest{
		RequestID:        "framework-profile-e2e-" + uuid.NewString(),
		RuntimeProfileID: info.RuntimeProfileID,
		Layout:           layout,
		Seed:             7,
		Headless:         true,
		RenderBackend:    "egl",
	}
	instance, err := client.StartScene(ctx, scene.SceneKey, request)
	if err != nil {
		t.Fatalf("启动 Profile 场景失败: %v", err)
	}
	defer stopProfileInstance(t, client, instance.InstanceID)

	// 启动请求需要具备幂等性；相同 request_id 和输入只能返回原实例。
	repeated, err := client.StartScene(ctx, scene.SceneKey, request)
	if err != nil || repeated.InstanceID != instance.InstanceID {
		t.Fatalf("重复启动请求没有返回原实例: first=%+v repeated=%+v err=%v",
			instance, repeated, err)
	}
	conflicting := request
	conflicting.Seed++
	if _, err := client.StartScene(ctx, scene.SceneKey, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("相同 request_id 的不同输入没有映射为 ErrConflict: %v", err)
	}

	instance = waitProfileInstance(t, ctx, client, instance.InstanceID)
	if instance.RuntimeProfileID != info.RuntimeProfileID || instance.Generation < 1 {
		t.Fatalf("场景实例缺少 Profile 或 generation: %+v", instance)
	}

	robots, err := client.Robots(ctx, instance.InstanceID)
	if err != nil {
		t.Fatalf("读取虚拟 Robot 失败: %v", err)
	}
	if len(robots) != 1 || robots[0].BackendProfile != info.RuntimeProfileID ||
		robots[0].SDKPackage != "semantic-robot-sdk-franka" || robots[0].Backend != "mujoco" || len(robots[0].JointNames) != 7 {
		t.Fatalf("Franka Robot 描述与 Profile 不一致: %+v", robots)
	}
	robotState, err := client.RobotState(ctx, robots[0].RobotID)
	if err != nil || robotState.Generation != instance.Generation {
		t.Fatalf("Robot 状态 generation 与场景不一致: state=%+v err=%v", robotState, err)
	}

	evaluation, err := client.SceneEvaluation(ctx, instance.InstanceID)
	if err != nil {
		t.Fatalf("读取原生评测证据失败: %v", err)
	}
	if evaluation.InstanceID != instance.InstanceID ||
		evaluation.SceneKey != scene.SceneKey ||
		evaluation.RuntimeProfileID != info.RuntimeProfileID ||
		evaluation.Generation != instance.Generation ||
		evaluation.ObservedAt.IsZero() || evaluation.Metrics == nil {
		t.Fatalf("原生评测字段或 generation 不完整: %+v", evaluation)
	}

	// 错误 generation 必须由 Runtime 拒绝，不能让 reset 前的状态控制当前 Robot。
	if _, err := client.HoldRobot(ctx, robots[0].RobotID, instance.Generation+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("错误 generation 的 hold 没有映射为 ErrConflict: %v", err)
	}

	stopped, err := client.SceneOperation(ctx, instance.InstanceID, "stop", nil)
	if err != nil {
		t.Fatalf("停止 Profile 场景失败: %v", err)
	}
	if stopped.State != "stopped" || stopped.Generation != instance.Generation {
		t.Fatalf("停止终态或 generation 不正确: %+v", stopped)
	}
}

// selectProfileScene 默认选择 Runtime 返回的第一项，也允许专项流水线用环境变量
// 固定具体场景。布局始终来自场景目录，避免 Framework 猜测 robosuite/LIBERO 内部名称。
func selectProfileScene(
	t *testing.T, scenes []SceneDescriptor, profileID string,
) (SceneDescriptor, string) {
	t.Helper()
	if len(scenes) == 0 {
		t.Fatal("Profile Runtime 没有返回可运行场景")
	}
	wanted := os.Getenv("PLUGIN_MUJOCO_PROFILE_SCENE_KEY")
	for _, scene := range scenes {
		if wanted != "" && scene.SceneKey != wanted {
			continue
		}
		if !slices.Contains(scene.CompatibleRuntimeProfiles, profileID) {
			t.Fatalf("场景 %s 未声明与 %s 兼容: %+v", scene.SceneKey, profileID, scene)
		}
		if !scene.ReadOnly || len(scene.Layouts) == 0 {
			t.Fatalf("Profile 原生场景必须为只读并提供 layout: %+v", scene)
		}
		return scene, scene.Layouts[0]
	}
	t.Fatalf("Profile Runtime 不包含指定场景 %q，可用场景: %+v", wanted, scenes)
	return SceneDescriptor{}, ""
}

func waitProfileInstance(
	t *testing.T, ctx context.Context, client *HTTPRuntimeClient, instanceID string,
) SceneInstance {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		instance, err := client.Scene(ctx, instanceID)
		if err != nil {
			t.Fatalf("轮询 Profile 场景失败: %v", err)
		}
		switch instance.State {
		case "running":
			return instance
		case "failed":
			t.Fatalf("Profile 场景启动失败: %s", instance.FailureReason)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("等待 Profile 场景进入 running 超时: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func stopProfileInstance(t *testing.T, client *HTTPRuntimeClient, instanceID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	instance, err := client.Scene(ctx, instanceID)
	if errors.Is(err, ErrNotFound) || (err == nil && instance.State == "stopped") {
		return
	}
	if err != nil {
		t.Errorf("清理前读取 Profile 场景失败: %v", err)
		return
	}
	if _, err := client.SceneOperation(ctx, instanceID, "stop", nil); err != nil {
		t.Errorf("清理 Profile 场景失败: %v", err)
	}
}
