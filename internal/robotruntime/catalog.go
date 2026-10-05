package robotruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Catalog 按 Robot 型号、后端和后端配置精确选择运行 Bundle。
// 同一型号可以同时安装 Fake、MuJoCo 和真机 Bundle，但不能靠注册顺序决定。
type Catalog struct {
	mu      sync.RWMutex
	bundles []Bundle
}

type bundleManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
	} `yaml:"metadata"`
	Spec struct {
		Robot struct {
			Model           string `yaml:"model"`
			BackendProfiles []struct {
				Backend string `yaml:"backend"`
				Profile string `yaml:"profile"`
			} `yaml:"backendProfiles"`
		} `yaml:"robot"`
		Runtime struct {
			ReadinessTimeout string `yaml:"readinessTimeout"`
		} `yaml:"runtime"`
	} `yaml:"spec"`
}

// LoadCatalog 从只读 Bundle Store 加载类型包。Framework 只读取包身份和
// model/backend/profile 匹配条件；进程路径、Wheel 和 Ability 清单仍由
// semantic-robot-instance 解释，避免 Server 再复制一套部署格式。
func LoadCatalog(root string) (*Catalog, error) {
	return loadCatalog(root, true)
}

func loadCatalog(root string, readIndex bool) (*Catalog, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("Robot Runtime Bundle Store 路径必填")
	}
	var bundles []Bundle
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "bundle.yaml" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var manifest bundleManifest
		if err := yaml.Unmarshal(content, &manifest); err != nil {
			return fmt.Errorf("解析 %s: %w", path, err)
		}
		if manifest.Kind != "RobotRuntimeBundle" || strings.TrimSpace(manifest.Metadata.Name) == "" ||
			strings.TrimSpace(manifest.Metadata.Version) == "" || strings.TrimSpace(manifest.Spec.Robot.Model) == "" {
			return fmt.Errorf("%s 不是有效 RobotRuntimeBundle", path)
		}
		// 就绪超时由包自己声明：加载大模型的 Bundle 冷启动明显慢于仿真包。
		// 声明非法时按未声明处理（回退默认值），不因可选字段使整包加载失败。
		readiness := time.Duration(0)
		if raw := strings.TrimSpace(manifest.Spec.Runtime.ReadinessTimeout); raw != "" {
			if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
				readiness = parsed
			}
		}
		for _, profile := range manifest.Spec.Robot.BackendProfiles {
			bundles = append(bundles, Bundle{Name: manifest.Metadata.Name, Version: manifest.Metadata.Version,
				Path: filepath.Dir(path), ReadinessTimeout: readiness, Match: MatchKey{RobotModel: manifest.Spec.Robot.Model,
					Backend: profile.Backend, BackendProfile: profile.Profile}})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("加载 Robot Runtime Bundle Store: %w", err)
	}
	if !readIndex {
		return NewCatalog(bundles...)
	}
	if content, err := os.ReadFile(filepath.Join(root, "installed-bundles.json")); err == nil {
		var installed map[string]string
		if err := json.Unmarshal(content, &installed); err != nil {
			return nil, err
		}
		for name, path := range installed {
			// 安装目录固定在组件摘要目录下；旧版本仍留在原处供运行实例与回退使用。
			next, err := loadCatalog(path, false)
			if err != nil {
				return nil, err
			}
			kept := bundles[:0]
			for _, b := range bundles {
				if b.Name != name {
					kept = append(kept, b)
				}
			}
			bundles = kept
			for _, b := range next.bundles {
				if b.Name != name {
					return nil, fmt.Errorf("安装索引与 Bundle 名称不匹配")
				}
				bundles = append(bundles, b)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return NewCatalog(bundles...)
}

func RegisterInstalledBundle(root, path string) error {
	next, err := loadCatalog(path, false)
	if err != nil {
		return err
	}
	if len(next.bundles) == 0 {
		return fmt.Errorf("组件中没有 RobotRuntimeBundle")
	}
	if err := os.MkdirAll(root, 0750); err != nil {
		return err
	}
	index := filepath.Join(root, "installed-bundles.json")
	installed := map[string]string{}
	if data, err := os.ReadFile(index); err == nil {
		if err := json.Unmarshal(data, &installed); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, b := range next.bundles {
		installed[b.Name] = path
	}
	body, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".bundles-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), index)
}

func NewCatalog(bundles ...Bundle) (*Catalog, error) {
	result := &Catalog{}
	for _, bundle := range bundles {
		if err := result.Register(bundle); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (c *Catalog) Register(bundle Bundle) error {
	if strings.TrimSpace(bundle.Name) == "" || strings.TrimSpace(bundle.Version) == "" {
		return fmt.Errorf("Runtime Bundle name 和 version 必填")
	}
	if err := validateMatch(bundle.Match); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.bundles {
		if existing.Name == bundle.Name && existing.Version == bundle.Version &&
			existing.Match == bundle.Match {
			return fmt.Errorf("Runtime Bundle %s@%s 已注册", bundle.Name, bundle.Version)
		}
	}
	c.bundles = append(c.bundles, cloneBundle(bundle))
	return nil
}

func (c *Catalog) Resolve(match MatchKey) (Bundle, error) {
	if err := validateMatch(match); err != nil {
		return Bundle{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result *Bundle
	for _, bundle := range c.bundles {
		if bundle.Match != match {
			continue
		}
		if result != nil {
			return Bundle{}, fmt.Errorf("%w: %+v", ErrBundleAmbiguous, match)
		}
		item := cloneBundle(bundle)
		result = &item
	}
	if result == nil {
		return Bundle{}, fmt.Errorf("%w: %+v", ErrBundleNotFound, match)
	}
	return *result, nil
}

// ResolveDescriptor 是 Server Orchestrator 的正式入口，必须完整匹配
// Model、Backend 和 BackendProfile。
func (c *Catalog) ResolveDescriptor(descriptor VirtualRobotDescriptor) (Bundle, error) {
	return c.Resolve(descriptor.MatchKey())
}
func validateMatch(match MatchKey) error {
	if strings.TrimSpace(match.RobotModel) == "" || strings.TrimSpace(match.Backend) == "" ||
		strings.TrimSpace(match.BackendProfile) == "" {
		return fmt.Errorf("Runtime Bundle 匹配需要 robot_model、backend 和 backend_profile")
	}
	return nil
}

func cloneBundle(source Bundle) Bundle {
	return source
}
