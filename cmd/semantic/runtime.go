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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/pkg/config"
)

// runRuntime 管理 Server 端 RuntimeInstallation 清单。它只读写固定 YAML，
// 不执行 manifest 中的 command，也不会从浏览器或 CLI 安装 Python/容器依赖。
func runRuntime(args []string) int {
	if len(args) == 0 {
		runtimeUsage()
		return 2
	}
	switch args[0] {
	case "install":
		return runRuntimeInstall(args[1:])
	case "doctor":
		return runRuntimeDoctor(args[1:])
	case "upgrade":
		return runRuntimeUpgrade(args[1:])
	case "uninstall":
		return runRuntimeUninstall(args[1:])
	case "test-start":
		return runRuntimeTestStart(args[1:])
	case "register":
		return runRuntimeRegister(args[1:])
	case "check":
		return runRuntimeCheck(args[1:])
	case "remove":
		// 旧命令保留为兼容别名，但必须走带 Project 绑定检查的卸载流程。
		return runRuntimeUninstall(args[1:])
	case "list":
		return runRuntimeList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知 runtime 子命令 %q\n", args[0])
		runtimeUsage()
		return 2
	}
}

func runtimeUsage() {
	fmt.Fprintln(os.Stderr, `用法:
  semantic runtime install <pack@version> [--asset-root 路径]
  semantic runtime install --pack 制品.runtime.tar.zst [内容路径]
  semantic runtime install --dev-source plugin-mujoco --profile <profile> [内容路径]
  semantic runtime doctor [-c 配置文件] [--all | --id installation_id] [--smoke] [--release]
  semantic runtime upgrade --id installation_id <pack@version|--pack 文件>
  semantic runtime uninstall --id installation_id
  semantic runtime test-start --id installation_id
  semantic runtime list [-c 配置文件]
  semantic runtime check [-c 配置文件] [--id installation_id | --file manifest.yaml]
  semantic runtime register [-c 配置文件] --file manifest.yaml [--replace]
  semantic runtime remove [-c 配置文件] --id installation_id  # uninstall 的兼容别名

install/upgrade 只执行 Framework 内建的 uv 与 Runtime 入口，不执行 Pack 中的 Shell。
外部资产与 benchmark 数据始终只读引用，uninstall 不删除这些内容。`)
}

func runtimeDirectory(configFlag string) (string, error) {
	configPath, err := config.ResolvePath(configFlag)
	if err != nil {
		return "", fmt.Errorf("解析配置路径失败: %w", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(cfg.Simulation.RuntimesDir)
	if dir == "" {
		return "", fmt.Errorf("simulation.runtimes_dir 未配置")
	}
	return filepath.Abs(dir)
}

func runRuntimeRegister(args []string) int {
	fs := flag.NewFlagSet("runtime register", flag.ContinueOnError)
	configFlag := fs.String("c", "", "配置文件路径")
	source := fs.String("file", "", "待注册的 Runtime manifest")
	replace := fs.Bool("replace", false, "修复同一 installation_id 时替换已有 manifest")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*source) == "" {
		fmt.Fprintln(os.Stderr, "--file 必填")
		return 2
	}
	installation, err := simulation.LoadRuntimeInstallationFile(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Runtime manifest 校验失败:", err)
		return 1
	}
	dir, err := runtimeDirectory(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 Runtime 配置失败:", err)
		return 1
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "创建 runtimes.d 失败:", err)
		return 1
	}
	target := filepath.Join(dir, installation.InstallationID+".yaml")
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() {
			fmt.Fprintln(os.Stderr, "目标 manifest 不是普通文件，拒绝覆盖:", target)
			return 1
		}
		if !*replace {
			fmt.Fprintln(os.Stderr, "Runtime 已注册；修复同一 ID 时显式使用 --replace:", installation.InstallationID)
			return 1
		}
	} else if !os.IsNotExist(statErr) {
		fmt.Fprintln(os.Stderr, "检查目标 manifest 失败:", statErr)
		return 1
	}
	data, err := os.ReadFile(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 manifest 失败:", err)
		return 1
	}
	if err := writeRuntimeManifestAtomic(dir, target, data); err != nil {
		fmt.Fprintln(os.Stderr, "写入 manifest 失败:", err)
		return 1
	}
	fmt.Printf("✓ Runtime %s 已注册到 %s（状态 %s）\n",
		installation.InstallationID, target, installation.Status)
	if installation.Diagnostic != "" {
		fmt.Printf("! 环境尚未就绪: %s\n", installation.Diagnostic)
	}
	return 0
}

