package bootstrap

import (
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/robotruntime"
)

// TestResolveReadinessPrecedence 固定就绪超时的优先级语义：
// 显式配置优先并精确生效；未显式配置时，取内置默认值与包声明中的较大者，
// 保证包声明的更长收敛时间能生效，而包声明的较短值不会误判原本能起来
// 的 Robot 为超时。
func TestResolveReadinessPrecedence(t *testing.T) {
	for _, test := range []struct {
		name     string
		override time.Duration
		bundle   time.Duration
		want     time.Duration
	}{
		{"无覆盖无声明用默认值", 0, 0, managedRobotReadinessTimeout},
		{"包声明更长时放宽", 0, 10 * time.Minute, 10 * time.Minute},
		{"包声明更短时不收紧", 0, 30 * time.Second, managedRobotReadinessTimeout},
		{"显式覆盖优先于更长包声明", 30 * time.Second, 10 * time.Minute, 30 * time.Second},
		{"显式覆盖优先于默认值", 8 * time.Minute, 0, 8 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher := &managedRobotInstanceLauncher{
				readiness:         managedRobotReadinessTimeout,
				readinessOverride: test.override,
			}
			got := launcher.resolveReadiness(robotruntime.Bundle{ReadinessTimeout: test.bundle})
			if got != test.want {
				t.Fatalf("resolveReadiness 应为 %s，实际: %s", test.want, got)
			}
		})
	}
}
