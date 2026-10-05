package simulation

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var runtimeInstallationIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

const runtimeInstallationSchemaVersion = 2

// RuntimeInstallation 描述一套已经安装或可远程连接的 Runtime。Profile 说明
// “能做什么”，Installation 说明“这台 Server 怎样启动或连接它”。
//
// Command/Workdir/Endpoint 只在 Server 内使用，公共接口必须返回 Public() 的脱敏
// 视图。命令始终以 argv 直接执行，不经过 shell；环境变量也只能按名称引用。
type RuntimeInstallation struct {
	SettingsPath         string            `yaml:"settings_path,omitempty" json:"-"`
	SchemaVersion        int               `yaml:"schema_version" json:"schema_version"`
	InstallationID       string            `yaml:"installation_id" json:"installation_id"`
	Profile              RuntimeProfile    `yaml:"profile" json:"profile"`
	PackID               string            `yaml:"pack_id,omitempty" json:"pack_id,omitempty"`
	PackVersion          string            `yaml:"pack_version,omitempty" json:"pack_version,omitempty"`
	Runner               string            `yaml:"runner,omitempty" json:"runner,omitempty"`
	LaunchMode           string            `yaml:"launch_mode" json:"launch_mode"`
	EnvironmentPath      string            `yaml:"environment_path,omitempty" json:"-"`
	PackPath             string            `yaml:"pack_path,omitempty" json:"-"`
	SceneCatalogPath     string            `yaml:"scene_catalog_path,omitempty" json:"-"`
	ContentRefs          map[string]string `yaml:"content_refs,omitempty" json:"-"`
	AcceptedLicenses     []string          `yaml:"accepted_licenses,omitempty" json:"-"`
	Development          bool              `yaml:"development,omitempty" json:"development,omitempty"`
	Endpoint             string            `yaml:"endpoint" json:"-"`
	Workdir              string            `yaml:"workdir" json:"-"`
	Image                string            `yaml:"image" json:"image,omitempty"`
	Command              []string          `yaml:"command" json:"-"`
	EnvironmentRefs      map[string]string `yaml:"environment_refs" json:"-"`
	AssetDataMounts      []RuntimeMount    `yaml:"asset_data_mounts" json:"asset_data_mounts,omitempty"`
	HardwareRequirements map[string]string `yaml:"hardware_requirements" json:"hardware_requirements,omitempty"`
	Enabled              bool              `yaml:"enabled" json:"enabled"`
	InstalledVersion     string            `yaml:"installed_version" json:"installed_version,omitempty"`
	Status               string            `yaml:"-" json:"status"`
	Diagnostic           string            `yaml:"-" json:"diagnostic,omitempty"`
	SourceFile           string            `yaml:"-" json:"-"`
}

// RuntimeMount 只声明已由管理员准备的挂载。Framework 不创建、下载或复制资产。
type RuntimeMount struct {
	Kind     string `yaml:"kind" json:"kind"`
	Source   string `yaml:"source" json:"-"`
	Target   string `yaml:"target" json:"target"`
	ReadOnly bool   `yaml:"read_only" json:"read_only"`
}

// RuntimeInstallationView 是 Studio 可以安全查看的安装信息。
type RuntimeInstallationView struct {
	InstallationID       string            `json:"installation_id"`
	ProfileID            string            `json:"profile_id"`
	PackID               string            `json:"pack_id,omitempty"`
	PackVersion          string            `json:"pack_version,omitempty"`
	Development          bool              `json:"development,omitempty"`
	Name                 string            `json:"name"`
	Engine               string            `json:"engine"`
	Loader               string            `json:"loader"`
	LaunchMode           string            `json:"launch_mode"`
	InstalledVersion     string            `json:"installed_version,omitempty"`
	Enabled              bool              `json:"enabled"`
	Status               string            `json:"status"`
	Diagnostic           string            `json:"diagnostic,omitempty"`
	HardwareRequirements map[string]string `json:"hardware_requirements,omitempty"`
	Capabilities         RuntimeCapability `json:"capabilities"`
}

func (i RuntimeInstallation) Public() RuntimeInstallationView {
	return RuntimeInstallationView{
		InstallationID: i.InstallationID, ProfileID: i.Profile.RuntimeProfileID,
		PackID: i.PackID, PackVersion: i.PackVersion, Development: i.Development,
		Name: i.Profile.Name, Engine: i.Profile.Engine, Loader: i.Profile.Loader,
		LaunchMode: i.LaunchMode, InstalledVersion: i.InstalledVersion,
		Enabled: i.Enabled, Status: i.Status, Diagnostic: i.Diagnostic,
		HardwareRequirements: cloneStrings(i.HardwareRequirements),
		Capabilities:         cloneRuntimeCapability(i.Profile.Capabilities),
	}
}

