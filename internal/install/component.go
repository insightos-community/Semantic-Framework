package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	processport "insightos.cn/semantic-framework/internal/ports/process"
)

// Component 是安装描述，不携带任意启动命令。业务执行契约仍由原来的
// Ability/Robot Skill 清单解释；这里仅组织制品、锁定依赖和模型绑定文件。
type Component struct {
	SourceRevision     string              `yaml:"source_revision,omitempty" json:"source_revision,omitempty"`
	SchemaVersion      int                 `yaml:"schema_version" json:"schema_version"`
	Kind               string              `yaml:"kind" json:"kind"`
	Name               string              `yaml:"name" json:"name"`
	Version            string              `yaml:"version" json:"version"`
	RobotModels        []string            `yaml:"robot_models,omitempty" json:"robot_models,omitempty"`
	Python             *PythonEnvironment  `yaml:"python,omitempty" json:"python,omitempty"`
	Abilities          []AbilityComponent  `yaml:"abilities,omitempty" json:"abilities,omitempty"`
	ModelConfig        string              `yaml:"model_config,omitempty" json:"model_config,omitempty"`
	SceneCatalog       string              `yaml:"scene_catalog,omitempty" json:"scene_catalog,omitempty"`
	BundleManifest     string              `yaml:"bundle_manifest,omitempty" json:"bundle_manifest,omitempty"`
	ModelCompatibility *ModelCompatibility `yaml:"model_compatibility,omitempty" json:"model_compatibility,omitempty"`
}

// 模型组件声明消费它的接口和后端；安装器只比较声明，不解析模型动作或推理配置。
// 同一个 Ability 可支持多个后端及多个权重版本，模型名称不参与兼容性判断。
type ModelCompatibility struct {
	Role            string   `yaml:"role" json:"role"`
	AbilityName     string   `yaml:"ability_name" json:"ability_name"`
	Backend         string   `yaml:"backend" json:"backend"`
	RuntimeProfiles []string `yaml:"runtime_profiles" json:"runtime_profiles"`
}

type PythonEnvironment struct {
	Executable    string   `yaml:"executable,omitempty" json:"executable,omitempty"`
	Version       string   `yaml:"version" json:"version"`
	Requirements  string   `yaml:"requirements" json:"requirements"`
	Wheels        []string `yaml:"wheels" json:"wheels"`
	ImportModules []string `yaml:"import_modules,omitempty" json:"import_modules,omitempty"`
}

type AbilityComponent struct {
	ModelBackends []string `yaml:"model_backends,omitempty" json:"model_backends,omitempty"`
	Role          string   `yaml:"role" json:"role"`
	Template      string   `yaml:"template" json:"template"`
	AbilityName   string   `yaml:"ability_name" json:"ability_name"`
	Package       string   `yaml:"package" json:"package"`
}

type InstalledComponent struct {
	Component
	ID               string    `json:"id"`
	Root             string    `json:"root"`
	PythonExecutable string    `json:"python_executable,omitempty"`
	InstalledAt      time.Time `json:"installed_at"`
}

type ComponentStore struct {
	Root string
	mu   sync.Mutex
}

