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

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// localDotEnvPath 是项目级 .env 文件（相对启动工作目录），优先级高于用户级。
const localDotEnvPath = ".env"

// DotEnvFile 记录单个 .env 文件的加载结果（只记录键名，绝不记录值）。
type DotEnvFile struct {
	// Path 实际读取的文件路径。
	Path string

	// Keys 本文件实际写入进程 env 的键名（已被进程 env 占用的键不在其中）。
	Keys []string
}

// DotEnvResult 汇总本次 .env 加载结果，供启动日志输出（不含任何密钥值）。
type DotEnvResult struct {
	// Files 实际读取到的 .env 文件，按加载顺序（高优先级在前）。
	Files []DotEnvFile
}

// SetKeys 返回全部经 .env 写入进程 env 的键名，供热重载识别"自有键"。
func (r DotEnvResult) SetKeys() []string {
	var keys []string
	for _, f := range r.Files {
		keys = append(keys, f.Keys...)
	}
	return keys
}

// LocalKeys 返回项目级 ./.env 实际写入的键名。
// 热重载只跟踪 ./.env（用户级 ~/.semantic/.env 不监听），因此只需这组键。
func (r DotEnvResult) LocalKeys() []string {
	for _, f := range r.Files {
		if f.Path == localDotEnvPath {
			return f.Keys
		}
	}
	return nil
}

// LoadDotEnv 在启动早期加载 .env 文件：先 ./.env（项目级），
// 再 ~/.semantic/.env（用户级），文件不存在则跳过。
// 优先级：进程 env > ./.env > ~/.semantic/.env——已存在于进程 env 的键
// 一律不覆盖，因此先加载的文件自然压住后加载的同名键。
// 文件存在但内容非法（缺少 = 的行）时返回错误，由调用方决定是否 fail-closed。
func LoadDotEnv() (DotEnvResult, error) {
	paths := []string{localDotEnvPath}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".semantic", ".env"))
	}
	return loadDotEnvFiles(paths)
}

// loadDotEnvFiles 按给定顺序加载 .env 文件，是 LoadDotEnv 的可测试内核。
func loadDotEnvFiles(paths []string) (DotEnvResult, error) {
	var res DotEnvResult
	for _, path := range paths {
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // 文件不存在属常态，静默跳过
		case err != nil:
			return res, fmt.Errorf("读取 .env 文件 %s 失败: %w", path, err)
		}
		keys, err := applyDotEnv(string(data), path)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, DotEnvFile{Path: path, Keys: keys})
	}
	return res, nil
}

// dotEnvEntry 是 .env 文件解析出的一行键值对。
type dotEnvEntry struct {
	// Key 环境变量名。
	Key string

	// Value 环境变量值（已去引号、已截行尾注释）。
	Value string
}

// applyDotEnv 解析单个 .env 文件内容并写入进程 env（不覆盖已有键），
// 返回实际写入的键名。
func applyDotEnv(content, path string) ([]string, error) {
	entries, err := parseDotEnv(content, path)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if _, exists := os.LookupEnv(e.Key); exists {
			continue // 不覆盖进程 env（含高优先级 .env 已写入的键）
		}
		if err := os.Setenv(e.Key, e.Value); err != nil {
			return nil, fmt.Errorf("写入环境变量 %s（来自 %s）失败: %w", e.Key, path, err)
		}
		keys = append(keys, e.Key)
	}
	return keys, nil
}

// parseDotEnv 把 .env 文件内容解析为键值对序列（不写进程 env）。
// 支持 KEY=VALUE、export 前缀、单双引号、整行注释（# 开头）与
// 非引用值的行尾注释（" #" 起）。
func parseDotEnv(content, path string) ([]dotEnvEntry, error) {
	var entries []dotEnvEntry
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("解析 .env 文件 %s 第 %d 行失败：非法行 %q（缺少 KEY=）", path, i+1, raw)
		}
		entries = append(entries, dotEnvEntry{
			Key:   strings.TrimSpace(key),
			Value: unquoteDotEnvValue(strings.TrimSpace(value)),
		})
	}
	return entries, nil
}

// unquoteDotEnvValue 去除值两侧的单双引号；非引用值按 " #" 截断行尾注释。
// 双引号内不展开转义序列——密钥类值应原样保留，避免静默改写。
func unquoteDotEnvValue(value string) string {
	if len(value) >= 2 {
		if q := value[0]; (q == '"' || q == '\'') && value[len(value)-1] == q {
			return value[1 : len(value)-1]
		}
	}
	if idx := strings.Index(value, " #"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return value
}