// RuntimeInstallationCatalog 是启动期加载的不可变安装清单。
type RuntimeInstallationCatalog struct {
	items    map[string]RuntimeInstallation
	mu       sync.RWMutex
	order    []string
	observed map[string]runtimeInstallationObservation
}

type runtimeInstallationObservation struct {
	status     string
	diagnostic string
}

// LoadRuntimeInstallationFile 严格解析一个管理员提供的安装清单。CLI 与
// Server 启动共用同一入口，防止“注册成功但 Server 无法加载”。函数只解析
// 固定 YAML 结构，不执行 command，也不安装 Python/容器依赖。
func LoadRuntimeInstallationFile(path string) (RuntimeInstallation, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return RuntimeInstallation{}, fmt.Errorf("读取 Runtime 安装清单失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return RuntimeInstallation{}, errors.New("Runtime 安装清单必须是普通文件，不能是目录或符号链接")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RuntimeInstallation{}, fmt.Errorf("读取 Runtime 安装清单失败: %w", err)
	}
	return parseRuntimeInstallation(data, path)
}

func parseRuntimeInstallation(data []byte, sourceFile string) (RuntimeInstallation, error) {
	var installation RuntimeInstallation
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&installation); err != nil {
		return RuntimeInstallation{}, fmt.Errorf("解析 Runtime 安装清单失败: %w", err)
	}
	installation.SourceFile = sourceFile
	if err := validateRuntimeInstallation(&installation); err != nil {
		return RuntimeInstallation{}, fmt.Errorf("Runtime 安装清单无效: %w", err)
	}
	return installation, nil
}

// LoadRuntimeInstallationsFS 读取编译期内置的默认清单。它与磁盘加载使用同一
// 严格解析器，但 SourceFile 留空，因此系统设置只能查看和诊断，不能修改内置
// 模板。semantic init 安装后的磁盘清单仍由 LoadRuntimeInstallations 管理。
func LoadRuntimeInstallationsFS(fsys fs.FS, dir string) (*RuntimeInstallationCatalog, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("读取内置 Runtime 安装清单目录失败: %w", err)
	}
	catalog := &RuntimeInstallationCatalog{items: map[string]RuntimeInstallation{}}
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yaml" &&
			filepath.Ext(entry.Name()) != ".yml") {
			continue
		}
		path := filepath.ToSlash(filepath.Join(dir, entry.Name()))
		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return nil, fmt.Errorf("读取内置 Runtime 安装清单 %s 失败: %w", path, readErr)
		}
		installation, parseErr := parseRuntimeInstallation(data, "")
		if parseErr != nil {
			return nil, fmt.Errorf("内置 Runtime 安装清单 %s 无效: %w", path, parseErr)
		}
		if _, exists := catalog.items[installation.InstallationID]; exists {
			return nil, fmt.Errorf("Runtime installation_id 重复: %s", installation.InstallationID)
		}
		catalog.items[installation.InstallationID] = installation
		catalog.order = append(catalog.order, installation.InstallationID)
	}
	sort.Strings(catalog.order)
	if len(catalog.order) == 0 {
		return nil, errors.New("内置 Runtime 安装清单目录中没有 YAML 文件")
	}
	return catalog, nil
}
func LoadRuntimeInstallations(dir string) (*RuntimeInstallationCatalog, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取 Runtime 安装清单目录失败: %w", err)
	}
	catalog := &RuntimeInstallationCatalog{items: map[string]RuntimeInstallation{}}
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yaml" &&
			filepath.Ext(entry.Name()) != ".yml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		installation, loadErr := LoadRuntimeInstallationFile(path)
		if loadErr != nil {
			return nil, fmt.Errorf("Runtime 安装清单 %s 无效: %w", path, loadErr)
		}
		if _, exists := catalog.items[installation.InstallationID]; exists {
			return nil, fmt.Errorf("Runtime installation_id 重复: %s", installation.InstallationID)
		}
		catalog.items[installation.InstallationID] = installation
		catalog.order = append(catalog.order, installation.InstallationID)
	}
	sort.Strings(catalog.order)
	return catalog, nil
}

