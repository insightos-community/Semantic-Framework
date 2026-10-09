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

package install

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SourceBuild 是开发期配方，允许显式列出同工作区内的依赖源码。发布的组件包
// 只带锁定 requirements 与 Wheel，不携带开发机路径，也不在安装时重新构建。
type SourceBuild struct {
	Component `yaml:",inline"`
	Build     struct {
		PythonProjects     []string          `yaml:"python_projects"`
		Extras             []string          `yaml:"extras"`
		AbilityDirectories map[string]string `yaml:"ability_directories"`
		Files              map[string]string `yaml:"files"`
		RequirementsLock   string            `yaml:"requirements_lock"`
		Offline            bool              `yaml:"offline"`
	} `yaml:"build"`
}

func BuildSource(ctx context.Context, source, destination string, progress func(string)) error {
	cache := os.Getenv("SEMANTIC_BUILD_CACHE")
	if cache == "" {
		cache = filepath.Join(".output", "cache", "component-build")
	}
	return buildSource(ctx, source, destination, cache, progress)
}

func buildSource(ctx context.Context, source, destination, cache string, progress func(string)) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(source, "semantic-source.yaml"))
	if err != nil {
		return err
	}
	var recipe SourceBuild
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&recipe); err != nil {
		return err
	}
	fingerprint, err := sourceFingerprint(source, recipe)
	if err != nil {
		return err
	}
	cached := filepath.Join(cache, fingerprint+".zip")
	if _, err := os.Stat(cached); err == nil {
		progress("源码与依赖未变，复用已构建组件")
		return copyBuildFile(cached, destination)
	}
	staging, err := os.MkdirTemp("", "semantic-component-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	c := recipe.Component
	c.SourceRevision = fingerprint
	for target, relative := range recipe.Build.Files {
		if !safeRelative(target) {
			return fmt.Errorf("构建输出路径无效: %s", target)
		}
		path := filepath.Join(source, relative)
		target = filepath.Join(staging, target)
		if info, err := os.Stat(path); err != nil {
			return err
		} else if info.IsDir() {
			if err := os.MkdirAll(target, 0750); err != nil {
				return err
			}
			if err := os.CopyFS(target, os.DirFS(path)); err != nil {
				return err
			}
		} else {
			if err := copyBuildFile(path, target); err != nil {
				return err
			}
		}
	}
	if c.Python != nil {
		if len(recipe.Build.PythonProjects) == 0 {
			return fmt.Errorf("源码 Ability 需要 build.python_projects")
		}
		wheels := filepath.Join(staging, "wheels")
		if err := os.MkdirAll(wheels, 0750); err != nil {
			return err
		}
		// 只使用固定构建命令。源码构建本来就会运行 Python build backend，CLI 调用
		// 是开发者的显式选择；Web 只接收构建后的包并在确认安装后创建环境。
		for _, relative := range recipe.Build.PythonProjects {
			path := filepath.Join(source, relative)
			if strings.HasSuffix(path, ".whl") {
				if err := copyBuildFile(path, filepath.Join(wheels, filepath.Base(path))); err != nil {
					return err
				}
				continue
			}
			if err := RunCommand(ctx, progress, "uv", "build", "--wheel", "--python", c.Python.Version, "--out-dir", wheels, path); err != nil {
				return err
			}
		}
		requirements := filepath.Join(staging, "requirements.lock")
		if recipe.Build.RequirementsLock != "" {
			if err := copyBuildFile(filepath.Join(source, recipe.Build.RequirementsLock), requirements); err != nil {
				return err
			}
		} else if _, err := os.Stat(filepath.Join(source, "uv.lock")); err == nil {
			args := []string{"export", "--project", source, "--frozen", "--no-dev", "--no-emit-local", "--no-hashes", "--output-file", requirements}
			for _, extra := range recipe.Build.Extras {
				args = append(args, "--extra", extra)
			}
			if err := RunCommand(ctx, progress, "uv", args...); err != nil {
				return err
			}
		} else {
			// 没有 uv.lock 的项目在构建时解析并产出锁文件，安装端始终只消费该锁。
			args := []string{"pip", "compile", "--python-version", c.Python.Version, "--find-links", wheels, "--output-file", requirements, filepath.Join(source, "pyproject.toml")}
			for _, extra := range recipe.Build.Extras {
				args = append(args, "--extra", extra)
			}
			if err := RunCommand(ctx, progress, "uv", args...); err != nil {
				return err
			}
		}
		buildEnv := filepath.Join(staging, "builder")
		if err := RunCommand(ctx, progress, "uv", "venv", "--seed", "--python", c.Python.Version, buildEnv); err != nil {
			return err
		}
		seeds := wheelSeeds()
		offline := recipe.Build.Offline || os.Getenv("SEMANTIC_BUILD_OFFLINE") == "1"
		// 先用 curl 收集器把依赖落到 wheels/：它从种子目录硬链接已有 Wheel，只下载
		// 缺失部分。实测 pip 在 GB 级文件上会连接僵死（317 MB 的 cublas 卡 25 分钟），
		// 且 --find-links 只是补充来源——有种子时 pip 仍会联网校验版本并重新下载。
		collected := false
		if !offline {
			collected = runCurlWheelCollector(ctx, progress, requirements, wheels, c.Python.Version, seeds)
		}
		downloadArgs := []string{"-m", "pip", "wheel", "--find-links", wheels, "-r", requirements, "--wheel-dir", wheels}
		for _, seed := range seeds {
			downloadArgs = append(downloadArgs, "--find-links", seed)
		}
		if offline || collected {
			// 依赖已在本地（种子 + curl 收集），禁止 pip 联网：否则它会重新下载大文件。
			downloadArgs = append(downloadArgs, "--no-index")
		} else {
			// curl 不可用，退回 pip 联网收集；加超时避免无限等待。
			downloadArgs = append(downloadArgs, "--timeout", "60", "--retries", "5")
		}
		if err := RunCommand(ctx, progress, filepath.Join(buildEnv, "bin", "python"), downloadArgs...); err != nil {
			return err
		}
		// pip 会把 --find-links 里另一平台标签的同版本 Wheel 也拷进 --wheel-dir，
		// 下游安装会因其判定版本冲突；收集后统一去重。
		runWheelDedup(ctx, progress, wheels, c.Python.Version)
		entries, err := os.ReadDir(wheels)
		if err != nil {
			return err
		}
		c.Python.Requirements = "requirements.lock"
		c.Python.Wheels = nil
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".whl") {
				c.Python.Wheels = append(c.Python.Wheels, "wheels/"+entry.Name())
			}
		}
		// 构建工具环境仅用于收集制品，交付时不打包虚拟环境及其绝对 shebang。
		if err := os.RemoveAll(buildEnv); err != nil {
			return err
		}
	}
	for index, a := range c.Abilities {
		dir, ok := recipe.Build.AbilityDirectories[a.Role]
		if !ok {
			return fmt.Errorf("缺少 %s 的 Ability 源码目录", a.Role)
		}
		if !safeRelative(a.Role) || strings.Contains(a.Role, "/") {
			return fmt.Errorf("Ability role 无效")
		}
		relative := "abilities/" + a.Role + ".zip"
		if err := ZipDirectory(filepath.Join(source, dir), filepath.Join(staging, relative)); err != nil {
			return err
		}
		c.Abilities[index].Package = relative
	}
	if c.Kind == "robot_base" {
		if err := placeRobotBuildArtifacts(staging, &c); err != nil {
			return err
		}
	}
	if c.Kind == "runtime" {
		if err := finalizeRuntimeBuild(staging, c); err != nil {
			return err
		}
	} else {
		data, err = yaml.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := ParseComponent(data); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(staging, "semantic-component.yaml"), data, 0640); err != nil {
			return err
		}
	}
	if after, err := sourceFingerprint(source, recipe); err != nil {
		return err
	} else if after != fingerprint {
		return fmt.Errorf("构建期间源码发生变化，请重新构建")
	}
	if err := ZipDirectory(staging, cached); err != nil {
		return err
	}
	return copyBuildFile(cached, destination)
}

