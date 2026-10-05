package simulation

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestViewerSceneAcceptsContentAboveSingleObjectLimit(t *testing.T) {
	content := make([]byte, maxVisualAssetBytes+1)
	copy(content, "glTF")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "model/gltf-binary")
		_, _ = w.Write(content)
	}))
	defer server.Close()
	got, _, err := NewHTTPRuntimeClient(server.URL, server.Client()).ViewerSceneContent(context.Background(), "scene")
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("完整场景传输不应受单物体容量约束: %v", err)
	}
}

func TestViewerSceneLimitIndependentFromObjectAsset(t *testing.T) {
	if maxViewerSceneBytes <= maxVisualAssetBytes {
		t.Fatal("整场视觉内容需要独立容量，单物体限制保持原样")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "model/gltf-binary")
		w.Header().Set("Content-Length", strconv.Itoa(maxViewerSceneBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := NewHTTPRuntimeClient(server.URL, server.Client())
	_, _, err := client.ViewerSceneContent(context.Background(), "scene")
	if err == nil || !strings.Contains(err.Error(), "256 MiB") {
		t.Fatalf("超限内容应在读取前明确拒绝: %v", err)
	}
}
