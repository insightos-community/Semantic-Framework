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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
)

const MaxUploadBytes int64 = 32 << 30

func ArchiveDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// 大型权重和 Wheel 以流方式落盘；摘要同时计算，上传不会占用整个包大小的内存。
func CopyArchive(source io.Reader, destination string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return "", err
	}
	file, err := os.Create(destination)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, h), io.LimitReader(source, MaxUploadBytes+1))
	if err != nil {
		return "", err
	}
	if n > MaxUploadBytes {
		return "", fmt.Errorf("包超过 32 GiB")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func InspectArchive(path string) (Package, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return inspectRuntimeTar(path)
	}
	defer z.Close()
	var manifest *zip.File
	for _, f := range z.File {
		if f.Name != "semantic-component.yaml" && f.Name != "runtime-pack.yaml" && f.Name != "semantic-source.yaml" && f.Name != "semantic-package.yaml" {
			continue
		}
		if manifest != nil {
			return Package{}, fmt.Errorf("安装包需要唯一的根清单")
		}
		manifest = f
	}
	if manifest != nil {
		f := manifest
		if f.UncompressedSize64 > 1<<20 {
			return Package{}, fmt.Errorf("组件清单过大")
		}
		r, err := f.Open()
		if err != nil {
			return Package{}, err
		}
		data, err := io.ReadAll(io.LimitReader(r, 1<<20))
		_ = r.Close()
		if err != nil {
			return Package{}, err
		}
		if f.Name == "semantic-component.yaml" {
			c, err := ParseComponent(data)
			pkg := Package{Kind: c.Kind, Name: c.Name, Version: c.Version, SourceRevision: c.SourceRevision}
			if err == nil && c.Kind == "scene_catalog" {
				for _, entry := range z.File {
					if entry.Name == c.SceneCatalog {
						reader, openErr := entry.Open()
						if openErr != nil {
							return pkg, openErr
						}
						body, readErr := io.ReadAll(io.LimitReader(reader, 8<<20))
						reader.Close()
						if readErr != nil {
							return pkg, readErr
						}
						var catalog struct {
							Entries []struct {
								ScenePackageSummary `yaml:",inline"`
								Versions            []struct {
									Variants []any `yaml:"variants"`
								} `yaml:"versions"`
							} `yaml:"entries"`
						}
						if err := yaml.Unmarshal(body, &catalog); err != nil {
							return pkg, err
						}
						for _, scene := range catalog.Entries {
							summary := scene.ScenePackageSummary
							for _, version := range scene.Versions {
								summary.InitialStates += len(version.Variants)
							}
							pkg.Scenes = append(pkg.Scenes, summary)
						}
					}
				}
			}
			return pkg, err
		}
		if f.Name == "semantic-source.yaml" {
			return inspectSourceManifest(data)
		}
		if f.Name == "semantic-package.yaml" {
			return inspectInstallPackage(data, z.File)
		}
		return InspectRuntimeManifest(data)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Package{}, err
	}
	if info.Size() > MaxPackageBytes {
		return Package{}, fmt.Errorf("旧格式 Scene/Skill 包超过 100 MiB，请按组件组织大型资产")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Package{}, err
	}
	return Inspect(data)
}

// 解包只允许普通文件和目录；路径、链接和累计展开大小统一检查，所有组件共用。
func ExtractZip(archive, root string) error {
	return ExtractZipContext(context.Background(), archive, root)
}

func ExtractZipContext(ctx context.Context, archive, root string) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer z.Close()
	if len(z.File) > 200000 {
		return fmt.Errorf("包内文件数过多")
	}
	var total uint64
	seen := map[string]bool{}
	for _, f := range z.File {
		name := filepath.Clean(f.Name)
		if !safeRelative(name) || seen[name] || !f.Mode().IsRegular() && !f.FileInfo().IsDir() {
			return fmt.Errorf("包内路径或文件类型无效: %s", f.Name)
		}
		seen[name] = true
		if f.UncompressedSize64 > 64<<30-total {
			return fmt.Errorf("包展开超过 64 GiB")
		}
		total += f.UncompressedSize64
	}
	for _, f := range z.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(root, filepath.Clean(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		// 禁止跟随先前失败安装留下的链接；已存在的普通文件可用于明确重试。
		if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("安装目标不是普通文件: %s", target)
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0640|f.Mode().Perm()&0111)
		if err != nil {
			_ = r.Close()
			return err
		}
		_, copyErr := io.Copy(out, &archiveContextReader{ctx: ctx, reader: r})
		_ = r.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// 权重常为单个大文件，取消不能只在两个文件之间检查。
type archiveContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