func writeRuntimeManifestAtomic(dir, target string, data []byte) error {
	temp, err := os.CreateTemp(dir, ".runtime-manifest-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o640); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}

func runRuntimeCheck(args []string) int {
	fs := flag.NewFlagSet("runtime check", flag.ContinueOnError)
	configFlag := fs.String("c", "", "配置文件路径")
	id := fs.String("id", "", "只检查一个 installation_id")
	source := fs.String("file", "", "只检查一个外部 manifest")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id != "" && *source != "" {
		fmt.Fprintln(os.Stderr, "--id 与 --file 不能同时使用")
		return 2
	}
	if *source != "" {
		item, err := simulation.LoadRuntimeInstallationFile(*source)
		if err != nil {
			fmt.Fprintln(os.Stderr, "✗", err)
			return 1
		}
		return printRuntimeCheck(item)
	}
	dir, err := runtimeDirectory(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 Runtime 配置失败:", err)
		return 1
	}
	catalog, err := simulation.LoadRuntimeInstallations(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载 Runtime 清单失败:", err)
		return 1
	}
	if *id != "" {
		item, getErr := catalog.Get(*id)
		if getErr != nil {
			fmt.Fprintln(os.Stderr, "✗", getErr)
			return 1
		}
		return printRuntimeCheck(item)
	}
	failed := false
	for _, item := range catalog.List() {
		if printRuntimeCheck(item) != 0 {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

func printRuntimeCheck(item simulation.RuntimeInstallation) int {
	if !item.Enabled {
		fmt.Printf("! %s 已停用\n", item.InstallationID)
		return 0
	}
	if item.Diagnostic != "" || item.Status == "failed" {
		fmt.Printf("✗ %s (%s/%s): %s\n", item.InstallationID,
			item.Profile.Engine, item.Profile.Loader, item.Diagnostic)
		return 1
	}
	fmt.Printf("✓ %s (%s/%s) manifest 有效，等待 Server probe\n",
		item.InstallationID, item.Profile.Engine, item.Profile.Loader)
	return 0
}

func runRuntimeList(args []string) int {
	fs := flag.NewFlagSet("runtime list", flag.ContinueOnError)
	configFlag := fs.String("c", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, err := runtimeDirectory(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 Runtime 配置失败:", err)
		return 1
	}
	catalog, err := simulation.LoadRuntimeInstallations(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载 Runtime 清单失败:", err)
		return 1
	}
	for _, item := range catalog.Views() {
		fmt.Printf("%s\t%s\t%s/%s\t%s\n", item.InstallationID,
			item.ProfileID, item.Engine, item.Loader, item.Status)
	}
	return 0
}

func runRuntimeRemove(args []string) int {
	fs := flag.NewFlagSet("runtime remove", flag.ContinueOnError)
	configFlag := fs.String("c", "", "配置文件路径")
	id := fs.String("id", "", "installation_id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		fmt.Fprintln(os.Stderr, "--id 必填")
		return 2
	}
	dir, err := runtimeDirectory(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 Runtime 配置失败:", err)
		return 1
	}
	catalog, err := simulation.LoadRuntimeInstallations(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载 Runtime 清单失败:", err)
		return 1
	}
	item, err := catalog.Get(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	relative, err := filepath.Rel(dir, item.SourceFile)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		fmt.Fprintln(os.Stderr, "拒绝删除 runtimes.d 之外的路径")
		return 1
	}
	if err := os.Remove(item.SourceFile); err != nil {
		fmt.Fprintln(os.Stderr, "移除 Runtime manifest 失败:", err)
		return 1
	}
	fmt.Printf("✓ 已移除 Runtime manifest %s；已有 Project 绑定会保留并显示不可用\n", item.InstallationID)
	return 0
}