func validateRuntimeInstallation(item *RuntimeInstallation) error {
	item.InstallationID = strings.TrimSpace(item.InstallationID)
	item.LaunchMode = strings.TrimSpace(item.LaunchMode)
	item.Endpoint = strings.TrimRight(strings.TrimSpace(item.Endpoint), "/")
	item.Workdir = resolveEnvReference(strings.TrimSpace(item.Workdir))
	if item.SchemaVersion != 1 && item.SchemaVersion != runtimeInstallationSchemaVersion {
		return fmt.Errorf("schema_version 只支持 1 或 %d", runtimeInstallationSchemaVersion)
	}
	if item.InstallationID == "" || item.Profile.RuntimeProfileID == "" ||
		item.Profile.Engine == "" || item.Profile.Loader == "" {
		return errors.New("installation_id、profile_id、engine 和 loader 不能为空")
	}
	if !runtimeInstallationIDPattern.MatchString(item.InstallationID) ||
		!runtimeInstallationIDPattern.MatchString(item.Profile.RuntimeProfileID) {
		return errors.New("installation_id 和 profile_id 只能使用小写字母、数字、点、下划线与连字符")
	}
	if item.SchemaVersion == runtimeInstallationSchemaVersion {
		if item.PackID == "" || item.PackVersion == "" || item.Runner == "" ||
			item.EnvironmentPath == "" || (!item.Development && item.PackPath == "") {
			return errors.New("正式安装缺少 pack_id、pack_version、runner 或 environment_path")
		}
		if !runtimeInstallationIDPattern.MatchString(item.PackID) ||
			!runtimePackVersionPattern.MatchString(item.PackVersion) {
			return errors.New("pack_id 或 pack_version 无效")
		}
		absoluteEnvironment, err := filepath.Abs(item.EnvironmentPath)
		if err != nil || absoluteEnvironment != item.EnvironmentPath {
			return errors.New("environment_path 必须是规范绝对路径")
		}
		for label, path := range map[string]string{
			"pack_path": item.PackPath, "scene_catalog_path": item.SceneCatalogPath,
			"settings_path": item.SettingsPath,
		} {
			if path == "" {
				continue
			}
			absolute, pathErr := filepath.Abs(path)
			if pathErr != nil || absolute != path {
				return fmt.Errorf("%s 必须是规范绝对路径", label)
			}
		}
		executable, err := RuntimeRunnerExecutable(item.EnvironmentPath, item.Runner)
		if err != nil {
			return err
		}
		if _, err := RuntimeContentEnvironment(item.ContentRefs); err != nil {
			return err
		}
		seenLicenses := map[string]bool{}
		for _, license := range item.AcceptedLicenses {
			license = strings.TrimSpace(license)
			if license == "" || seenLicenses[license] {
				return errors.New("accepted_licenses 不能包含空值或重复值")
			}
			seenLicenses[license] = true
		}
		if item.LaunchMode != "process" {
			return errors.New("Runtime Pack 本机安装的 launch_mode 必须为 process")
		}
		parsedEndpoint, endpointErr := url.Parse(item.Endpoint)
		if endpointErr != nil || parsedEndpoint.Scheme != "http" ||
			parsedEndpoint.Hostname() == "" || parsedEndpoint.Port() == "" ||
			(parsedEndpoint.Hostname() != "127.0.0.1" &&
				parsedEndpoint.Hostname() != "localhost" && parsedEndpoint.Hostname() != "::1") {
			return errors.New("Runtime Pack 本机 endpoint 必须是带端口的 loopback http URL")
		}
		// schema v2 不读取 pack 中的 command。这里覆盖为 Framework 内建映射，
		// 即使手工清单误填 command 也不会执行。
		item.Command = []string{executable}
		item.Workdir = item.EnvironmentPath
		item.EnvironmentRefs = nil
	}
	if !item.Enabled {
		item.Status = "disabled"
		return nil
	}
	switch item.LaunchMode {
	case "uv", "process":
		if len(item.Command) == 0 || strings.TrimSpace(item.Command[0]) == "" {
			item.Status = "failed"
			item.Diagnostic = "安装清单缺少 command"
			return nil
		}
		if item.Workdir == "" {
			item.Status = "failed"
			item.Diagnostic = "workdir 环境引用尚未配置"
			return nil
		}
	case "remote":
		parsed, err := url.Parse(item.Endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" {
			item.Status = "failed"
			item.Diagnostic = "remote endpoint 必须是 http(s) URL"
			return nil
		}
	case "container":
		if strings.TrimSpace(item.Image) == "" || item.Endpoint == "" {
			item.Status = "failed"
			item.Diagnostic = "container 安装必须声明 image 和 endpoint"
			return nil
		}
	default:
		return fmt.Errorf("launch_mode %q 不受支持", item.LaunchMode)
	}
	for key, envName := range item.EnvironmentRefs {
		if strings.TrimSpace(key) == "" || !strings.HasPrefix(envName, "SEMANTIC_") {
			return fmt.Errorf("environment_refs 必须把固定变量名映射到 SEMANTIC_ 环境变量")
		}
	}
	item.Status = "offline"
	return nil
}

func resolveEnvReference(value string) string {
	if len(value) > 3 && strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		return strings.TrimSpace(os.Getenv(value[2 : len(value)-1]))
	}
	return value
}

func (c *RuntimeInstallationCatalog) List() []RuntimeInstallation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]RuntimeInstallation, 0, len(c.order))
	for _, id := range c.order {
		result = append(result, c.items[id])
	}
	return result
}

