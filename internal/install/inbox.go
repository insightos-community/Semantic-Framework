package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"insightos.cn/semantic-framework/internal/store"
)

type Record struct {
	Package
	SelectedComponents []string  `json:"selected_components,omitempty"`
	InstallationStatus string    `json:"installation_status,omitempty"`
	Progress           string    `json:"progress,omitempty"`
	ID                 string    `json:"id"`
	Filename           string    `json:"filename"`
	Status             string    `json:"status"`
	Error              string    `json:"error,omitempty"`
	Resources          []string  `json:"resources,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Projects interface {
	GetProject(string) (store.Project, error)
	ListDevelopmentProjects() ([]store.Project, error)
}

type Importer func(context.Context, string, Package, []byte) ([]string, error)

type Inbox struct {
	mu        sync.Mutex
	projects  Projects
	apply     Importer
	scanned   map[string]os.FileInfo
	installer Installer
	ctx       context.Context
	jobs      sync.WaitGroup
	closed    bool
	active    map[string]context.CancelFunc
}

func NewInbox(projects Projects, apply Importer) *Inbox {
	return &Inbox{projects: projects, apply: apply, scanned: map[string]os.FileInfo{}, ctx: context.Background(), active: map[string]context.CancelFunc{}}
}

func (s *Inbox) Directory(projectID string) (string, error) {
	p, err := s.projects.GetProject(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(p.WorkspaceRoot, "imports"), nil
}

// Upload 与目录扫描共用 importLocked。内容摘要是项目内的幂等身份，改名上传
// 同一包仍返回原记录；已登记的源文件保留，方便复核和重新复制到其他实例。
func (s *Inbox) Upload(ctx context.Context, projectID, filename string, data []byte) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, "\\:") {
		return Record{}, errors.New("文件名无效")
	}
	root, err := s.Directory(projectID)
	if err != nil {
		return Record{}, err
	}
	return s.importLocked(ctx, projectID, root, filename, data, false)
}

func (s *Inbox) importLocked(ctx context.Context, projectID, root, filename string, data []byte, retry bool) (Record, error) {
	if len(data) > MaxPackageBytes {
		return Record{}, errors.New("安装包超过 100 MiB")
	}
	id := Digest(data)
	recordPath := filepath.Join(root, ".records", id+".json")
	if body, err := os.ReadFile(recordPath); err == nil {
		var old Record
		if err := json.Unmarshal(body, &old); err != nil {
			return Record{}, err
		}
		if !retry || old.Status != "failed" {
			return old, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}
	item := Record{ID: id, Filename: filename, UpdatedAt: time.Now().UTC(), Status: "importing"}
	info, parseErr := Inspect(data)
	item.Package = info
	if err := os.MkdirAll(filepath.Join(root, ".packages"), 0o700); err != nil {
		return item, err
	}
	if err := writeAtomic(filepath.Join(root, ".packages", id+".zip"), data); err != nil {
		return item, err
	}
	// 先持久化快照和处理中状态。进程中断后保留可见记录，由用户检查导入结果，
	// 避免自动重复创建场景草稿。成功返回表示导入完成，Robot 安装另有对账状态。
	if err := saveRecord(recordPath, item); err != nil {
		return item, err
	}
	if parseErr == nil {
		item.Resources, parseErr = s.apply(ctx, projectID, info, data)
	}
	item.Status = "imported"
	if parseErr != nil {
		item.Status, item.Error = "failed", parseErr.Error()
	}
	item.UpdatedAt = time.Now().UTC()
	return item, saveRecord(recordPath, item)
}

func (s *Inbox) List(projectID string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := s.Directory(projectID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, ".records"))
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	items := []Record{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, ".records", entry.Name()))
		if err != nil {
			return nil, err
		}
		var item Record
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		if item.InstallationStatus == "installing" && s.active[filepath.Join(root, ".records", entry.Name())] == nil {
			item.InstallationStatus = "failed"
			item.Error = "上次安装被 Server 中断，可重新安装；已有运行版本保持不变"
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	return items, nil
}

// Scan 只读取默认投递目录顶层的 ZIP。用户可先复制为 .partial，完成后改名为
// .zip；后台扫描另外等待文件最近写入结束。稳定快照检查避免导入复制中的内容。
func (s *Inbox) Scan(ctx context.Context, projectID string) error {
	root, err := s.Directory(projectID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.HasPrefix(entry.Name(), ".") || !(strings.EqualFold(filepath.Ext(entry.Name()), ".zip") || strings.HasSuffix(strings.ToLower(entry.Name()), ".tar.zst")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || time.Since(info.ModTime()) < 2*time.Second {
			continue
		}
		path := filepath.Join(root, entry.Name())
		s.mu.Lock()
		old := s.scanned[path]
		s.mu.Unlock()
		if old != nil && os.SameFile(old, info) && old.Size() == info.Size() && old.ModTime().Equal(info.ModTime()) {
			continue
		}
		// 元数据未变时无需每五秒重读大包；内容摘要仍是持久化去重的依据。
		if info.Size() > MaxUploadBytes {
			// 超限文件只登记元数据，既能在 Web 看见拒绝原因，也不复制大文件。
			// 文件修正后会重新检查；拒绝记录没有包快照，不提供原包重试。
			id := Digest([]byte(fmt.Sprintf("oversized:%s:%d:%s", entry.Name(), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))))
			item := Record{ID: id, Filename: entry.Name(), Status: "rejected", Error: "安装包超过 32 GiB，请拆分组件后重新导入", UpdatedAt: time.Now().UTC()}
			if err := saveRecord(filepath.Join(root, ".records", id+".json"), item); err != nil {
				return err
			}
			s.mu.Lock()
			s.scanned[path] = info
			s.mu.Unlock()
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, readErr := s.UploadReader(ctx, projectID, entry.Name(), file)
		after, statErr := file.Stat()
		_ = file.Close()
		if readErr != nil {
			return readErr
		}
		if statErr != nil {
			return statErr
		}
		if !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
			continue
		}
		s.mu.Lock()
		s.scanned[path] = after
		s.mu.Unlock()
	}
	return nil
}

func (s *Inbox) Retry(ctx context.Context, projectID, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return Record{}, errors.New("导入记录 ID 无效")
	}
	root, err := s.Directory(projectID)
	if err != nil {
		return Record{}, err
	}
	body, err := os.ReadFile(filepath.Join(root, ".records", id+".json"))
	if err != nil {
		return Record{}, err
	}
	var old Record
	if err := json.Unmarshal(body, &old); err != nil {
		return Record{}, err
	}
	if old.Status == "importing" {
		return Record{}, errors.New("上次导入尚未确认结束，请先检查项目资源与 Server 日志")
	}
	data, err := os.ReadFile(filepath.Join(root, ".packages", id+".zip"))
	if err != nil {
		return Record{}, err
	}
	if Digest(data) != id {
		return Record{}, errors.New("导入快照摘要不匹配")
	}
	return s.importLocked(ctx, projectID, root, old.Filename, data, true)
}

// Run 使用 Server 生命周期统一退出。只扫描开发模式项目，目录导入不因浏览器
// 是否打开而改变；每个失败包保留记录，后续周期不会自动重复导入。
func (s *Inbox) Run(ctx context.Context, report func(error)) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.closed = true
		for _, cancel := range s.active {
			cancel()
		}
		s.mu.Unlock()
		s.jobs.Wait()
	}()
	scan := func() {
		projects, err := s.projects.ListDevelopmentProjects()
		if err != nil {
			report(err)
			return
		}
		for _, p := range projects {
			if ctx.Err() != nil {
				return
			}
			if err := s.Scan(ctx, p.ID); err != nil {
				report(err)
			}
		}
	}
	scan()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		}
	}
}

// 组合包中的旧版 Scene/Skill 复用同一导入实现，避免复制其业务格式。
func (s *Inbox) ApplyPackage(ctx context.Context, projectID string, pkg Package, data []byte) ([]string, error) {
	return s.apply(ctx, projectID, pkg, data)
}

func saveRecord(path string, item Record) error {
	body, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, body)
}
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".import-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
