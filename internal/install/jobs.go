package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Options struct {
	GeneratePreviews *bool    `json:"generate_previews,omitempty"`
	SceneIDs         []string `json:"scene_ids,omitempty"`
	Components       []string `json:"components,omitempty"`
	ProjectDefault   bool     `json:"project_default,omitempty"`
	ApplyNow         bool     `json:"apply_now,omitempty"`
	ConfirmCode      bool     `json:"confirm_code"`
	RobotID          string   `json:"robot_id,omitempty"`
	AssetRoot        string   `json:"asset_root,omitempty"`
	ModelRoot        string   `json:"model_root,omitempty"`
	LiberoRoot       string   `json:"libero_root,omitempty"`
	LiberoProRoot    string   `json:"libero_pro_root,omitempty"`
	Endpoint         string   `json:"endpoint,omitempty"`
	AcceptedLicenses []string `json:"accepted_licenses,omitempty"`
}
type Installer func(context.Context, string, Record, string, Options, func(string)) ([]string, error)

func (s *Inbox) SetInstaller(fn Installer) { s.mu.Lock(); defer s.mu.Unlock(); s.installer = fn }

func InspectRuntimeManifest(data []byte) (Package, error) {
	var header struct {
		Schema  int    `yaml:"schema_version"`
		Name    string `yaml:"pack_id"`
		Version string `yaml:"pack_version"`
	}
	if err := yaml.Unmarshal(data, &header); err != nil {
		return Package{}, err
	}
	if header.Schema != 1 || !componentSegment(header.Name) || !componentSegment(header.Version) {
		return Package{}, fmt.Errorf("Runtime Pack 清单无效")
	}
	return Package{Kind: "runtime", Name: header.Name, Version: header.Version}, nil
}

func (s *Inbox) UploadReader(ctx context.Context, projectID, filename string, reader io.Reader) (Record, error) {
	if filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, "\\:") {
		return Record{}, fmt.Errorf("文件名无效")
	}
	root, err := s.Directory(projectID)
	if err != nil {
		return Record{}, err
	}
	if err := os.MkdirAll(filepath.Join(root, ".packages"), 0700); err != nil {
		return Record{}, err
	}
	temp, err := os.CreateTemp(filepath.Join(root, ".packages"), ".upload-*")
	if err != nil {
		return Record{}, err
	}
	_ = temp.Close()
	defer os.Remove(temp.Name())
	var before os.FileInfo
	if file, ok := reader.(*os.File); ok {
		before, err = file.Stat()
		if err != nil {
			return Record{}, err
		}
	}
	id, err := CopyArchive(reader, temp.Name())
	if err != nil {
		return Record{}, err
	}
	if before != nil {
		after, err := reader.(*os.File).Stat()
		if err != nil {
			return Record{}, err
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return Record{}, fmt.Errorf("投递文件仍在写入，请完成复制后重试")
		}
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recordPath := filepath.Join(root, ".records", id+".json")
	if body, err := os.ReadFile(recordPath); err == nil {
		var item Record
		err = json.Unmarshal(body, &item)
		return item, err
	}
	item := Record{ID: id, Filename: filename, Status: "imported", UpdatedAt: time.Now().UTC()}
	archive := filepath.Join(root, ".packages", id+".zip")
	if err := os.Rename(temp.Name(), archive); err != nil {
		return item, err
	}
	pkg, parseErr := InspectArchive(archive)
	item.Package = pkg
	if parseErr == nil {
		if pkg.Kind == "scene" || pkg.Kind == "robot_skill" {
			item.Status = "importing"
			if err := saveRecord(recordPath, item); err != nil {
				return item, err
			}
			data, readErr := os.ReadFile(archive)
			if readErr != nil {
				parseErr = readErr
			} else {
				item.Resources, parseErr = s.apply(ctx, projectID, pkg, data)
			}
		} else {
			item.InstallationStatus = "pending"
		}
	}
	item.Status = "imported"
	if parseErr != nil {
		item.Status = "failed"
		item.Error = parseErr.Error()
	}
	return item, saveRecord(recordPath, item)
}

// 安装在 Server 生命周期中运行，浏览器关闭不会中断。状态持久化后才开始
// 执行代码；同一记录重复点击只返回当前状态。停止 Server 时取消子进程并等待收尾。
func (s *Inbox) Install(projectID, id string, options Options) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return Record{}, fmt.Errorf("记录 ID 无效")
	}
	if s.installer == nil {
		return Record{}, fmt.Errorf("安装服务尚未配置")
	}
	if s.closed || s.ctx.Err() != nil {
		return Record{}, fmt.Errorf("Server 正在停止，安装未开始")
	}
	root, err := s.Directory(projectID)
	if err != nil {
		return Record{}, err
	}
	path := filepath.Join(root, ".records", id+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var item Record
	if err := json.Unmarshal(body, &item); err != nil {
		return item, err
	}
	if item.Status != "imported" {
		return item, fmt.Errorf("包尚未成功导入")
	}
	if !options.ConfirmCode {
		return item, fmt.Errorf("请确认安装并运行此来源的代码和依赖")
	}
	if item.InstallationStatus == "installing" && s.active[path] != nil {
		return item, nil
	}
	if item.Kind == "package" {
		entries, err := SelectPackageEntries(item.Components, options.Components)
		if err != nil {
			return item, err
		}
		item.SelectedComponents = nil
		for _, entry := range entries {
			item.SelectedComponents = append(item.SelectedComponents, entry.ID)
		}
	}
	item.InstallationStatus = "installing"
	item.Error = ""
	item.Progress = "开始准备安装"
	item.UpdatedAt = time.Now().UTC()
	if err := saveRecord(path, item); err != nil {
		return item, err
	}
	initial := item
	ctx, cancel := context.WithCancel(s.ctx)
	s.active[path] = cancel
	installer := s.installer
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer cancel()
		progress := func(message string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			item.Progress = message
			item.UpdatedAt = time.Now().UTC()
			_ = saveRecord(path, item)
		}
		resources, err := installer(ctx, projectID, item, filepath.Join(root, ".packages", id+".zip"), options, progress)
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.active, path)
		item.Resources = resources
		item.InstallationStatus = "installed"
		item.UpdatedAt = time.Now().UTC()
		if err != nil {
			item.InstallationStatus = "failed"
			item.Error = err.Error()
		}
		_ = saveRecord(path, item)
	}()
	return initial, nil
}

func (s *Inbox) CancelInstall(projectID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(id) != 64 || !componentSegment(id) {
		return fmt.Errorf("导入记录 ID 无效")
	}
	root, err := s.Directory(projectID)
	if err != nil {
		return err
	}
	if cancel := s.active[filepath.Join(root, ".records", id+".json")]; cancel != nil {
		cancel()
		return nil
	}
	return fmt.Errorf("该记录没有进行中的安装")
}
