package simulation

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLifecycleTimeoutDoesNotExtendStatusQueries(t *testing.T) {
	client := NewHTTPRuntimeClient("http://runtime", nil)
	var budgets []time.Duration
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("请求必须有明确预算")
		}
		budgets = append(budgets, time.Until(deadline))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	client.client.Transport = transport
	client.lifecycleClient.Transport = transport
	if _, err := client.Scene(context.Background(), "scene"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SceneOperation(context.Background(), "scene", "reset", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SceneOperation(context.Background(), "scene", "stop", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SceneOperation(context.Background(), "scene", "pause", nil); err != nil {
		t.Fatal(err)
	}
	for i, expected := range []time.Duration{15 * time.Second, 90 * time.Second, 90 * time.Second, 15 * time.Second} {
		if budgets[i] > expected || budgets[i] < expected-time.Second {
			t.Fatalf("请求 %d 预算错误: %v", i, budgets[i])
		}
	}
}

func TestExplicitRuntimeHTTPClientPreservesItsTimeout(t *testing.T) {
	custom := &http.Client{Timeout: 2 * time.Second}
	client := NewHTTPRuntimeClient("http://runtime", custom)
	if client.client != custom || client.lifecycleClient != custom {
		t.Fatal("显式注入的客户端应保留调用方预算")
	}
}
