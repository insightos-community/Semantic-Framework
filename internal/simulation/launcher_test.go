package simulation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecLauncherMissingBinaryIsUnavailable(t *testing.T) {
	launcher := ExecLauncher{Command: filepath.Join(t.TempDir(), "plugin-mujoco")}
	_, err := launcher.Start(context.Background())
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("缺入口应映射为 Runtime 不可用, 得到 %v", err)
	}
	if !strings.Contains(err.Error(), "入口不存在") {
		t.Fatalf("错误应指出入口不存在: %v", err)
	}
}

func TestExecLauncherEmptyCommandIsUnavailable(t *testing.T) {
	_, err := ExecLauncher{}.Start(context.Background())
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("空命令应映射为 Runtime 不可用, 得到 %v", err)
	}
}

func TestExecLauncherResolvesBinaryFromPATH(t *testing.T) {
	launcher := ExecLauncher{Command: "sleep", Args: []string{"30"}}
	process, err := launcher.Start(context.Background())
	if err != nil {
		t.Fatalf("PATH 中存在的 Runtime 入口应能启动: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := process.Stop(ctx); err != nil {
		t.Fatalf("停止测试 Runtime: %v", err)
	}
}
