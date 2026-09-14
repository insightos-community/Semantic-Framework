// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package simulation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"insightos.cn/semantic-framework/internal/ports/platform"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const RuntimePackSchemaVersion = 1

var runtimePackVersionPattern = regexp.MustCompile(
	`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`,
)

var runtimePackSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RuntimePackFile 是 Pack 内必须存在且需要校验摘要的文件。
// Path 只能指向 Pack 根目录内的普通文件，安装器不跟随符号链接。
type RuntimePackFile struct {
	Path   string `yaml:"path" json:"path"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

// RuntimeContentRequirement 描述 Pack 不携带的大型/受限内容。安装器只登记
// 本机只读路径，并按固定提交与许可确认进行检查，不会下载或复制这些内容。
type RuntimeContentRequirement struct {
	Required            bool   `yaml:"required" json:"required"`
	Revision            string `yaml:"revision,omitempty" json:"revision,omitempty"`
	LicenseConfirmation string `yaml:"license_confirmation,omitempty" json:"license_confirmation,omitempty"`
}

// RuntimePackManifest 描述 CI 已构建的可安装 Runtime 制品。
// Pack 只携带 Wheel、锁文件和元数据；运行命令由 Framework 根据 Runner 生成，
// 因此制品不能通过 manifest 注入任意 Shell 或 Python 入口。
type RuntimePackManifest struct {
	SchemaVersion        int                                  `yaml:"schema_version" json:"schema_version"`
	PackID               string                               `yaml:"pack_id" json:"pack_id"`
	PackVersion          string                               `yaml:"pack_version" json:"pack_version"`
	Profile              RuntimeProfile                       `yaml:"profile" json:"profile"`
	Runner               string                               `yaml:"runner" json:"runner"`
	PythonVersion        string                               `yaml:"python_version" json:"python_version"`
	Endpoint             string                               `yaml:"endpoint" json:"endpoint"`
	HardwareRequirements map[string]string                    `yaml:"hardware_requirements,omitempty" json:"hardware_requirements,omitempty"`
	RequirementsLock     RuntimePackFile                      `yaml:"requirements_lock" json:"requirements_lock"`
	Wheels               []RuntimePackFile                    `yaml:"wheels" json:"wheels"`
	Wheelhouse           []RuntimePackFile                    `yaml:"wheelhouse" json:"wheelhouse"`
	SceneCatalog         RuntimePackFile                      `yaml:"scene_catalog" json:"scene_catalog"`
	SceneResources       []RuntimePackFile                    `yaml:"scene_resources,omitempty" json:"scene_resources,omitempty"`
	Licenses             []RuntimePackFile                    `yaml:"licenses" json:"licenses"`
	VerificationFiles    []RuntimePackFile                    `yaml:"verification_files" json:"verification_files"`
	SmokeSceneKey        string                               `yaml:"smoke_scene_key" json:"smoke_scene_key"`
	SmokeRequest         RuntimePackFile                      `yaml:"smoke_request" json:"smoke_request"`
	ContentRequirements  map[string]RuntimeContentRequirement `yaml:"content_requirements,omitempty" json:"content_requirements,omitempty"`
}

// LoadRuntimePack 严格读取已解包目录。所有内容先校验再创建 Python 环境，
// 防止半安装状态和被替换的 Wheel 进入可用 installation。
func LoadRuntimePack(root string) (RuntimePackManifest, error) {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil || root == "" {
		return RuntimePackManifest{}, errors.New("Runtime Pack 根目录无效")
	}
	data, err := os.ReadFile(filepath.Join(root, "runtime-pack.yaml"))
	if err != nil {
		return RuntimePackManifest{}, fmt.Errorf("读取 runtime-pack.yaml 失败: %w", err)
	}
	var manifest RuntimePackManifest
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return RuntimePackManifest{}, fmt.Errorf("解析 runtime-pack.yaml 失败: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return RuntimePackManifest{}, err
	}
	for _, file := range manifest.Files() {
		if err := verifyRuntimePackFile(root, file); err != nil {
			return RuntimePackManifest{}, err
		}
	}
	smokeData, err := os.ReadFile(filepath.Join(
		root, filepath.FromSlash(manifest.SmokeRequest.Path),
	))
	if err != nil {
		return RuntimePackManifest{}, err
	}
	var smoke map[string]any
	if err := json.Unmarshal(smokeData, &smoke); err != nil || len(smoke) == 0 {
		return RuntimePackManifest{}, errors.New("Runtime Pack smoke_request 必须是非空 JSON 对象")
	}
	requestID, _ := smoke["request_id"].(string)
	if strings.TrimSpace(requestID) == "" {
		return RuntimePackManifest{}, errors.New("Runtime Pack smoke_request 缺少 request_id")
	}
	return manifest, nil
}

func (m RuntimePackManifest) Validate() error {
	if m.SchemaVersion != RuntimePackSchemaVersion {
		return fmt.Errorf("runtime pack schema_version 必须为 %d", RuntimePackSchemaVersion)
	}
	if !runtimeInstallationIDPattern.MatchString(m.PackID) ||
		!runtimePackVersionPattern.MatchString(m.PackVersion) {
		return errors.New("pack_id 或 pack_version 无效")
	}
	if m.Profile.RuntimeProfileID == "" || m.Profile.Engine == "" ||
		m.Profile.Loader == "" || m.PythonVersion == "" {
		return errors.New("profile、engine、loader 和 python_version 不能为空")
	}
	if _, err := RuntimeRunnerExecutable("/runtime-env", m.Runner); err != nil {
		return err
	}
	for key, value := range m.HardwareRequirements {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return errors.New("hardware_requirements 不能包含空键或空值")
		}
	}
	endpoint, err := url.Parse(strings.TrimSpace(m.Endpoint))
	if err != nil || endpoint.Scheme != "http" || endpoint.Port() == "" ||
		(endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost" &&
			endpoint.Hostname() != "::1") {
		return errors.New("Pack endpoint 必须是带端口的 loopback http URL")
	}
	if len(m.Wheels) == 0 || len(m.Wheelhouse) == 0 || len(m.Licenses) == 0 ||
		len(m.VerificationFiles) == 0 ||
		m.RequirementsLock.Path == "" || m.SceneCatalog.Path == "" || m.SmokeSceneKey == "" ||
		m.SmokeRequest.Path == "" {
		return errors.New("Pack 必须包含 wheels、wheelhouse、依赖锁、场景目录、许可证和 smoke_scene_key")
	}
	seen := map[string]bool{}
	for _, file := range m.Files() {
		if err := validateRuntimePackRelativePath(file.Path); err != nil {
			return err
		}
		if !runtimePackSHA256Pattern.MatchString(file.SHA256) {
			return fmt.Errorf("Runtime Pack 文件 %s 的 sha256 无效", file.Path)
		}
		if seen[file.Path] {
			return fmt.Errorf("Runtime Pack 文件重复: %s", file.Path)
		}
		seen[file.Path] = true
	}
	for key, requirement := range m.ContentRequirements {
		if _, err := RuntimeContentEnvironment(map[string]string{key: "/content"}); err != nil {
			return err
		}
		if !requirement.Required && requirement.Revision == "" &&
			requirement.LicenseConfirmation == "" {
			return fmt.Errorf("Runtime content requirement %s 没有约束", key)
		}
		if requirement.Revision != "" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(requirement.Revision) {
			return fmt.Errorf("Runtime content requirement %s 的 revision 必须是完整 Git commit", key)
		}
	}
	return nil
}

func (m RuntimePackManifest) Files() []RuntimePackFile {
	result := []RuntimePackFile{m.RequirementsLock, m.SceneCatalog, m.SmokeRequest}
	result = append(result, m.Wheels...)
	result = append(result, m.Wheelhouse...)
	result = append(result, m.SceneResources...)
	result = append(result, m.Licenses...)
	result = append(result, m.VerificationFiles...)
	return result
}

func validateRuntimePackRelativePath(value string) error {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(value)))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") ||
		strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") || clean != value {
		return fmt.Errorf("Runtime Pack 路径必须是规范相对路径: %q", value)
	}
	return nil
}

func verifyRuntimePackFile(root string, file RuntimePackFile) error {
	path := filepath.Join(root, filepath.FromSlash(file.Path))
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("Runtime Pack 缺少 %s: %w", file.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Runtime Pack 文件 %s 必须是普通文件", file.Path)
	}
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != file.SHA256 {
		return fmt.Errorf("Runtime Pack 文件 %s 摘要不匹配", file.Path)
	}
	return nil
}

// RuntimeRunnerExecutable 是 formal installation 唯一允许的入口映射。
// Runtime Pack 不能覆盖这份映射，也不能提供任意 command。
func RuntimeRunnerExecutable(environmentPath, runner string) (string, error) {
	name := ""
	switch runner {
	case "native-mujoco":
		name = "plugin-mujoco"
	case "robosuite-1.5", "libero-robosuite-1.4":
		name = "semantic-sim-runtime"
	default:
		return "", fmt.Errorf("Runtime runner %q 不受支持", runner)
	}
	return platform.VenvExecutable(environmentPath, name), nil
}

// RuntimeContentEnvironment 将安装时登记的只读内容转换为已知环境变量。
// 未知 key 会被拒绝，避免 manifest 或网页构造任意子进程环境。
func RuntimeContentEnvironment(content map[string]string) ([]string, error) {
	known := map[string]string{
		"mujoco_assets":     "MUJOCO_ASSET_ROOT",
		"r1pro_model":       "R1PRO_MODEL_ROOT",
		"franka_model":      "FRANKA_MODEL_ROOT",
		"libero_source":     "SEMANTIC_LIBERO_ROOT",
		"libero_pro_source": "SEMANTIC_LIBERO_PRO_ROOT",
	}
	result := make([]string, 0, len(content)+1)
	for key, value := range content {
		name, ok := known[key]
		if !ok {
			return nil, fmt.Errorf("未知 Runtime content_ref: %s", key)
		}
		if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) {
			return nil, fmt.Errorf("Runtime content_ref %s 必须是绝对路径", key)
		}
		result = append(result, name+"="+value)
	}
	backend := platform.RenderBackend()
	result = append(result, "MUJOCO_GL="+backend)
	sort.Strings(result)
	return result, nil
}
