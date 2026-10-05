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

package kernel

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	localbackend "github.com/cloudwego/eino-ext/adk/backend/local"
	"github.com/cloudwego/eino/adk"
	adkfs "github.com/cloudwego/eino/adk/filesystem"
	einofs "github.com/cloudwego/eino/adk/middlewares/filesystem"

	"insightos.cn/semantic-framework/pkg/log"
)

const projectVirtualRoot = "/workspace"

// projectFilesystemBackend 把 Eino-ext Local Backend 限定到一个 Project
// workspace，并向模型暴露稳定的虚拟根目录 /workspace。它只做领域路径边界
// 和结果路径转换，文件读取、Glob 与 Grep 仍由开源 Backend 实现。
type projectFilesystemBackend struct {
	// root 是解析符号链接后的 Project workspace 真实路径。
	root string

	// local 是 Eino-ext 提供的成熟本地文件 Backend。
	local *localbackend.Local
}

// newProjectFilesystemBackend 创建只属于一个 Project 的文件后端。
func newProjectFilesystemBackend(ctx context.Context, workspaceRoot string) (*projectFilesystemBackend, error) {
	root, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil {
		return nil, fmt.Errorf("解析 Project workspace 失败: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("解析 Project workspace 符号链接失败: %w", err)
	}
	local, err := localbackend.NewBackend(ctx, &localbackend.Config{})
	if err != nil {
		return nil, fmt.Errorf("创建 Eino Local 文件 Backend 失败: %w", err)
	}
	return &projectFilesystemBackend{root: filepath.Clean(root), local: local}, nil
}

// resolve 把 /workspace 下的虚拟绝对路径解析为真实路径，并检查已有路径
// 组成部分的符号链接没有逃出 Project。不存在的末端仍交给 Local Backend
// 返回标准“文件不存在”错误。
func (b *projectFilesystemBackend) resolve(virtualPath string) (string, error) {
	virtualPath = strings.TrimSpace(virtualPath)
	// Eino 的 grep/glob path 是可选字段；省略时应落到当前 Project 根，
	// 不能沿用 Local Backend 的宿主进程当前目录或“/”默认值。
	if virtualPath == "" {
		virtualPath = projectVirtualRoot
	}
	cleaned := path.Clean(virtualPath)
	if cleaned != projectVirtualRoot && !strings.HasPrefix(cleaned, projectVirtualRoot+"/") {
		return "", fmt.Errorf("路径必须位于 %s 内", projectVirtualRoot)
	}
	relative := strings.TrimPrefix(cleaned, projectVirtualRoot)
	relative = strings.TrimPrefix(relative, "/")
	target := filepath.Join(b.root, filepath.FromSlash(relative))
	if !pathWithinRoot(b.root, target) {
		return "", fmt.Errorf("路径越出 Project workspace")
	}
	resolved, err := resolveExistingPrefix(target)
	if err != nil {
		return "", err
	}
	if !pathWithinRoot(b.root, resolved) {
		return "", fmt.Errorf("路径通过符号链接越出 Project workspace")
	}
	return target, nil
}

// resolveExistingPrefix 解析路径中最深的已有部分，用于同时覆盖“现有符号链接
// + 不存在末端”的逃逸路径。
func resolveExistingPrefix(target string) (string, error) {
	current := filepath.Clean(target)
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("检查文件路径失败: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("文件路径没有可解析的已有父目录")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", fmt.Errorf("解析文件路径符号链接失败: %w", err)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	return resolved, nil
}

// pathWithinRoot 使用 filepath.Rel 做目录边界判断，避免字符串前缀把
// /workspace-a 错当成 /workspace 的子目录。
func pathWithinRoot(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// virtualizePath 把 Local Backend 返回的真实路径重新转换为模型可见路径。
func (b *projectFilesystemBackend) virtualizePath(realPath string) string {
	if !filepath.IsAbs(realPath) {
		return filepath.ToSlash(realPath)
	}
	relative, err := filepath.Rel(b.root, realPath)
	if err != nil || !pathWithinRoot(b.root, realPath) {
		return projectVirtualRoot
	}
	if relative == "." {
		return projectVirtualRoot
	}
	return path.Join(projectVirtualRoot, filepath.ToSlash(relative))
}

// LsInfo 复用 Local Backend 列目录。
func (b *projectFilesystemBackend) LsInfo(ctx context.Context, req *adkfs.LsInfoRequest) ([]adkfs.FileInfo, error) {
	realPath, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	cloned := *req
	cloned.Path = realPath
	return b.local.LsInfo(ctx, &cloned)
}

// Read 复用 Local Backend 的分页文本读取。
func (b *projectFilesystemBackend) Read(ctx context.Context, req *adkfs.ReadRequest) (*adkfs.FileContent, error) {
	realPath, err := b.resolve(req.FilePath)
	if err != nil {
		return nil, err
	}
	cloned := *req
	cloned.FilePath = realPath
	return b.local.Read(ctx, &cloned)
}

// GrepRaw 复用 Local Backend 的 ripgrep 搜索，并把命中路径恢复为虚拟路径。
func (b *projectFilesystemBackend) GrepRaw(ctx context.Context, req *adkfs.GrepRequest) ([]adkfs.GrepMatch, error) {
	realPath, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	cloned := *req
	cloned.Path = realPath
	matches, err := b.local.GrepRaw(ctx, &cloned)
	if err != nil {
		return nil, err
	}
	for i := range matches {
		matches[i].Path = b.virtualizePath(matches[i].Path)
	}
	return matches, nil
}

// GlobInfo 复用 Local Backend 的文件模式匹配。Local 返回相对请求目录的
// 路径，因此结果无需暴露真实 workspace 根路径。
func (b *projectFilesystemBackend) GlobInfo(ctx context.Context, req *adkfs.GlobInfoRequest) ([]adkfs.FileInfo, error) {
	realPath, err := b.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	cloned := *req
	cloned.Path = realPath
	return b.local.GlobInfo(ctx, &cloned)
}

// Write 是 Backend 接口要求的方法；本版本不向 Agent 注册 write_file，防止
// 文件写入绕过 ask/auto/full 的显式 execute 工具策略。
func (b *projectFilesystemBackend) Write(context.Context, *adkfs.WriteRequest) error {
	return fmt.Errorf("Project 文件系统是只读的；需要写入时请使用 execute 或 execute_host")
}

// Edit 与 Write 使用同一只读边界。
func (b *projectFilesystemBackend) Edit(context.Context, *adkfs.EditRequest) error {
	return fmt.Errorf("Project 文件系统是只读的；需要修改时请使用 execute 或 execute_host")
}

// buildFilesystemMiddleware 构建 Eino 官方文件系统中间件。这里只启用四个
// 只读工具，不传 Shell，避免与 Semantic 的 execute/execute_host 重名或绕过审批。
func buildFilesystemMiddleware(ctx context.Context, workspaceRoot string, logger *log.Logger) (adk.ChatModelAgentMiddleware, error) {
	backend, err := newProjectFilesystemBackend(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	writeDisabled := &einofs.ToolConfig{Disable: true}
	editDisabled := &einofs.ToolConfig{Disable: true}
	prompt := "# Project 文件系统\n\n只可使用 /workspace 虚拟根目录读取当前 Project 文件；不要猜测或使用宿主真实路径。"
	readDesc := filesystemReadDescription
	lsDesc := filesystemLsDescription
	globDesc := filesystemGlobDescription
	grepDesc := filesystemGrepDescription
	middleware, err := einofs.New(ctx, &einofs.MiddlewareConfig{
		Backend:             backend,
		CustomSystemPrompt:  &prompt,
		LsToolConfig:        &einofs.ToolConfig{Desc: &lsDesc},
		ReadFileToolConfig:  &einofs.ToolConfig{Desc: &readDesc},
		GlobToolConfig:      &einofs.ToolConfig{Desc: &globDesc},
		GrepToolConfig:      &einofs.ToolConfig{Desc: &grepDesc},
		WriteFileToolConfig: writeDisabled,
		EditFileToolConfig:  editDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("构建 Eino Filesystem Middleware 失败: %w", err)
	}
	logger.Debug("filesystem middleware 已启用", "virtual_root", projectVirtualRoot)
	return middleware, nil
}
