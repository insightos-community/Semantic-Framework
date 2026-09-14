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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"insightos.cn/semantic-framework/internal/ports/platform"
	processport "insightos.cn/semantic-framework/internal/ports/process"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
	semanticlog "insightos.cn/semantic-framework/pkg/log"
)

const runtimeInstallTimeout = 15 * time.Minute

const (
	runtimeChecksumMaxBytes = 1 << 20
	runtimeArchiveMaxBytes  = int64(8) << 30
)

type runtimeInstallOptions struct {
	configPath        string
	packArchive       string
	devSource         string
	profileID         string
	installationID    string
	registry          string
	expectedSHA256    string
	assetRoot         string
	modelRoot         string
	liberoRoot        string
	liberoProRoot     string
	sceneCatalog      string
	endpoint          string
	acceptedLicenses  stringListFlag
	expectedProfileID string
	replace           bool
}

type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("许可确认名不能为空")
	}
	*s = append(*s, value)
	return nil
}

type runtimePaths struct {
	root       string
	runtimes   string
	scenes     string
	packs      string
	envs       string
	config     *config.Config
	configPath string
}

func runRuntimeInstall(args []string) int {
	options, reference, err := parseRuntimeInstallOptions("runtime install", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := installRuntimePack(options, reference); err != nil {
		fmt.Fprintln(os.Stderr, "Runtime 安装失败:", err)
		return 1
	}
	return 0
}

func parseRuntimeInstallOptions(name string, args []string) (runtimeInstallOptions, string, error) {
	var options runtimeInstallOptions
	reference := ""
	filtered := make([]string, 0, len(args))
	for _, argument := range args {
		parts := strings.Split(argument, "@")
		if len(parts) == 2 && installationIDValid(parts[0]) &&
			runtimeVersionValid(parts[1]) && reference == "" {
			reference = argument
			continue
		}
		filtered = append(filtered, argument)
	}
	args = filtered
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.StringVar(&options.configPath, "c", "", "配置文件路径")
	flags.StringVar(&options.packArchive, "pack", "", "离线 Runtime Pack 制品")
	flags.StringVar(&options.devSource, "dev-source", "", "plugin-mujoco 源码目录（仅开发）")
	flags.StringVar(&options.profileID, "profile", "", "开发源码模式的 Runtime Profile")
	flags.StringVar(&options.installationID, "installation-id", "", "本机 installation_id")
	flags.StringVar(&options.installationID, "id", "", "已有 installation_id（upgrade）")
	flags.StringVar(&options.registry, "registry", "", "Runtime Pack Registry HTTPS 根地址")
	flags.StringVar(&options.expectedSHA256, "sha256", "", "离线制品 SHA256")
	flags.StringVar(&options.assetRoot, "asset-root", "", "MuJoCo 资产根目录")
	flags.StringVar(&options.modelRoot, "model-root", "", "Robot Model Bundle 根目录")
	flags.StringVar(&options.liberoRoot, "libero-root", "", "固定版本 LIBERO 源码/数据目录")
	flags.StringVar(&options.liberoProRoot, "libero-pro-root", "", "固定版本 LIBERO-Pro 目录")
	flags.StringVar(&options.sceneCatalog, "scene-catalog", "", "开发模式场景目录 YAML")
	flags.StringVar(&options.endpoint, "endpoint", "", "本机 Runtime endpoint 覆盖")
	flags.Var(&options.acceptedLicenses, "accept-license", "确认外部内容许可，可重复")
	flags.BoolVar(&options.replace, "replace", false, "升级或修复同一 installation")
	if err := flags.Parse(args); err != nil {
		return options, "", err
	}
	if flags.NArg() > 1 {
		return options, "", errors.New("最多只能提供一个 pack@version")
	}
	if flags.NArg() == 1 {
		if reference != "" {
			return options, "", errors.New("重复提供 pack@version")
		}
		reference = flags.Arg(0)
	}
	sources := 0
	for _, value := range []string{reference, options.packArchive, options.devSource} {
		if strings.TrimSpace(value) != "" {
			sources++
		}
	}
	if sources != 1 {
		return options, "", errors.New("必须且只能选择 pack@version、--pack 或 --dev-source")
	}
	return options, reference, nil
}

func runtimeVersionValid(value string) bool {
	parts := strings.SplitN(value, "-", 2)
	base := strings.Split(parts[0], ".")
	if len(base) != 3 {
		return false
	}
	for _, part := range base {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func runtimeInstallPaths(configFlag string) (runtimePaths, error) {
	configPath, err := config.ResolvePath(configFlag)
	if err != nil {
		return runtimePaths{}, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return runtimePaths{}, err
	}
	root := runtimeRoot(configPath)
	runtimes, err := filepath.Abs(cfg.Simulation.RuntimesDir)
	if err != nil {
		return runtimePaths{}, err
	}
	scenes, err := filepath.Abs(cfg.Simulation.CatalogDir)
	if err != nil {
		return runtimePaths{}, err
	}
	return runtimePaths{
		root: root, runtimes: runtimes, scenes: scenes,
		packs:  filepath.Join(root, "runtime-packs"),
		envs:   filepath.Join(root, "runtime-envs"),
		config: cfg, configPath: configPath,
	}, nil
}

func installRuntimePack(options runtimeInstallOptions, reference string) error {
	paths, err := runtimeInstallPaths(options.configPath)
	if err != nil {
		return fmt.Errorf("读取 Semantic 安装配置失败: %w", err)
	}
	for _, dir := range []string{paths.runtimes, paths.scenes, paths.packs, paths.envs} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if options.devSource != "" {
		return installDevelopmentRuntime(paths, options)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeInstallTimeout)
	defer cancel()
	source, cleanup, err := acquireRuntimePack(ctx, paths, options, reference)
	if err != nil {
		return err
	}
	defer cleanup()
	manifest, err := simulation.LoadRuntimePack(source)
	if err != nil {
		return err
	}
	if options.expectedProfileID != "" &&
		manifest.Profile.RuntimeProfileID != options.expectedProfileID {
		return fmt.Errorf("升级 Pack profile %s 与 installation profile %s 不一致",
			manifest.Profile.RuntimeProfileID, options.expectedProfileID)
	}
	installationID := strings.TrimSpace(options.installationID)
	if installationID == "" {
		installationID = "local-" + manifest.Profile.RuntimeProfileID
	}
	if !installationIDValid(installationID) {
		return errors.New("installation_id 格式无效")
	}
	content, err := resolveRuntimeContent(manifest.Runner, options)
	if err != nil {
		return err
	}
	if err := validateContentRequirements(ctx, content, manifest.ContentRequirements,
		options.acceptedLicenses); err != nil {
		return err
	}
	packPath := filepath.Join(paths.packs, manifest.PackID, manifest.PackVersion)
	_, packStatErr := os.Stat(packPath)
	packExisted := packStatErr == nil
	if err := materializeRuntimePack(source, packPath); err != nil {
		return err
	}
	// 再次从最终路径校验，防止复制阶段遗漏或替换文件。
	manifest, err = simulation.LoadRuntimePack(packPath)
	if err != nil {
		return err
	}
	environmentPath := filepath.Join(paths.envs, installationID, manifest.PackVersion)
	_, environmentStatErr := os.Stat(environmentPath)
	environmentExisted := environmentStatErr == nil
	activated := false
	defer func() {
		if activated {
			return
		}
		if !environmentExisted && pathInside(paths.envs, environmentPath) {
			_ = os.RemoveAll(environmentPath)
		}
		if !packExisted && pathInside(paths.packs, packPath) {
			_ = os.RemoveAll(packPath)
		}
	}()
	if err := buildRuntimeEnvironment(ctx, packPath, environmentPath, manifest); err != nil {
		return err
	}
	endpoint := selectRuntimeEndpoint(options.endpoint, manifest.Endpoint,
		manifest.Profile.RuntimeProfileID)
	installation := simulation.RuntimeInstallation{
		SchemaVersion: 2, InstallationID: installationID,
		Profile: manifest.Profile, PackID: manifest.PackID, PackVersion: manifest.PackVersion,
		Runner: manifest.Runner, LaunchMode: "process", EnvironmentPath: environmentPath,
		PackPath: packPath, ContentRefs: content, Endpoint: endpoint,
		HardwareRequirements: maps.Clone(manifest.HardwareRequirements),
		AcceptedLicenses:     normalizedStrings(options.acceptedLicenses),
		Enabled:              true, InstalledVersion: manifest.PackVersion,
	}
	if err := runInstalledRuntimeSmoke(ctx, installation, manifest, packPath); err != nil {
		return err
	}
	sceneRoot := filepath.Join(paths.scenes, installationID)
	sceneTarget := filepath.Join(sceneRoot, filepath.Base(manifest.SceneCatalog.Path))
	installation.SceneCatalogPath = sceneTarget
	if err := activateRuntimeInstallation(paths, installation, manifest, packPath,
		options.replace); err != nil {
		return err
	}
	activated = true
	fmt.Printf("✓ Runtime %s (%s@%s) 已安装并通过真实场景 smoke\n",
		installationID, manifest.PackID, manifest.PackVersion)
	fmt.Printf("  环境: %s\n  清单: %s\n", environmentPath,
		filepath.Join(paths.runtimes, installationID+".yaml"))
	return nil
}

func installationIDValid(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			(character == '-' || character == '_' || character == '.') && index > 0
		if !valid {
			return false
		}
	}
	return true
}

func acquireRuntimePack(ctx context.Context, paths runtimePaths, options runtimeInstallOptions,
	reference string) (string, func(), error) {
	if options.packArchive != "" {
		archive, err := filepath.Abs(options.packArchive)
		if err != nil {
			return "", func() {}, err
		}
		if err := verifyArchiveSHA256(archive, options.expectedSHA256); err != nil {
			return "", func() {}, err
		}
		return extractRuntimePackArchive(ctx, paths.packs, archive)
	}
	parts := strings.Split(reference, "@")
	if len(parts) != 2 || !installationIDValid(parts[0]) || parts[1] == "" {
		return "", func() {}, errors.New("在线 Pack 必须使用 pack_id@version")
	}
	registry := strings.TrimRight(strings.TrimSpace(options.registry), "/")
	if registry == "" {
		registry = strings.TrimRight(strings.TrimSpace(os.Getenv("SEMANTIC_RUNTIME_REGISTRY")), "/")
	}
	parsed, err := url.Parse(registry)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", func() {}, errors.New("在线安装需要 HTTPS --registry 或 SEMANTIC_RUNTIME_REGISTRY")
	}
	filename := "semantic-" + parts[0] + "-" + parts[1] + ".runtime.tar.zst"
	archive := filepath.Join(paths.packs, ".download-"+filename)
	defer func() { _ = os.Remove(archive); _ = os.Remove(archive + ".sha256") }()
	base := registry + "/" + parts[0] + "/" + parts[1] + "/" + filename
	checksum, err := downloadRuntimeFile(ctx, base+".sha256", "", runtimeChecksumMaxBytes)
	if err != nil {
		return "", func() {}, err
	}
	fields := strings.Fields(string(checksum))
	if len(fields) == 0 {
		return "", func() {}, errors.New("Registry checksum 响应为空")
	}
	if _, err := downloadRuntimeFile(ctx, base, archive, runtimeArchiveMaxBytes); err != nil {
		return "", func() {}, err
	}
	if err := verifyArchiveSHA256(archive, fields[0]); err != nil {
		return "", func() {}, err
	}
	return extractRuntimePackArchive(ctx, paths.packs, archive)
}

func downloadRuntimeFile(
	ctx context.Context, address, target string, maxBytes int64,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(os.Getenv("SEMANTIC_RUNTIME_REGISTRY_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("下载 Runtime Pack 失败: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 Runtime Pack 返回 HTTP %d", response.StatusCode)
	}
	if maxBytes <= 0 {
		return nil, errors.New("下载大小上限无效")
	}
	var buffer bytes.Buffer
	var output io.Writer = &buffer
	partial := ""
	var file *os.File
	if target != "" {
		partial = target + ".partial"
		_ = os.Remove(partial)
		file, err = os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = file.Close()
			_ = os.Remove(partial)
		}()
		output = file
	}
	written, err := io.Copy(output, io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if written > maxBytes {
		return nil, fmt.Errorf("Runtime Pack 下载超过上限 %d bytes", maxBytes)
	}
	if file != nil {
		if err := file.Sync(); err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(partial, target); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return buffer.Bytes(), nil
}

func verifyArchiveSHA256(path, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		data, err := os.ReadFile(path + ".sha256")
		if err != nil {
			return errors.New("离线 Pack 必须提供 --sha256 或同名 .sha256 文件")
		}
		fields := strings.Fields(string(data))
		if len(fields) == 0 {
			return errors.New("Pack checksum 文件为空")
		}
		expected = fields[0]
	}
	if len(expected) != 64 {
		return errors.New("Pack SHA256 格式无效")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return errors.New("Pack SHA256 格式无效")
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
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("Runtime Pack 制品 SHA256 不匹配")
	}
	return nil
}

func extractRuntimePackArchive(ctx context.Context, parent, archive string) (string, func(), error) {
	if strings.HasSuffix(archive, ".tar.gz") {
		return extractRuntimePackGzip(ctx, parent, archive)
	}
	listing, err := exec.CommandContext(ctx, "tar", "--zstd", "-tf", archive).Output()
	if err != nil {
		return "", func() {}, fmt.Errorf("读取 Runtime Pack 目录失败: %w", err)
	}
	for _, entry := range strings.Split(string(listing), "\n") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if err := validateRuntimeArchiveEntry(entry); err != nil {
			return "", func() {}, fmt.Errorf("Runtime Pack 包含不安全路径: %q", entry)
		}
	}
	temp, err := os.MkdirTemp(parent, ".runtime-pack-extract-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(temp) }
	command := exec.CommandContext(ctx, "tar", "--zstd", "-xf", archive, "-C", temp,
		"--no-same-owner", "--no-same-permissions")
	if output, err := command.CombinedOutput(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("解包 Runtime Pack 失败: %w: %s", err, output)
	}
	if err := validateExtractedRuntimePackTree(temp); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if _, err := simulation.LoadRuntimePack(temp); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return temp, cleanup, nil
}

// tar 会为目录打印尾部斜杠；校验时只移除这个表示类型的斜杠，仍拒绝
// 绝对路径、父目录逃逸、Windows 盘符和非规范路径。解包后的文件类型由
// validateExtractedRuntimePackTree 再检查一次，不能仅相信 tar 的文本列表。
func validateRuntimeArchiveEntry(entry string) error {
	trimmed := strings.TrimSuffix(strings.TrimSpace(entry), "/")
	if trimmed == "" || trimmed == "." {
		return errors.New("空路径")
	}
	clean := filepath.ToSlash(filepath.Clean(trimmed))
	if clean != trimmed || strings.HasPrefix(clean, "/") || clean == ".." ||
		strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
		return errors.New("路径不安全")
	}
	return nil
}

func validateExtractedRuntimePackTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			relative, _ := filepath.Rel(root, path)
			return fmt.Errorf("Runtime Pack 只能包含目录和普通文件: %s", relative)
		}
		return nil
	})
}

