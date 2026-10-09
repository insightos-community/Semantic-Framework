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
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 兼容现有 Runtime 的 tar.zst 交付格式。zstd 只负责解压字节流；路径和文件
// 类型由 Go 读取 tar 头校验，目录投递不会调用 tar 将未知内容写入宿主目录。
func readRuntimeTar(parent context.Context, archive string, visit func(*tar.Header, io.Reader) (bool, error)) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd := exec.CommandContext(ctx, "zstd", "-dc", "--", archive)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	reader := tar.NewReader(pipe)
	count := 0
	var total int64
	var parseErr error
	stoppedEarly := false
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			parseErr = err
			break
		}
		header.Name = strings.TrimPrefix(header.Name, "./")
		if header.Name == "" && header.Typeflag == tar.TypeDir {
			continue
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !safeRelative(name) || seen[name] || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			parseErr = fmt.Errorf("Runtime 包路径或类型无效: %s", header.Name)
			break
		}
		seen[name] = true
		count++
		if count > 200000 || header.Size < 0 || header.Size > 64<<30-total {
			parseErr = fmt.Errorf("Runtime 包展开过大")
			break
		}
		total += header.Size
		stop, err := visit(header, reader)
		if err != nil {
			parseErr = err
			break
		}
		if stop {
			stoppedEarly = true
			break
		}
	}
	if parseErr != nil || stoppedEarly {
		cancel()
	}
	_ = pipe.Close()
	waitErr := cmd.Wait()
	if parseErr != nil {
		return parseErr
	}
	if stoppedEarly {
		return nil
	}
	return waitErr
}

func inspectRuntimeTar(archive string) (Package, error) {
	var pkg Package
	found := false
	err := readRuntimeTar(context.Background(), archive, func(header *tar.Header, reader io.Reader) (bool, error) {
		if header.Name != "runtime-pack.yaml" {
			return false, nil
		}
		if header.Size > 1<<20 {
			return false, fmt.Errorf("Runtime 清单超过 1 MiB")
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return false, err
		}
		pkg, err = InspectRuntimeManifest(body)
		found = true
		return true, err
	})
	if err == nil && !found {
		err = fmt.Errorf("包内缺少 runtime-pack.yaml")
	}
	return pkg, err
}

func ExtractRuntimeArchive(ctx context.Context, archive, root string) error {
	if file, err := os.Open(archive); err == nil {
		var magic [4]byte
		_, _ = file.Read(magic[:])
		_ = file.Close()
		if string(magic[:2]) == "PK" {
			return ExtractZipContext(ctx, archive, root)
		}
	}
	return readRuntimeTar(ctx, archive, func(header *tar.Header, reader io.Reader) (bool, error) {
		path := filepath.Join(root, filepath.FromSlash(header.Name))
		if header.Typeflag == tar.TypeDir {
			return false, os.MkdirAll(path, 0750)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return false, err
		}
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return false, fmt.Errorf("目标不是普通文件: %s", path)
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640|os.FileMode(header.Mode)&0111)
		if err != nil {
			return false, err
		}
		_, copyErr := io.Copy(out, reader)
		closeErr := out.Close()
		if copyErr != nil {
			return false, copyErr
		}
		return false, closeErr
	})
}