func (c *RuntimeInstallationCatalog) Views() []RuntimeInstallationView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	items := make([]RuntimeInstallation, 0, len(c.order))
	for _, id := range c.order {
		items = append(items, c.items[id])
	}
	result := make([]RuntimeInstallationView, 0, len(items))
	for _, item := range items {
		result = append(result, c.viewLocked(item))
	}
	return result
}

func (c *RuntimeInstallationCatalog) View(id string) (RuntimeInstallationView, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.items[id]
	if !ok {
		return RuntimeInstallationView{}, fmt.Errorf(
			"%w: Runtime installation 不存在: %s", ErrNotFound, id,
		)
	}
	return c.viewLocked(item), nil
}

func (c *RuntimeInstallationCatalog) viewLocked(item RuntimeInstallation) RuntimeInstallationView {
	view := item.Public()
	if item.Enabled {
		if observed, ok := c.observed[item.InstallationID]; ok {
			view.Status = observed.status
			view.Diagnostic = observed.diagnostic
		}
	}
	return view
}

// ObserveStatus 只保存当前 Server 进程看到的运行状态，不写回安装清单。
// 安装路径/命令的静态诊断与网络、进程状态因此不会互相覆盖。
func (c *RuntimeInstallationCatalog) ObserveStatus(id, status, diagnostic string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[id]; !ok {
		return
	}
	if c.observed == nil {
		c.observed = make(map[string]runtimeInstallationObservation)
	}
	c.observed[id] = runtimeInstallationObservation{status: status, diagnostic: diagnostic}
}

func (c *RuntimeInstallationCatalog) Get(id string) (RuntimeInstallation, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.items[id]
	if !ok {
		return RuntimeInstallation{}, fmt.Errorf("%w: Runtime installation 不存在: %s",
			ErrNotFound, id)
	}
	return item, nil
}

func (c *RuntimeInstallationCatalog) ByProfile(profileID string) (RuntimeInstallation, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, item := range c.items {
		if item.Profile.RuntimeProfileID == profileID {
			return item, nil
		}
	}
	return RuntimeInstallation{}, fmt.Errorf("%w: Runtime profile 不存在: %s",
		ErrNotFound, profileID)
}

func (i RuntimeInstallation) Binding() RuntimeBinding {
	endpoint := i.Endpoint
	client := NewHTTPRuntimeClient(endpoint, nil)
	var launcher RuntimeLauncher
	if i.Enabled && i.Diagnostic == "" && (i.LaunchMode == "uv" || i.LaunchMode == "process") {
		env := make([]string, 0, len(i.EnvironmentRefs)+len(i.ContentRefs)+1)
		if i.SchemaVersion == runtimeInstallationSchemaVersion {
			env, _ = RuntimeContentEnvironment(i.ContentRefs)
			env = append(env, RuntimeEndpointEnvironment(i.Endpoint)...)
			// 同一入口可以装载多个 Profile；正式启动与安装 smoke 使用同一份
			// Profile 身份，安装实例 ID 仅用于进程管理，不传给场景加载器。
			env = append(env, "SEMANTIC_SIM_PROFILE="+i.Profile.RuntimeProfileID)
			if i.SettingsPath != "" {
				env = append(env, "SEMANTIC_RUNTIME_CONFIG="+i.SettingsPath)
			}
		} else {
			for key, envName := range i.EnvironmentRefs {
				if value := os.Getenv(envName); value != "" {
					env = append(env, key+"="+value)
				}
			}
		}
		sort.Strings(env)
		launcher = ExecLauncher{
			Command: i.Command[0], Args: append([]string{}, i.Command[1:]...),
			Dir: i.Workdir, Env: env,
		}
	}
	return RuntimeBinding{InstallationID: i.InstallationID, Profile: i.Profile, Client: client, Launcher: launcher}
}

// RuntimeEndpointEnvironment 将已校验 endpoint 转换为 Runtime 入口接受的
// host/port。端口只来自管理员生成的 installation，不接受网页透传。
func RuntimeEndpointEnvironment(endpoint string) []string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" || port == "" {
		return nil
	}
	// 新 Runtime 使用引擎无关的地址变量；保留既有变量供已发布 MuJoCo 包使用。
	return []string{"PLUGIN_MUJOCO_HOST=" + host, "PLUGIN_MUJOCO_PORT=" + port,
		"SEMANTIC_RUNTIME_HOST=" + host, "SEMANTIC_RUNTIME_PORT=" + port}
}

func cloneStrings(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func cloneRuntimeCapability(input RuntimeCapability) RuntimeCapability {
	result := input
	result.ViewerCameraModes = append([]string(nil), input.ViewerCameraModes...)
	result.RobotModels = append([]string(nil), input.RobotModels...)
	result.SensorKinds = append([]string(nil), input.SensorKinds...)
	return result
}