func ParseComponent(data []byte) (Component, error) {
	var c Component
	d := yaml.NewDecoder(strings.NewReader(string(data)))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("组件清单只能包含一个 YAML 文档")
	}
	if c.SchemaVersion != 1 || !componentSegment(c.Name) || !componentSegment(c.Version) {
		return c, fmt.Errorf("组件需要 schema_version=1、有效 name 和 version")
	}
	switch c.Kind {
	case "robot_ability":
		if c.Python == nil || len(c.Abilities) == 0 || len(c.RobotModels) == 0 {
			return c, fmt.Errorf("Robot Ability 需要 python、abilities 和 robot_models")
		}
	case "model":
		if c.ModelConfig == "" || len(c.RobotModels) == 0 {
			return c, fmt.Errorf("模型需要 model_config 和 robot_models")
		}
	case "scene_catalog":
		if c.SceneCatalog == "" {
			return c, fmt.Errorf("场景组件需要 scene_catalog")
		}
	case "robot_base":
		if c.BundleManifest == "" || len(c.RobotModels) == 0 {
			return c, fmt.Errorf("Robot 底座需要 bundle_manifest 和 robot_models")
		}
	default:
		return c, fmt.Errorf("未知组件类型 %s", c.Kind)
	}
	paths := []string{c.ModelConfig, c.SceneCatalog, c.BundleManifest}
	if m := c.ModelCompatibility; m != nil {
		if c.Kind != "model" || !componentSegment(m.Role) || m.AbilityName == "" || m.Backend == "" || len(m.RuntimeProfiles) == 0 {
			return c, fmt.Errorf("model_compatibility 需要模型组件、role、ability_name、backend 和 runtime_profiles")
		}
	}
	roles := map[string]bool{}
	for _, a := range c.Abilities {
		if !componentSegment(a.Role) || a.Template == "" || a.AbilityName == "" || a.Package == "" || roles[a.Role] {
			return c, fmt.Errorf("Ability 的 role、template、ability_name、package 必须明确且角色唯一")
		}
		roles[a.Role] = true
		paths = append(paths, a.Package)
	}
	if c.Python != nil {
		if c.Python.Version == "" || c.Python.Requirements == "" || len(c.Python.Wheels) == 0 {
			return c, fmt.Errorf("Python 环境需要版本、依赖锁及 Wheel")
		}
		paths = append(paths, c.Python.Requirements)
		paths = append(paths, c.Python.Executable)
		paths = append(paths, c.Python.Wheels...)
	}
	for _, path := range paths {
		if path != "" && !safeRelative(path) {
			return c, fmt.Errorf("组件路径必须位于包内: %s", path)
		}
	}
	return c, nil
}

func componentSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}
func safeRelative(s string) bool {
	return s != "" && !filepath.IsAbs(s) && filepath.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../") && !strings.ContainsAny(s, "\\:")
}

