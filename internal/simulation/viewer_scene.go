package simulation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
)

// 整座原生场景包含多物体几何和材质，容量独立于单个 VisualAsset 的 64 MiB。
// 仍限制单次读取内存，避免将大型场景拆成用户必须手动导入的多个模型。
const maxViewerSceneBytes = 256 << 20

// RuntimeViewerSceneClient 是无需服务端会话的 GLB + Pose Stream 能力。
// 采用可选接口，未升级的 remote Runtime 会明确报告不支持，而不会退回 JPEG。
type RuntimeViewerSceneClient interface {
	ViewerScene(context.Context, string) (ViewerScene, error)
	ViewerSceneContent(context.Context, string) ([]byte, string, error)
	ScenePoseURL(string) string
}

func (c *HTTPRuntimeClient) ViewerScene(
	ctx context.Context, instanceID string,
) (ViewerScene, error) {
	var result ViewerScene
	path := "/api/v1/scene-instances/" + url.PathEscape(instanceID) + "/viewer-scene"
	return result, c.do(ctx, http.MethodGet, path, nil, &result)
}

func (c *HTTPRuntimeClient) ViewerSceneContent(
	ctx context.Context, instanceID string,
) ([]byte, string, error) {
	path := "/api/v1/scene-instances/" + url.PathEscape(instanceID) +
		"/viewer-scene/content"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, "", fmt.Errorf("创建 Viewer Scene 请求失败: %w", err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, "", fmt.Errorf("Runtime Viewer Scene 请求失败 %s: %s",
			response.Status, string(data))
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "model/gltf-binary" {
		return nil, "", fmt.Errorf("Runtime Viewer Scene Content-Type 无效: %q",
			response.Header.Get("Content-Type"))
	}
	if response.ContentLength > maxViewerSceneBytes {
		return nil, "", fmt.Errorf("Runtime Viewer Scene 超过 256 MiB 上限")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maxViewerSceneBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取 Viewer Scene 失败: %w", err)
	}
	if len(content) > maxViewerSceneBytes || len(content) < 12 ||
		!bytes.Equal(content[:4], []byte("glTF")) {
		return nil, "", fmt.Errorf("Runtime Viewer Scene 不是有效的受限 GLB")
	}
	return content, mediaType, nil
}

func (c *HTTPRuntimeClient) ScenePoseURL(instanceID string) string {
	return c.streamURL("/api/v1/scene-instances/" + url.PathEscape(instanceID) + "/pose-stream")
}

func (s *Service) ViewerScene(
	ctx context.Context, projectID, instanceID string,
) (ViewerScene, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return ViewerScene{}, err
	}
	client, ok := binding.Client.(RuntimeViewerSceneClient)
	if !ok {
		return ViewerScene{}, fmt.Errorf("%w: 当前 Runtime 不提供 GLB Physics Viewer",
			ErrNotFound)
	}
	result, err := client.ViewerScene(ctx, state.InstanceID)
	if err != nil {
		return ViewerScene{}, err
	}
	if state.LastInstance == nil || result.Generation != state.LastInstance.Generation {
		return ViewerScene{}, fmt.Errorf("%w: Viewer Scene generation 已失效", ErrConflict)
	}
	base := "/api/v1/projects/" + url.PathEscape(projectID) + "/simulation/instances/" +
		url.PathEscape(instanceID) + "/viewer-scene"
	result.ContentURL = base + "/content"
	result.PoseStreamURL = "/ws/simulation-stream?kind=pose&project_id=" +
		url.QueryEscape(projectID) + "&instance_id=" + url.QueryEscape(instanceID)
	return result, nil
}

func (s *Service) ViewerSceneContent(
	ctx context.Context, projectID, instanceID string,
) ([]byte, string, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return nil, "", err
	}
	client, ok := binding.Client.(RuntimeViewerSceneClient)
	if !ok {
		return nil, "", fmt.Errorf("%w: 当前 Runtime 不提供 GLB Physics Viewer",
			ErrNotFound)
	}
	return client.ViewerSceneContent(ctx, state.InstanceID)
}

func (s *Service) ScenePoseURL(projectID, instanceID string) (string, error) {
	state, binding, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return "", err
	}
	client, ok := binding.Client.(RuntimeViewerSceneClient)
	if !ok {
		return "", fmt.Errorf("%w: 当前 Runtime 不提供 Scene Pose Stream", ErrNotFound)
	}
	return client.ScenePoseURL(state.InstanceID), nil
}
