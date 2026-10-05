package pilot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrArtifactNotAuthorized        = errors.New("Artifact 未授权给当前 Execution")
	ErrArtifactPathOutsideWorkspace = errors.New("发布文件不在当前 Execution workspace")
)

type artifactDownload struct {
	Ref       string
	URL       string
	MediaType string
	Summary   string
}

type localArtifact struct {
	LocalID     string `json:"local_artifact_id"`
	ExecutionID string `json:"execution_id"`
	Path        string `json:"path"`
	MediaType   string `json:"media_type"`
	Summary     string `json:"summary"`
	SizeBytes   int64  `json:"size_bytes"`
	ServerRef   string `json:"server_ref,omitempty"`
	SyncStatus  string `json:"sync_status"`
}

// ArtifactStore 是设备侧独立 Artifact 空间。它只保存本地路径与 Server
// 引用映射；二进制通过 HTTP Bridge 传输，不进入 Worker JSON-RPC。
type ArtifactStore struct {
	BaseDirectory            string
	AbilityExchangeDirectory string
	ServerBaseURL            string
	PilotInstanceID          string
	AccessToken              string
	HTTPClient               *http.Client
	Announce                 func(execution SkillExecution, artifact localArtifact) error

	mu         sync.RWMutex
	authorized map[string]map[string]artifactDownload
	local      map[string]localArtifact
}

func NewArtifactStore(baseDirectory, serverBaseURL string) *ArtifactStore {
	return &ArtifactStore{BaseDirectory: baseDirectory, ServerBaseURL: strings.TrimRight(serverBaseURL, "/"),
		HTTPClient: &http.Client{Timeout: 5 * time.Minute}, authorized: make(map[string]map[string]artifactDownload), local: make(map[string]localArtifact)}
}

func (s *ArtifactStore) Workspace(execution SkillExecution) (string, error) {
	root := filepath.Join(s.BaseDirectory, "executions", safePilotSegment(execution.ID), "workspace")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", err
	}
	return root, nil
}

func (s *ArtifactStore) Authorize(executionID string, items []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed := make(map[string]artifactDownload, len(items))
	for _, item := range items {
		ref, _ := item["ref"].(string)
		url, _ := item["url"].(string)
		if ref != "" && url != "" {
			allowed[ref] = artifactDownload{Ref: ref, URL: url, MediaType: stringValue(item["media_type"]), Summary: stringValue(item["summary"])}
		}
	}
	s.authorized[executionID] = allowed
}

func (s *ArtifactStore) Resolve(ctx context.Context, execution SkillExecution, ref string) (map[string]any, error) {
	s.mu.RLock()
	item, ok := s.authorized[execution.ID][ref]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrArtifactNotAuthorized
	}
	cache := filepath.Join(s.BaseDirectory, "cache", "server", safePilotSegment(strings.TrimPrefix(ref, "artifact://")))
	if info, err := os.Stat(cache); err == nil && info.Mode().IsRegular() {
		_ = os.Chmod(cache, 0o440)
		return map[string]any{"ref": ref, "local_path": cache, "media_type": item.MediaType, "size_bytes": info.Size(), "summary": item.Summary}, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.absoluteURL(item.URL), nil)
	if err != nil {
		return nil, err
	}
	s.authorizeRequest(request)
	response, err := s.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 Artifact 失败: HTTP %d", response.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o750); err != nil {
		return nil, err
	}
	temp := cache + ".partial-" + uuid.NewString()
	output, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	size, copyErr := io.Copy(output, response.Body)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(temp)
		return nil, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temp)
		return nil, closeErr
	}
	if err := os.Rename(temp, cache); err != nil {
		_ = os.Remove(temp)
		return nil, err
	}
	_ = os.Chmod(cache, 0o440)
	mediaType := item.MediaType
	if mediaType == "" {
		mediaType = response.Header.Get("Content-Type")
	}
	return map[string]any{"ref": ref, "local_path": cache, "media_type": mediaType, "size_bytes": size, "summary": item.Summary}, nil
}