// Install 在最终版本目录建立环境，避免移动 venv 后 shebang 仍指向临时路径。
// uv 的缓存复用相同依赖 Wheel；每个组件修订保留独立环境，更新不会修改正在
// 被其他 Robot 使用的环境。安装记录只在依赖校验完成后提交。
func (s *ComponentStore) Install(ctx context.Context, archive, id string, progress func(string)) (InstalledComponent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actual, err := ArchiveDigest(archive)
	if err != nil {
		return InstalledComponent{}, err
	}
	if actual != id {
		return InstalledComponent{}, fmt.Errorf("组件摘要不一致")
	}
	root := filepath.Join(s.Root, "components", id)
	if body, err := os.ReadFile(filepath.Join(s.Root, "receipts", id+".json")); err == nil {
		var item InstalledComponent
		err = json.Unmarshal(body, &item)
		return item, err
	}
	if err := ExtractZipContext(ctx, archive, root); err != nil {
		return InstalledComponent{}, err
	}
	data, err := os.ReadFile(filepath.Join(root, "semantic-component.yaml"))
	if err != nil {
		return InstalledComponent{}, err
	}
	c, err := ParseComponent(data)
	if err != nil {
		return InstalledComponent{}, err
	}
	item := InstalledComponent{Component: c, ID: id, Root: root, InstalledAt: time.Now().UTC()}
	if c.Python != nil {
		progress("准备组件的锁定 Python 环境")
		env := filepath.Join(s.Root, "environments", id)
		if err := RunCommand(ctx, progress, "uv", "venv", "--python", c.Python.Version, "--allow-existing", env); err != nil {
			return item, err
		}
		python := filepath.Join(env, "bin", "python")
		args := []string{"pip", "sync", "--python", python, "--no-index"}
		wheelDirs := map[string]bool{}
		for _, w := range c.Python.Wheels {
			dir := filepath.Dir(filepath.Join(root, w))
			if !wheelDirs[dir] {
				args = append(args, "--find-links", dir)
				wheelDirs[dir] = true
			}
		}
		args = append(args, filepath.Join(root, c.Python.Requirements))
		if err := RunCommand(ctx, progress, "uv", args...); err != nil {
			return item, err
		}
		args = []string{"pip", "install", "--python", python, "--no-index", "--no-deps"}
		for _, w := range c.Python.Wheels {
			args = append(args, filepath.Join(root, w))
		}
		if err := RunCommand(ctx, progress, "uv", args...); err != nil {
			return item, err
		}
		if err := RunCommand(ctx, progress, "uv", "pip", "check", "--python", python); err != nil {
			return item, err
		}
		// import 名称作为数据传给 Python，不插入动态代码；触发本地库加载检查。
		if len(c.Python.ImportModules) > 0 {
			args = []string{"-c", "import importlib,sys; [importlib.import_module(name) for name in sys.argv[1:]]"}
			args = append(args, c.Python.ImportModules...)
			if err := RunCommand(ctx, progress, python, args...); err != nil {
				return item, err
			}
		}
		item.PythonExecutable = python
		if c.Python.Executable != "" {
			// 运行包保存可移植的相对入口；环境在最终安装目录准备后生成轻量入口，
			// 避免交付整个 venv 或让入口依赖构建机的绝对 shebang。
			path := filepath.Join(root, c.Python.Executable)
			if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
				return item, err
			}
			quoted := "'" + strings.ReplaceAll(python, "'", "'\"'\"'") + "'"
			bin := "'" + strings.ReplaceAll(filepath.Dir(python), "'", "'\"'\"'") + "'"
			if err := os.WriteFile(path, []byte("#!/bin/sh\nexport PATH="+bin+":\"$PATH\"\nexec "+quoted+" \"$@\"\n"), 0750); err != nil {
				return item, err
			}
		}
	}
	for _, a := range c.Abilities {
		if _, err := os.Stat(filepath.Join(root, a.Package)); err != nil {
			return item, err
		}
	}
	for _, p := range []string{c.ModelConfig, c.SceneCatalog, c.BundleManifest} {
		if p != "" {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				return item, err
			}
		}
	}
	body, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return item, err
	}
	if err := ctx.Err(); err != nil {
		return item, err
	}
	// 回执独立于上传内容，失败后重试不会把包中同名文件误当成安装成功证据。
	return item, writeAtomic(filepath.Join(s.Root, "receipts", id+".json"), body)
}

func RunCommand(ctx context.Context, progress func(string), command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	log := &commandProgress{report: progress}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 30 * time.Second
	// uv 等安装命令会创建子进程，取消整个进程组才能避免 Server 退出后仍写环境。
	var tree *processport.Tree
	cmd.Cancel = func() error {
		if tree == nil {
			return nil
		}
		return tree.Terminate()
	}
	var err error
	if tree, err = processport.Start(cmd); err != nil {
		return fmt.Errorf("%s 启动失败: %w", command, err)
	}
	defer func() { _ = tree.Close() }()
	runErr := cmd.Wait()
	if ctx.Err() != nil && cmd.Process != nil {
		// WaitDelay 到期只会强制终止主进程，清理同组遗留子进程后才结束取消。
		_ = tree.Kill()
	}
	output := log.tail
	if len(output) > 0 {
		progress(strings.TrimSpace(string(output)))
	}
	if runErr != nil {
		return fmt.Errorf("%s 执行失败: %w: %s", command, runErr, output)
	}
	return nil
}

// 子进程输出始终有界；下载或编译时间较长时，Web 仍能看到最近进度。
// stdout/stderr 由 os/exec 对同一个 Writer 顺序写入，不持有安装目录锁。
type commandProgress struct {
	tail   []byte
	last   time.Time
	report func(string)
}

func (w *commandProgress) Write(data []byte) (int, error) {
	w.tail = append(w.tail, data...)
	if len(w.tail) > 6000 {
		w.tail = w.tail[len(w.tail)-6000:]
	}
	if time.Since(w.last) >= time.Second {
		w.last = time.Now()
		w.report(strings.TrimSpace(string(w.tail)))
	}
	return len(data), nil
}
