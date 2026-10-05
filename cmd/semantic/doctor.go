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
	"errors"
	"flag"
	"fmt"
	"net"
	"os"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/llm"
)

// runDoctor 执行启动前自检，逐项输出 ✓/✗/!；存在 ✗ 项时返回 1，否则返回 0。
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	// doctor 与 Server 复用同一套路径优先级，避免检查的文件与实际启动文件不同。
	configFlag := fs.String("c", "", "配置文件路径（默认读取 semantic init 的安装配置）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	configPath, err := config.ResolvePath(*configFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析配置路径失败:", err)
		return 1
	}

	failed := false

	// ⓪ .env 加载状态（结果取 main 早期的加载，避免二次加载误报键数）。
	// 未找到文件仅是提示（!），解析失败才是 ✗。
	if checkDotEnv() {
		failed = true
	}

	// ① 配置文件可加载且通过 schema 校验；端口与 LLM 检查依赖配置内容，
	// 配置加载失败时跳过这两项检查。
	cfg, err := config.Load(configPath)
	if err != nil {
		printConfigLoadFailure(configPath, err)
		failed = true
	} else {
		fmt.Printf("✓ 配置文件 %s 可加载，schema 校验通过\n", configPath)

		// ② 配置端口未被占用
		for _, addr := range []string{cfg.Server.HTTPAddr, cfg.Server.WSAddr} {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				fmt.Printf("✗ 端口 %s 已被占用: %v\n", addr, err)
				failed = true
				continue
			}
			_ = ln.Close()
			fmt.Printf("✓ 端口 %s 空闲\n", addr)
		}

		// ③ LLM 端点密钥逐项检查：注册表本身非法（如 default 不在清单）
		// 是配置错误，记 ✗；单个端点缺 key 仅警告（!），不阻塞启动。
		if checkLLMKeys(cfg.LLM) {
			failed = true
		}
	}

	if failed {
		return 1
	}
	return 0
}

// checkDotEnv 输出 .env 加载状态检查项，返回是否存在 ✗。
func checkDotEnv() bool {
	if bootDotEnv.err != nil {
		fmt.Printf("✗ .env 加载失败: %v\n", bootDotEnv.err)
		fmt.Println("  修复建议：检查 .env 文件格式（每行 KEY=VALUE，# 为注释）")
		return true
	}
	if len(bootDotEnv.result.Files) == 0 {
		fmt.Println("! 未找到 .env 文件（./.env 与 ~/.semantic/.env），密钥需由进程环境变量提供")
		return false
	}
	for _, f := range bootDotEnv.result.Files {
		fmt.Printf("✓ .env 已加载 %s（%d 个键）\n", f.Path, len(f.Keys))
	}
	return false
}

// printConfigLoadFailure 区分 schema 校验失败与其他加载失败：
// schema 问题逐条列出并给出修复建议。
func printConfigLoadFailure(path string, err error) {
	var verr *config.ValidationError
	if !errors.As(err, &verr) {
		fmt.Printf("✗ 配置文件 %s 加载失败: %v\n", path, err)
		return
	}
	fmt.Printf("✗ 配置 schema 校验失败（%d 处问题）:\n", len(verr.Problems))
	for _, problem := range verr.Problems {
		fmt.Printf("    - %s\n", problem)
	}
	fmt.Println("  修复建议：对照 docs/developer/06-configuration-reference.md 检查键名拼写与字段类型")
}

// checkLLMKeys 按 providers 逐项检查密钥环境变量，返回是否存在 ✗ 项。
func checkLLMKeys(cfg config.LLMConfig) bool {
	reg, err := llm.Load(cfg)
	if err != nil {
		fmt.Printf("✗ LLM 注册表加载失败: %v\n", err)
		return true
	}
	failed := false
	for _, name := range reg.Names() {
		p, err := reg.Get(name)
		if err != nil {
			continue // Names 返回的名字必然存在，防御性跳过
		}
		if !kernel.KnownComponent(p.Component) {
			fmt.Printf("✗ llm.providers.%s.component: unknown component %q（当前支持 openai/mock）\n",
				name, p.Component)
			failed = true
			continue
		}
		if !kernel.RequiresAPIKey(p.Component) {
			fmt.Printf("✓ 模型端点 %s（组件 %s）无需密钥\n", name, p.Component)
			continue
		}
		if reg.APIKey(name) == "" {
			fmt.Printf("! 模型端点 %s 密钥 %s 未设置，该模型暂不可用（可后续配置）\n",
				name, llm.APIKeyEnv(name))
		} else {
			fmt.Printf("✓ 模型端点 %s 密钥 %s 已设置\n", name, llm.APIKeyEnv(name))
		}
	}
	return failed
}
