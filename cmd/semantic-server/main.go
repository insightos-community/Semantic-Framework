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

// semantic-server 入口：只解析参数与加载配置，装配逻辑全部在 internal/bootstrap。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"insightos.cn/semantic-framework/internal/bootstrap"
	"insightos.cn/semantic-framework/pkg/config"
	"insightos.cn/semantic-framework/pkg/log"
)

func main() {
	// -c 留空时依次尝试 SEMANTIC_CONFIG 和 semantic init 的默认安装位置；
	// 解析后的绝对路径会继续传给热重载和设置 API，保证全链路只有一个目标。
	configFlag := flag.String("c", "", "配置文件路径（默认读取 semantic init 的安装配置）")
	flag.Parse()
	configPath, err := config.ResolvePath(*configFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析配置路径失败: %v\n", err)
		os.Exit(1)
	}

	// .env 必须先于配置加载：其中的 SEMANTIC_* 键经 env 覆盖链生效。
	// 解析失败 fail-closed——静默跳过会让"以为已生效"的密钥/配置实际缺席。
	dotenv, err := config.LoadDotEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载 .env 失败: %v\n", err)
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		// 日志器尚未初始化，启动前的致命错误直接写 stderr。
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n请先运行 semantic init，或用 -c 指定已有配置。\n", err)
		os.Exit(1)
	}

	// 运行日志同时写到当前终端和当前安装实例的 logs 目录。
	// 日志路径由实际加载配置中的 SQLite 路径推导，不能写死到仓库或默认安装目录。
	runtimeLog, err := log.OpenRuntimeFile(cfg.Store.SQLitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化运行日志失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = runtimeLog.Close() }()
	logger := log.New(log.Options{
		Level:  log.ParseLevel(cfg.Log.Level),
		Writer: io.MultiWriter(os.Stdout, runtimeLog),
	})
	logger.Info("配置加载完成",
		"path", configPath,
		"log_path", runtimeLog.Path(),
		"http_addr", cfg.Server.HTTPAddr,
		"ws_addr", cfg.Server.WSAddr,
		"log_level", cfg.Log.Level,
		"store_driver", cfg.Store.Driver,
	)
	// .env 加载结果只记文件与键数，绝不记值。
	for _, f := range dotenv.Files {
		logger.Info(".env 已加载", "path", f.Path, "keys", len(f.Keys))
	}

	app, err := bootstrap.Wire(cfg, logger)
	if err != nil {
		logger.WithError(err).Fatal("应用装配失败")
	}
	// 热重载初始化失败不阻塞启动：降级为"配置变更需重启"。
	if err := app.EnableConfigReload(configPath, dotenv.LocalKeys()); err != nil {
		logger.WithError(err).Warn("配置热重载不可用，配置变更需重启生效")
	}
	if err := app.EnableMDNSDiscovery(); err != nil {
		logger.WithError(err).Warn("Semantic Server mDNS 广播启动失败，Pilot 仍可通过显式地址加入")
	}
	if err := app.Run(context.Background()); err != nil {
		logger.WithError(err).Fatal("服务异常退出")
	}
}