func (s *ArtifactStore) Publish(_ context.Context, execution SkillExecution, path, mediaType, summary string) (map[string]any, error) {
	workspace, err := s.Workspace(execution)
	if err != nil {
		return nil, err
	}
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(realWorkspace, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, ErrArtifactPathOutsideWorkspace
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrArtifactPathOutsideWorkspace
	}
	if mediaType == "" {
		mediaType = mime.TypeByExtension(filepath.Ext(realPath))
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	s.mu.RLock()
	for _, current := range s.local {
		if current.ExecutionID == execution.ID && current.Path == realPath {
			s.mu.RUnlock()
			return map[string]any{
				"local_ref":   s.localRef(current.LocalID),
				"server_ref":  current.ServerRef,
				"sync_status": current.SyncStatus,
			}, nil
		}
	}
	s.mu.RUnlock()
	item := localArtifact{LocalID: "pla-" + uuid.NewString(), ExecutionID: execution.ID, Path: realPath, MediaType: mediaType, Summary: summary, SizeBytes: info.Size(), SyncStatus: "pending"}
	s.mu.Lock()
	s.local[item.LocalID] = item
	s.mu.Unlock()
	if err := s.persist(item); err != nil {
		return nil, err
	}
	if s.Announce != nil {
		if err := s.Announce(execution, item); err != nil {
			item.SyncStatus = "failed"
			s.update(item)
			return nil, err
		}
	}
	return map[string]any{"local_ref": s.localRef(item.LocalID), "sync_status": item.SyncStatus}, nil
}

func resolveAbilityExchangeFile(root, exchangePath string) (string, error) {
	cleaned := filepath.Clean(exchangePath)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", ErrArtifactPathOutsideWorkspace
	}
	source, err := fileInsideExchangeRoot(root, filepath.Join(root, cleaned))
	if err == nil {
		return source, nil
	}
	if !exchangeFileMissing(err) {
		return "", err
	}
	// 已安装的旧 Ability 仍会把 Framework 实例 UUID 套在共享根下，却返回
	// 相对于该子目录的 captures/... 句柄。这里只多看一层直接子目录，
	// 仍拒绝越出交换根的符号链接和 `..`。
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate, candidateErr := fileInsideExchangeRoot(root, filepath.Join(root, entry.Name(), cleaned))
		if candidateErr != nil {
			if errors.Is(candidateErr, ErrArtifactPathOutsideWorkspace) {
				return "", candidateErr
			}
			continue
		}
		if found != "" && found != candidate {
			return "", fmt.Errorf("Ability 交换目录存在多个同名证据: %s", cleaned)
		}
		found = candidate
	}
	if found == "" {
		return "", err
	}
	return found, nil
}

func fileInsideExchangeRoot(root, candidate string) (string, error) {
	source, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, source)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", ErrArtifactPathOutsideWorkspace
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", ErrArtifactPathOutsideWorkspace
	}
	return source, nil
}

func exchangeFileMissing(err error) bool {
	return err != nil && os.IsNotExist(err)
}

func (s *ArtifactStore) ImportAbilityArtifact(ctx context.Context, execution SkillExecution, exchangePath, mediaType, summary string) (map[string]any, error) {
	// 交换目录不属于 Worker workspace，所以不能放宽 Publish 的路径边界。
	// 这里先校验相对句柄确实位于受管交换根目录，再复制到当前 Execution
	// workspace，随后完全复用 Publish→announce→HTTP upload 链路。
	if s.AbilityExchangeDirectory == "" || exchangePath == "" || filepath.IsAbs(exchangePath) {
		return nil, ErrArtifactPathOutsideWorkspace
	}
	root, err := filepath.EvalSymlinks(s.AbilityExchangeDirectory)
	if err != nil {
		return nil, err
	}
	source, err := resolveAbilityExchangeFile(root, exchangePath)
	if err != nil {
		return nil, err
	}
	workspace, err := s.Workspace(execution)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(exchangePath))
	extension := filepath.Ext(source)
	target := filepath.Join(workspace, "ability-evidence", fmt.Sprintf("%x%s", digest[:12], extension))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		input, openErr := os.Open(source)
		if openErr != nil {
			return nil, openErr
		}
		temporary := target + ".partial-" + uuid.NewString()
		output, createErr := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			input.Close()
			return nil, createErr
		}
		_, copyErr := io.Copy(output, input)
		inputCloseErr, outputCloseErr := input.Close(), output.Close()
		if copyErr != nil || inputCloseErr != nil || outputCloseErr != nil {
			_ = os.Remove(temporary)
			return nil, errors.Join(copyErr, inputCloseErr, outputCloseErr)
		}
		if err := os.Rename(temporary, target); err != nil {
			_ = os.Remove(temporary)
			return nil, err
		}
	}
	return s.Publish(ctx, execution, target, mediaType, summary)
}

func (s *ArtifactStore) Upload(ctx context.Context, localID, uploadURL string) (map[string]any, error) {
	s.mu.RLock()
	item, ok := s.local[localID]
	s.mu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	file, err := os.Open(item.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, s.absoluteURL(uploadURL), file)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", item.MediaType)
	s.authorizeRequest(request)
	response, err := s.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上传 Artifact 失败: HTTP %d", response.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	item.ServerRef = stringValue(result["server_ref"])
	item.SyncStatus = stringValue(result["sync_status"])
	s.update(item)
	return map[string]any{"local_ref": s.localRef(localID), "server_ref": item.ServerRef, "sync_status": item.SyncStatus}, nil
}

func (s *ArtifactStore) localRef(localID string) string {
	return "pilot-artifact://" + s.PilotInstanceID + "/" + localID
}

func (s *ArtifactStore) Get(localID string) (localArtifact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.local[localID]
	return item, ok
}
func (s *ArtifactStore) update(item localArtifact) {
	s.mu.Lock()
	s.local[item.LocalID] = item
	s.mu.Unlock()
	_ = s.persist(item)
}
func (s *ArtifactStore) persist(item localArtifact) error {
	directory := filepath.Join(s.BaseDirectory, "artifacts", "metadata")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, safePilotSegment(item.LocalID)+".json"), body, 0o640)
}
func (s *ArtifactStore) authorizeRequest(request *http.Request) {
	if s.AccessToken != "" {
		request.Header.Set("Authorization", "Bearer "+s.AccessToken)
	}
}

func (s *ArtifactStore) absoluteURL(value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return s.ServerBaseURL + "/" + strings.TrimLeft(value, "/")
}
func safePilotSegment(value string) string {
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, "\\", "_")
	return value
}