// wheelSeeds 返回可复用的 Wheel 种子目录。依赖版本仍由锁文件决定，种子只提供
// 来源，不改变解析结果。来源按优先级：
//  1. 显式 SEMANTIC_BUILD_WHEELHOUSE
//  2. 共享 wheelhouse（SEMANTIC_WHEELHOUSE 或 /data/wheelhouse，含其子目录）
//  3. 本工作区 .output 下的既有 Wheel 目录（产物 5 的 franka 构建缓存、通用
//     wheelhouse），使同一批依赖闭包只下载一次
func wheelSeeds() []string {
	var seeds []string
	if seed := os.Getenv("SEMANTIC_BUILD_WHEELHOUSE"); seed != "" {
		seeds = append(seeds, seed)
	}
	// SEMANTIC_WHEELHOUSE 显式设置时只用它（便于隔离测试与自定义部署）；
	// 未设置才回退到约定路径 /data/wheelhouse。
	bases := []string{"/data/wheelhouse"}
	if explicit, ok := os.LookupEnv("SEMANTIC_WHEELHOUSE"); ok {
		bases = []string{explicit}
	}
	for _, base := range bases {
		if base == "" {
			continue
		}
		root := filepath.Clean(base)
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		if hasWheel(root) {
			seeds = append(seeds, root)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				sub := filepath.Join(root, entry.Name())
				if hasWheel(sub) {
					seeds = append(seeds, sub)
				}
			}
		}
	}
	// 本工作区 .output 下的既有 Wheel 目录：产物 5 收集过的 franka 依赖闭包、
	// 以及约定的共享 wheelhouse，避免产物 3 重复下载同一批 GB 级依赖。
	for _, candidate := range []string{
		filepath.Join(".output", "franka-bundle-build", "build", "wheels"),
		filepath.Join(".output", "wheelhouse"),
	} {
		if hasWheel(candidate) {
			seeds = append(seeds, candidate)
		}
	}
	return seeds
}