func materializeRuntimePack(source, target string) error {
	if _, err := os.Stat(target); err == nil {
		_, err = simulation.LoadRuntimePack(target)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	manifest, err := simulation.LoadRuntimePack(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(target), ".runtime-pack-stage-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	files := append([]simulation.RuntimePackFile(nil), manifest.Files()...)
	files = append(files, simulation.RuntimePackFile{Path: "runtime-pack.yaml"})
	for _, file := range files {
		if err := copyPackFile(source, temp, file.Path); err != nil {
			return err
		}
	}
	if _, err := simulation.LoadRuntimePack(temp); err != nil {
		return err
	}
	return os.Rename(temp, target)
}

func copyPackFile(sourceRoot, targetRoot, relative string) error {
	source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("Pack 文件不是普通文件: %s", relative)
	}
	target := filepath.Join(targetRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func resolveRuntimeContent(runner string, options runtimeInstallOptions) (map[string]string, error) {
	content := map[string]string{}
	add := func(key, value string) error {
		if strings.TrimSpace(value) == "" {
			return nil
		}
		absolute, err := filepath.Abs(value)
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return fmt.Errorf("Runtime 内容 %s 不可访问: %w", key, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("Runtime 内容 %s 必须是目录", key)
		}
		content[key] = resolved
		return nil
	}
	switch runner {
	case "native-mujoco":
		if err := add("mujoco_assets", options.assetRoot); err != nil {
			return nil, err
		}
		if err := add("r1pro_model", options.modelRoot); err != nil {
			return nil, err
		}
	case "robosuite-1.5":
		if err := add("franka_model", options.modelRoot); err != nil {
			return nil, err
		}
	case "libero-robosuite-1.4":
		if err := add("franka_model", options.modelRoot); err != nil {
			return nil, err
		}
		if err := add("libero_source", options.liberoRoot); err != nil {
			return nil, err
		}
		if err := add("libero_pro_source", options.liberoProRoot); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("未知 Runtime runner: %s", runner)
	}
	return content, nil
}

func validateContentRequirements(ctx context.Context, content map[string]string,
	requirements map[string]simulation.RuntimeContentRequirement, accepted []string) error {
	acceptedSet := map[string]bool{}
	for _, value := range accepted {
		acceptedSet[value] = true
	}
	for key, requirement := range requirements {
		path := content[key]
		if requirement.Required && path == "" {
			return fmt.Errorf("Runtime Pack 需要内容 %s，请在安装时提供路径", key)
		}
		if path == "" {
			continue
		}
		if requirement.LicenseConfirmation != "" && !acceptedSet[requirement.LicenseConfirmation] {
			return fmt.Errorf("内容 %s 需要 --accept-license %s", key, requirement.LicenseConfirmation)
		}
		if requirement.Revision != "" {
			output, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "HEAD").Output()
			if err != nil || strings.TrimSpace(string(output)) != requirement.Revision {
				return fmt.Errorf("内容 %s Git revision 不符合 Pack 要求 %s", key, requirement.Revision)
			}
		}
	}
	return nil
}

func buildRuntimeEnvironment(ctx context.Context, packPath, target string,
	manifest simulation.RuntimePackManifest) error {
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		executable, mapErr := simulation.RuntimeRunnerExecutable(target, manifest.Runner)
		if mapErr == nil {
			if _, statErr := os.Stat(executable); statErr == nil {
				return nil
			}
		}
		return errors.New("已有 Runtime 环境不完整；请先运行 doctor 后修复")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := exec.LookPath("uv"); err != nil {
		return errors.New("未找到 uv；Runtime Pack 安装器只使用固定 uv 环境")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	// Python venv 的 console script shebang 会写入创建时的绝对 Python 路径，
	// 因此不能在随机目录创建后整体 rename。这里直接写尚未登记的版本目录；
	// smoke 与 installation manifest 激活之前失败都会删除该目录。
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(target)
		}
	}()
	if output, err := exec.CommandContext(ctx, "uv", "venv", "--python", manifest.PythonVersion,
		target).CombinedOutput(); err != nil {
		return fmt.Errorf("创建 Runtime Python 环境失败: %w: %s", err, output)
	}
	python := platform.VenvExecutable(target, "python")
	wheelhouse := filepath.Join(packPath, "wheelhouse")
	lock := filepath.Join(packPath, filepath.FromSlash(manifest.RequirementsLock.Path))
	args := []string{"pip", "sync", "--python", python, "--no-index", "--find-links", wheelhouse, lock}
	if output, err := exec.CommandContext(ctx, "uv", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("安装锁定 Runtime 依赖失败: %w: %s", err, output)
	}
	wheelArgs := []string{"pip", "install", "--python", python, "--no-index",
		"--find-links", wheelhouse, "--no-deps"}
	for _, wheel := range manifest.Wheels {
		wheelArgs = append(wheelArgs, filepath.Join(packPath, filepath.FromSlash(wheel.Path)))
	}
	if output, err := exec.CommandContext(ctx, "uv", wheelArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("安装 Runtime Wheel 失败: %w: %s", err, output)
	}
	executable, err := simulation.RuntimeRunnerExecutable(target, manifest.Runner)
	if err != nil {
		return err
	}
	if info, err := os.Stat(executable); err != nil || !platform.Runnable(info) {
		return fmt.Errorf("Runtime Pack 安装后缺少可执行入口 %s", executable)
	}
	complete = true
	return nil
}

func selectRuntimeEndpoint(explicit, fromPack, profileID string) string {
	if strings.TrimSpace(explicit) != "" {
		return strings.TrimRight(explicit, "/")
	}
	if strings.TrimSpace(fromPack) != "" {
		return strings.TrimRight(fromPack, "/")
	}
	port := "8090"
	if profileID == "robosuite-1.5" {
		port = "8091"
	}
	if profileID == "libero-robosuite-1.4" {
		port = "8092"
	}
	return "http://127.0.0.1:" + port
}

func runInstalledRuntimeSmoke(ctx context.Context, installation simulation.RuntimeInstallation,
	manifest simulation.RuntimePackManifest, packPath string) error {
	request, err := os.ReadFile(filepath.Join(packPath, filepath.FromSlash(manifest.SmokeRequest.Path)))
	if err != nil {
		return err
	}
	return runRuntimeSmoke(ctx, installation, manifest.SmokeSceneKey, request)
}

func runRuntimeSmoke(ctx context.Context, installation simulation.RuntimeInstallation,
	sceneKey string, requestBody []byte) error {
	parsed, err := url.Parse(installation.Endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" || parsed.Port() == "" {
		return errors.New("本机 Runtime endpoint 必须是带端口的 http URL")
	}
	if parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" && parsed.Hostname() != "::1" {
		return errors.New("本机 Runtime Pack endpoint 只能绑定 loopback")
	}
	if connection, dialErr := net.DialTimeout("tcp", parsed.Host, 250*time.Millisecond); dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("Runtime smoke 端口已被占用: %s", parsed.Host)
	}
	executable, err := simulation.RuntimeRunnerExecutable(installation.EnvironmentPath, installation.Runner)
	if err != nil {
		return err
	}
	logFile, err := os.CreateTemp("", "semantic-runtime-smoke-*.log")
	if err != nil {
		return err
	}
	logPath := logFile.Name()
	defer func() { _ = logFile.Close(); _ = os.Remove(logPath) }()
	command := exec.Command(executable)
	env, err := simulation.RuntimeContentEnvironment(installation.ContentRefs)
	if err != nil {
		return err
	}
	env = append(env, simulation.RuntimeEndpointEnvironment(installation.Endpoint)...)
	env = append(env, "SEMANTIC_SIM_PROFILE="+installation.Profile.RuntimeProfileID,
		"PYTHONUNBUFFERED=1")
	command.Env = append(os.Environ(), env...)
	command.Dir = installation.EnvironmentPath
	command.Stdout, command.Stderr = logFile, logFile
	tree, err := processport.Start(command)
	if err != nil {
		return fmt.Errorf("启动 Runtime smoke 失败: %w", err)
	}
	defer stopSmokeProcess(command, tree)
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(logPath)
			return fmt.Errorf("Runtime healthz 超时: %s", tailText(data, 4096))
		}
		response, getErr := client.Get(installation.Endpoint + "/healthz")
		if getErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	response, err := client.Post(installation.Endpoint+"/api/v1/scenes/"+
		url.PathEscape(sceneKey)+"/instances", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return fmt.Errorf("Runtime smoke 启动场景失败 HTTP %d: %s", response.StatusCode, data)
	}
	var instance struct {
		InstanceID string `json:"instance_id"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal(data, &instance); err != nil || instance.InstanceID == "" {
		return errors.New("Runtime smoke 启动响应缺少 instance_id")
	}
	for time.Now().Before(deadline) {
		current, getErr := client.Get(installation.Endpoint + "/api/v1/scene-instances/" +
			url.PathEscape(instance.InstanceID))
		if getErr == nil {
			body, _ := io.ReadAll(current.Body)
			_ = current.Body.Close()
			if current.StatusCode == http.StatusOK {
				_ = json.Unmarshal(body, &instance)
				if instance.State == "running" {
					break
				}
				if instance.State == "failed" {
					return fmt.Errorf("Runtime smoke 场景加载失败: %s", body)
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if instance.State != "running" {
		return errors.New("Runtime smoke 场景未进入 running")
	}
	stop, err := http.NewRequestWithContext(ctx, http.MethodPost, installation.Endpoint+
		"/api/v1/scene-instances/"+url.PathEscape(instance.InstanceID)+"/stop", nil)
	if err != nil {
		return err
	}
	stopped, err := client.Do(stop)
	if err != nil {
		return err
	}
	defer func() { _ = stopped.Body.Close() }()
	if stopped.StatusCode < 200 || stopped.StatusCode >= 300 {
		return fmt.Errorf("Runtime smoke 停止场景失败 HTTP %d", stopped.StatusCode)
	}
	return nil
}

func stopSmokeProcess(command *exec.Cmd, tree *processport.Tree) {
	defer tree.Close()
	if command == nil || command.Process == nil {
		return
	}
	_ = tree.Terminate()
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		_ = tree.Kill()
		<-done
	}
}

func tailText(data []byte, max int) string {
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return strings.TrimSpace(string(data))
}

func activateRuntimeInstallation(paths runtimePaths, installation simulation.RuntimeInstallation,
	manifest simulation.RuntimePackManifest, packPath string, replace bool) error {
	manifestTarget := filepath.Join(paths.runtimes, installation.InstallationID+".yaml")
	if _, err := os.Stat(manifestTarget); err == nil && !replace {
		return errors.New("Runtime installation 已存在；upgrade 或修复时使用 --replace")
	}
	sceneRoot := filepath.Dir(installation.SceneCatalogPath)
	stage, err := os.MkdirTemp(paths.scenes, ".scene-catalog-stage-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	catalogRoot := filepath.ToSlash(filepath.Dir(manifest.SceneCatalog.Path))
	files := append([]simulation.RuntimePackFile{manifest.SceneCatalog}, manifest.SceneResources...)
	for _, file := range files {
		relative, relErr := filepath.Rel(filepath.FromSlash(catalogRoot), filepath.FromSlash(file.Path))
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("场景资源 %s 必须位于 catalog 根目录 %s", file.Path, catalogRoot)
		}
		if err := copyRegularFile(filepath.Join(packPath, filepath.FromSlash(file.Path)),
			filepath.Join(stage, relative)); err != nil {
			return err
		}
	}
	if _, err := simulation.LoadSceneCatalog(stage); err != nil {
		return fmt.Errorf("安装的场景目录无效: %w", err)
	}
	encoded, err := yaml.Marshal(installation)
	if err != nil {
		return err
	}
	validationFile, err := os.CreateTemp(paths.runtimes, ".runtime-installation-validate-*.yaml")
	if err != nil {
		return err
	}
	validationPath := validationFile.Name()
	defer func() { _ = os.Remove(validationPath) }()
	if _, err := validationFile.Write(encoded); err != nil {
		_ = validationFile.Close()
		return err
	}
	if err := validationFile.Close(); err != nil {
		return err
	}
	if _, err := simulation.LoadRuntimeInstallationFile(validationPath); err != nil {
		return err
	}
	backup := ""
	if _, err := os.Stat(sceneRoot); err == nil {
		backupHandle, backupErr := os.CreateTemp(paths.scenes, ".scene-catalog-previous-")
		if backupErr != nil {
			return backupErr
		}
		backup = backupHandle.Name()
		_ = backupHandle.Close()
		_ = os.Remove(backup)
		if err := os.Rename(sceneRoot, backup); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, sceneRoot); err != nil {
		if backup != "" {
			_ = os.Rename(backup, sceneRoot)
		}
		return err
	}
	if err := writeRuntimeManifestAtomic(paths.runtimes, manifestTarget, encoded); err != nil {
		_ = os.RemoveAll(sceneRoot)
		if backup != "" {
			_ = os.Rename(backup, sceneRoot)
		}
		return err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func copyRegularFile(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("源文件不是普通文件: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func installDevelopmentRuntime(paths runtimePaths, options runtimeInstallOptions) error {
	if options.profileID == "" {
		return errors.New("--dev-source 必须同时提供 --profile")
	}
	source, err := filepath.Abs(options.devSource)
	if err != nil {
		return err
	}
	if info, err := os.Stat(filepath.Join(source, "pyproject.toml")); err != nil || info.IsDir() {
		return errors.New("--dev-source 不是 plugin-mujoco 源码根目录")
	}
	runner := options.profileID
	projectDir := source
	if runner == "robosuite-1.5" {
		projectDir = filepath.Join(source, "profiles", "robosuite")
	}
	if runner == "libero-robosuite-1.4" {
		projectDir = filepath.Join(source, "profiles", "libero")
	}
	if runner != "native-mujoco" && runner != "robosuite-1.5" && runner != "libero-robosuite-1.4" {
		return errors.New("开发模式 profile 不受支持")
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeInstallTimeout)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "uv", "sync", "--project", projectDir,
		"--frozen").CombinedOutput(); err != nil {
		return fmt.Errorf("同步开发 Runtime 环境失败: %w: %s", err, output)
	}
	content, err := resolveRuntimeContent(runner, options)
	if err != nil {
		return err
	}
	installationID := options.installationID
	if installationID == "" {
		installationID = "dev-" + options.profileID
	}
	endpoint := selectRuntimeEndpoint(options.endpoint, "", options.profileID)
	installation := simulation.RuntimeInstallation{
		SchemaVersion: 2, InstallationID: installationID,
		Profile: developmentRuntimeProfile(options.profileID),
		PackID:  "dev-" + options.profileID, PackVersion: "0.4.0-dev", Runner: runner,
		LaunchMode: "process", EnvironmentPath: filepath.Join(projectDir, ".venv"),
		ContentRefs: content, AcceptedLicenses: normalizedStrings(options.acceptedLicenses),
		Development: true, Endpoint: endpoint,
		Enabled: true, InstalledVersion: "development",
	}
	sceneKey, smoke := developmentSmoke(options.profileID)
	if err := runRuntimeSmoke(ctx, installation, sceneKey, smoke); err != nil {
		return err
	}
	if options.sceneCatalog != "" {
		catalog, absErr := filepath.Abs(options.sceneCatalog)
		if absErr != nil {
			return absErr
		}
		info, statErr := os.Stat(catalog)
		if statErr != nil || !info.IsDir() {
			return errors.New("--scene-catalog 必须指向包含 catalog YAML 与 authoring 资源的目录")
		}
		targetRoot := filepath.Join(paths.scenes, installationID)
		if err := copyDevelopmentCatalog(catalog, targetRoot, options.replace); err != nil {
			return err
		}
		loaded, loadErr := simulation.LoadSceneCatalog(targetRoot)
		if loadErr != nil {
			_ = os.RemoveAll(targetRoot)
			return loadErr
		}
		if len(loaded.List(options.profileID)) == 0 {
			_ = os.RemoveAll(targetRoot)
			return errors.New("开发场景目录没有兼容条目")
		}
		installation.SceneCatalogPath = filepath.Join(targetRoot, "catalog.yaml")
	}
	encoded, err := yaml.Marshal(installation)
	if err != nil {
		return err
	}
	target := filepath.Join(paths.runtimes, installationID+".yaml")
	if _, statErr := os.Stat(target); statErr == nil && !options.replace {
		return errors.New("开发 Runtime 已存在；修复时使用 --replace")
	}
	if err := writeRuntimeManifestAtomic(paths.runtimes, target, encoded); err != nil {
		return err
	}
	fmt.Printf("✓ 开发 Runtime %s 已安装并标记 development；不得用于 RC 验收\n", installationID)
	return nil
}

func copyDevelopmentCatalog(source, target string, replace bool) error {
	if _, err := os.Stat(target); err == nil && !replace {
		return errors.New("开发场景目录已存在；请先使用同一 installation 修复流程")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".dev-scene-catalog-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(source, path)
		if relErr != nil {
			return relErr
		}
		if relative == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("开发场景目录不能包含符号链接: %s", relative)
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(stage, relative), 0o700)
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".yaml" && extension != ".yml" && extension != ".json" &&
			extension != ".svg" && extension != ".png" && extension != ".webp" {
			return fmt.Errorf("开发场景目录包含不支持的文件: %s", relative)
		}
		return copyRegularFile(path, filepath.Join(stage, relative))
	})
	if err != nil {
		return err
	}
	if _, err := simulation.LoadSceneCatalog(stage); err != nil {
		return err
	}
	backup := ""
	if _, err := os.Stat(target); err == nil {
		backup = target + ".previous"
		_ = os.RemoveAll(backup)
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, target); err != nil {
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func profileLoader(profileID string) string {
	if profileID == "native-mujoco" {
		return "native"
	}
	if profileID == "robosuite-1.5" {
		return "robosuite"
	}
	return "libero"
}

func developmentRuntimeProfile(profileID string) simulation.RuntimeProfile {
	profile := simulation.RuntimeProfile{
		RuntimeProfileID: profileID, Name: profileID + " (development)",
		Engine: "mujoco", Loader: profileLoader(profileID), APIVersion: "v1",
		Environment: "development-source", EnvironmentReady: true,
		Available: true, AvailabilityKnown: true,
		Capabilities: simulation.RuntimeCapability{
			Viewer: true, ViewerCameraModes: []string{"fixed"}, SceneStep: true,
			SceneReset: true, SensorKinds: []string{"rgb", "depth", "contact", "holding"},
		},
	}
	switch profileID {
	case "native-mujoco":
		profile.SceneKinds = []string{"scene_document", "asset_scene"}
		profile.Capabilities.EditableScene = true
		profile.Capabilities.ViewerCameraModes = []string{"free", "fixed"}
		profile.Capabilities.RobotModels = []string{"r1_pro_chassis"}
	case "robosuite-1.5":
		profile.SceneKinds = []string{"robosuite-task"}
		profile.Capabilities.NativeEvaluator = true
		profile.Capabilities.RobotModels = []string{"franka_panda"}
	case "libero-robosuite-1.4":
		profile.SceneKinds = []string{"libero-task", "libero-pro-evaluation"}
		profile.Capabilities.NativeEvaluator = true
		profile.Capabilities.RobotModels = []string{"franka_panda"}
	}
	return profile
}

func developmentSmoke(profileID string) (string, []byte) {
	scene := "palletizing_depalletizing_001"
	layout := "layout001"
	if profileID == "robosuite-1.5" {
		scene, layout = "Lift", "default"
	}
	if profileID == "libero-robosuite-1.4" {
		scene, layout = "libero_spatial:0", "init-state-0"
	}
	body, _ := json.Marshal(map[string]any{"request_id": "semantic-runtime-install-smoke",
		"layout": layout, "seed": 7, "headless": true})
	return scene, body
}

func runRuntimeDoctor(args []string) int {
	flags := flag.NewFlagSet("runtime doctor", flag.ContinueOnError)
	configFlag := flags.String("c", "", "配置文件路径")
	id := flags.String("id", "", "只检查一个 installation")
	all := flags.Bool("all", false, "检查全部 installation")
	smoke := flags.Bool("smoke", false, "离线 Runtime 执行真实启动 smoke")
	release := flags.Bool("release", false, "作为 RC/正式制品检查，拒绝 development 安装")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *id == "" && !*all {
		*all = true
	}
	paths, err := runtimeInstallPaths(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	catalog, err := simulation.LoadRuntimeInstallations(paths.runtimes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	items := catalog.List()
	if len(items) == 0 {
		fmt.Println("没有已安装的 Runtime；请先运行 semantic runtime install")
		return 0
	}
	if *id != "" {
		item, getErr := catalog.Get(*id)
		if getErr != nil {
			fmt.Fprintln(os.Stderr, getErr)
			return 1
		}
		items = []simulation.RuntimeInstallation{item}
	}
	failed := false
	for _, item := range items {
		if err := diagnoseRuntimeInstallation(item, *smoke, *release); err != nil {
			failed = true
			fmt.Printf("✗ %s: %v\n", item.InstallationID, err)
		} else {
			fmt.Printf("✓ %s: 配置、环境、内容与健康检查通过\n", item.InstallationID)
		}
	}
	if failed {
		return 1
	}
	return 0
}

func diagnoseRuntimeInstallation(
	item simulation.RuntimeInstallation, smoke, release bool,
) error {
	if !item.Enabled {
		return errors.New("installation 已停用")
	}
	if item.Diagnostic != "" {
		return errors.New(item.Diagnostic)
	}
	if release && item.Development {
		return errors.New("development Runtime 不得用于 RC 或正式制品验收")
	}
	if release && strings.Contains(item.PackVersion, "-dev.") {
		return errors.New("开发版本 Runtime Pack 不得用于 RC 或正式制品验收")
	}
	executable, err := simulation.RuntimeRunnerExecutable(item.EnvironmentPath, item.Runner)
	if err != nil {
		return err
	}
	if info, err := os.Stat(executable); err != nil || !platform.Runnable(info) {
		return errors.New("Runtime 入口不存在或不可执行")
	}
	for key, path := range item.ContentRefs {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return fmt.Errorf("content_ref %s 损坏", key)
		}
	}
	if !item.Development {
		manifest, err := simulation.LoadRuntimePack(item.PackPath)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := validateContentRequirements(ctx, item.ContentRefs,
			manifest.ContentRequirements, item.AcceptedLicenses); err != nil {
			return err
		}
		if smoke {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return runInstalledRuntimeSmoke(ctx, item, manifest, item.PackPath)
		}
	} else if smoke {
		sceneKey, request := developmentSmoke(item.Profile.RuntimeProfileID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return runRuntimeSmoke(ctx, item, sceneKey, request)
	}
	return nil
}

func runRuntimeTestStart(args []string) int {
	args = append(args, "--smoke")
	return runRuntimeDoctor(args)
}

func runRuntimeUpgrade(args []string) int {
	options, reference, err := parseRuntimeInstallOptions("runtime upgrade", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if options.installationID == "" {
		fmt.Fprintln(os.Stderr, "upgrade 必须提供 --id")
		return 2
	}
	paths, err := runtimeInstallPaths(options.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	catalog, err := simulation.LoadRuntimeInstallations(paths.runtimes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	current, err := catalog.Get(options.installationID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if current.Development {
		fmt.Fprintln(os.Stderr, "development Runtime 不能使用正式 Pack upgrade；请用 install --dev-source --replace")
		return 1
	}
	options.expectedProfileID = current.Profile.RuntimeProfileID
	applyExistingRuntimeContent(&options, current)
	options.replace = true
	if err := installRuntimePack(options, reference); err != nil {
		fmt.Fprintln(os.Stderr, "Runtime 升级失败，旧安装保持不变:", err)
		return 1
	}
	return 0
}

func applyExistingRuntimeContent(
	options *runtimeInstallOptions, current simulation.RuntimeInstallation,
) {
	if options.assetRoot == "" {
		options.assetRoot = current.ContentRefs["mujoco_assets"]
	}
	if options.modelRoot == "" {
		options.modelRoot = current.ContentRefs["franka_model"]
		if options.modelRoot == "" {
			options.modelRoot = current.ContentRefs["r1pro_model"]
		}
	}
	if options.liberoRoot == "" {
		options.liberoRoot = current.ContentRefs["libero_source"]
	}
	if options.liberoProRoot == "" {
		options.liberoProRoot = current.ContentRefs["libero_pro_source"]
	}
	if options.endpoint == "" {
		options.endpoint = current.Endpoint
	}
	if len(options.acceptedLicenses) == 0 {
		options.acceptedLicenses = append(options.acceptedLicenses, current.AcceptedLicenses...)
	}
}

func normalizedStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func runRuntimeUninstall(args []string) int {
	flags := flag.NewFlagSet("runtime uninstall", flag.ContinueOnError)
	configFlag := flags.String("c", "", "配置文件路径")
	id := flags.String("id", "", "installation_id")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if !installationIDValid(*id) {
		fmt.Fprintln(os.Stderr, "--id 必填且格式必须有效")
		return 2
	}
	paths, err := runtimeInstallPaths(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	catalog, err := simulation.LoadRuntimeInstallations(paths.runtimes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	item, err := catalog.Get(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if count, err := countRuntimeProjectBindings(paths.config.Store, *id); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	} else if count > 0 {
		fmt.Fprintf(os.Stderr, "Runtime 被 %d 个 Project 永久绑定，拒绝卸载；可以停用并修复同一 installation_id\n", count)
		return 1
	}
	if runtimeEndpointOnline(item.Endpoint) {
		fmt.Fprintln(os.Stderr, "Runtime 仍在线；请先退出 Project/停止 Server 后卸载")
		return 1
	}
	manifestPath := filepath.Join(paths.runtimes, *id+".yaml")
	if err := os.Remove(manifestPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if item.SceneCatalogPath != "" && pathInside(paths.scenes, filepath.Dir(item.SceneCatalogPath)) {
		_ = os.RemoveAll(filepath.Dir(item.SceneCatalogPath))
	}
	if item.EnvironmentPath != "" && pathInside(paths.envs, item.EnvironmentPath) {
		_ = os.RemoveAll(item.EnvironmentPath)
	}
	if !item.Development && item.PackPath != "" && pathInside(paths.packs, item.PackPath) &&
		!packUsedByOtherInstallation(catalog.List(), item) {
		_ = os.RemoveAll(item.PackPath)
	}
	fmt.Printf("✓ Runtime %s 已卸载；外部资产、模型和 benchmark 数据未删除\n", *id)
	return 0
}

func countRuntimeProjectBindings(cfg config.StoreConfig, installationID string) (int, error) {
	if _, err := os.Stat(cfg.SQLitePath); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	logger := semanticlog.New(semanticlog.Options{Level: semanticlog.LevelError, Writer: io.Discard})
	storage, err := store.Open(cfg, logger)
	if err != nil {
		return 0, err
	}
	defer func() { _ = storage.Close() }()
	return storage.CountProjectsUsingRuntime(installationID)
}

func runtimeEndpointOnline(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	connection, err := net.DialTimeout("tcp", parsed.Host, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func pathInside(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func packUsedByOtherInstallation(items []simulation.RuntimeInstallation,
	removed simulation.RuntimeInstallation) bool {
	for _, item := range items {
		if item.InstallationID != removed.InstallationID && item.PackPath == removed.PackPath {
			return true
		}
	}
	return false
}
