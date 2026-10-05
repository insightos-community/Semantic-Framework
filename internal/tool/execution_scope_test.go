package tool

import (
	"context"
	"testing"
)

// TestExecutionScope 验证执行边界只随当前 context 传播，空 context 不会
// 意外获得其他会话的工作区权限。
func TestExecutionScope(t *testing.T) {
	if _, ok := ExecutionScopeFromContext(context.Background()); ok {
		t.Fatal("空 context 不应包含执行边界")
	}

	want := ExecutionScope{SessionID: "session-1", ProjectID: "project-1", WorkspaceRoot: "/tmp/project-1"}
	ctx := WithExecutionScope(context.Background(), want)
	got, ok := ExecutionScopeFromContext(ctx)
	if !ok || got != want {
		t.Fatalf("执行边界读取不一致: got=%+v ok=%v want=%+v", got, ok, want)
	}
}