func hasWheel(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".whl") {
			return true
		}
	}
	return false
}

// runCurlWheelCollector 调用共享的 curl Wheel 下载器（scripts/curl_wheel.py）把依赖
// 落到 wheels/：从 seeds 硬链接已有 Wheel，只下载缺失部分。返回是否成功——成功则
// 调用方可用 --no-index 让 pip 完全离线，避免它联网重新下载大文件。
func runCurlWheelCollector(ctx context.Context, progress func(string), requirements, wheels, python string, seeds []string) bool {
	script, err := locateCurlWheel()
	if err != nil {
		progress("未找到 curl 下载器，改用 pip 收集: " + err.Error())
		return false
	}
	index := os.Getenv("UV_DEFAULT_INDEX")
	if index == "" {
		index = os.Getenv("PIP_INDEX_URL")
	}
	args := []string{script, "--requirements", requirements, "--dest", wheels, "--python", python}
	for _, seed := range seeds {
		args = append(args, "--seed", seed)
	}
	if index != "" {
		args = append(args, "--index", index)
	}
	progress("预取依赖（curl 下载器，规避 pip 大文件僵死）")
	if err := RunCommand(ctx, progress, "python3", args...); err != nil {
		progress("curl 下载器未完成，剩余依赖交由 pip 处理")
		return false
	}
	return true
}

// runWheelDedup 调用共享脚本的 --dedup-only 模式，清除同版本的平台标签变体。
func runWheelDedup(ctx context.Context, progress func(string), wheels, python string) {
	script, err := locateCurlWheel()
	if err != nil {
		return
	}
	args := []string{script, "--dedup-only", "--dest", wheels, "--python", python}
	if err := RunCommand(ctx, progress, "python3", args...); err != nil {
		progress("Wheel 去重跳过: " + err.Error())
	}
}

// locateCurlWheel 定位 scripts/curl_wheel.py：优先 $SEMANTIC 工作区，其次可执行文件
// 与当前目录的各级父目录（覆盖仓库并排与嵌套两种布局）。
func locateCurlWheel() (string, error) {
	var roots []string
	if semantic := os.Getenv("SEMANTIC"); semantic != "" {
		roots = append(roots, semantic)
	}
	if executable, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(executable))
	}
	if cwd, err := os.Getwd(); err == nil {
		roots = append(roots, cwd)
	}
	for _, root := range roots {
		dir := root
		for {
			candidate := filepath.Join(dir, "scripts", "curl_wheel.py")
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate, nil
			}
			candidate = filepath.Join(dir, "semantic-framework", "scripts", "curl_wheel.py")
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("scripts/curl_wheel.py 不存在（请确认工作区含 semantic-framework 仓）")
}

// 运行支持包继续使用原安装器的独立 robot 目录。源码构建产生的 Wheel 和
// Ability 同样放入该目录，使 bundle 的相对引用与导出已构建包时保持一致。
// 外层依赖锁及组件收据仍归组件安装器管理，不改变具体机器人的运行配置。
func placeRobotBuildArtifacts(staging string, c *Component) error {
	root := filepath.Dir(c.BundleManifest)
	if root == "." || !safeRelative(root) {
		return fmt.Errorf("bundle_manifest 请放在独立的 robot 子目录")
	}
	if err := os.MkdirAll(filepath.Join(staging, root), 0750); err != nil {
		return err
	}
	for _, directory := range []string{"wheels", "abilities"} {
		if _, err := os.Stat(filepath.Join(staging, directory)); os.IsNotExist(err) {
			continue
		}
		if err := os.Rename(filepath.Join(staging, directory), filepath.Join(staging, root, directory)); err != nil {
			return err
		}
	}
	if c.Python != nil {
		for i, file := range c.Python.Wheels {
			c.Python.Wheels[i] = filepath.ToSlash(filepath.Join(root, file))
		}
	}
	for i, ability := range c.Abilities {
		c.Abilities[i].Package = filepath.ToSlash(filepath.Join(root, ability.Package))
	}
	return nil
}

func copyBuildFile(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("构建输入不是普通文件: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// 固定顺序、固定 ZIP 时间戳，相同制品产生相同内容身份。源码目录忽略开发环境、
// 版本库和缓存，显式的 files 配方负责决定大型资产是否进入交付包。
func ZipDirectory(root, destination string) error {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if skipBuildEntry(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("源码包含非普通文件: %s", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	if err := os.MkdirAll(filepath.Dir(destination), 0750); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".component-zip-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	writer := zip.NewWriter(file)
	copyFiles := func() error {
		for _, path := range paths {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			header := zip.FileHeader{Name: filepath.ToSlash(relative), Method: zip.Deflate}
			if extension := filepath.Ext(path); extension == ".whl" || extension == ".zip" || extension == ".zst" {
				header.Method = zip.Store
			}
			header.SetMode(info.Mode())
			out, err := writer.CreateHeader(&header)
			if err != nil {
				return err
			}
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			_ = in.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}
	copyErr := copyFiles()
	zipErr := writer.Close()
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if zipErr != nil {
		return zipErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), destination)
}
