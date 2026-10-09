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
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type ScenePreviewStatus struct {
	State   string `json:"state"`
	Message string `json:"message"`
}
type scenePreviewResult struct {
	Description string `json:"description"`
	Variants    map[string]struct {
		Preview     string `json:"preview"`
		Description string `json:"description"`
	} `json:"variants"`
}

// 缓存身份只依赖场景内容和渲染器，不依赖 Robot、Skill 或模型绑定。
// 场景组件是按摘要安装的不可变输入；打包版本相同但内容更新也会失效。
func (s *Service) previewLocation(e SceneCatalogEntry, v SceneCatalogVersion) (string, string) {
	component, _ := os.ReadFile(filepath.Join(filepath.Dir(e.ContentRoot), ".component-id"))
	var runtime any
	if s.installations != nil {
		if i, err := s.installations.ByProfile(e.CompatibleRuntimeProfile); err == nil {
			manifest, _ := os.ReadFile(filepath.Join(i.PackPath, "runtime-pack.yaml"))
			runtime = []any{i.PackID, i.PackVersion, string(manifest)}
		}
	}
	data, _ := json.Marshal([]any{e.SceneID, e.ContentRoot, v, string(component), runtime, "preview-v2"})
	digest := sha256.Sum256(data)
	key := hex.EncodeToString(digest[:])
	root, _ := filepath.Abs(filepath.Join(s.sceneCatalog.directory, ".previews", key))
	return key, root
}

func writePreviewJSON(path string, value any) {
	data, _ := json.Marshal(value)
	if os.WriteFile(path+".tmp", data, 0600) == nil {
		_ = os.Rename(path+".tmp", path)
	}
}

// 安装和补全共用相同实现。预览失败保留场景安装结果，单独记录状态供重试。
func (s *Service) PrepareScenePreviews(ctx context.Context, ids []string, progress func(string)) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		e, err := s.sceneCatalog.Get(id)
		if err != nil {
			progress(err.Error())
			continue
		}
		for _, v := range e.Versions {
			_, root := s.previewLocation(e, v)
			if err := os.MkdirAll(root, 0700); err != nil {
				progress(err.Error())
				continue
			}
			status := func(state, message string) {
				writePreviewJSON(filepath.Join(root, "status.json"), ScenePreviewStatus{state, message})
				progress(e.Name + "：" + message)
			}
			if ctx.Err() != nil {
				status("cancelled", "预览已取消，可继续补全")
				return
			}
			variants := []string{}
			for _, variant := range v.Variants {
				if variant.Preview == "" {
					variants = append(variants, variant.VariantID)
				}
			}
			if len(variants) == 0 {
				status("ready", "已复用包内预览")
				continue
			}
			if s.installations == nil {
				status("unavailable", "场景已安装，需先安装兼容 Runtime 才能生成预览")
				continue
			}
			installation, err := s.installations.ByProfile(e.CompatibleRuntimeProfile)
			if err != nil || !installation.Enabled || !installation.Profile.Capabilities.ScenePreviews {
				status("unavailable", "场景已安装，请安装支持场景预览的兼容 Runtime，然后补全预览")
				continue
			}
			launcher, ok := installation.Binding().Launcher.(ExecLauncher)
			if !ok {
				status("unavailable", "当前 Runtime 安装方式不支持本机离屏预览")
				continue
			}
			request := filepath.Join(root, "request.json")
			writePreviewJSON(request, map[string]any{"preview_version": 2, "scene_key": v.RuntimeSceneKey, "content_root": e.ContentRoot,
				"variants": variants, "output_dir": root})
			// 命令来自受信任的 Runtime 安装配置；场景包只提供数据，不能指定程序。
			args := append(append([]string{}, launcher.Args...), "--prepare-scene", request)
			cmd := exec.CommandContext(ctx, launcher.Command, args...)
			// Runtime 可能持有独立引擎进程或容器。先发送终止信号让它回收资源，
			// 再以有限等待兜底，避免取消预览只杀掉外层命令却遗留 GPU 占用。
			cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
			cmd.WaitDelay = 45 * time.Second
			cmd.Dir = launcher.Dir
			cmd.Env = append(os.Environ(), launcher.Env...)
			cmd.Env = append(cmd.Env, "MUJOCO_GL=egl", "PYOPENGL_PLATFORM=egl")
			log, err := os.Create(filepath.Join(root, "render.log"))
			if err != nil {
				status("failed", err.Error())
				continue
			}
			cmd.Stderr = log
			pipe, err := cmd.StdoutPipe()
			failureMessage := ""
			if err == nil {
				err = cmd.Start()
			}
			if err == nil {
				status("running", "正在准备原生任务和初态预览")
				scanner := bufio.NewScanner(pipe)
				for scanner.Scan() {
					line := scanner.Text()
					fmt.Fprintln(log, line)
					if strings.HasPrefix(line, "预览准备失败：") {
						failureMessage = strings.TrimPrefix(line, "预览准备失败：")
					}
					if strings.HasPrefix(line, "已生成 ") {
						status("running", line)
					}
				}
				err = cmd.Wait()
			}
			log.Close()
			if ctx.Err() != nil {
				status("cancelled", "预览已取消，已完成图片保留")
				return
			}
			if err != nil {
				if failureMessage == "" {
					failureMessage = err.Error() + "；可补全重试"
				}
				status("failed", "场景已安装，预览未完成："+failureMessage)
				continue
			}
			status("ready", fmt.Sprintf("已完成 %d 个初态预览", len(variants)))
		}
	}
}

func (s *Service) StartScenePreviews(id string) error {
	if s.sceneCatalog == nil {
		return ErrNotFound
	}
	if _, err := s.sceneCatalog.Get(id); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, loaded := s.previewJobs.LoadOrStore(id, cancel); loaded {
		cancel()
		return nil
	}
	entry, _ := s.sceneCatalog.Get(id)
	for _, version := range entry.Versions {
		_, root := s.previewLocation(entry, version)
		_ = os.MkdirAll(root, 0700)
		writePreviewJSON(filepath.Join(root, "status.json"), ScenePreviewStatus{"queued", "预览已排队，当前现场保持运行"})
	}
	go func() {
		defer s.previewJobs.Delete(id)
		defer cancel()
		s.PrepareScenePreviews(ctx, []string{id}, func(string) {})
		// 排队时取消尚未进入渲染循环，同样需要结束界面的排队状态。
		if ctx.Err() != nil {
			for _, version := range entry.Versions {
				_, root := s.previewLocation(entry, version)
				writePreviewJSON(filepath.Join(root, "status.json"), ScenePreviewStatus{"cancelled", "预览已取消，已完成图片保留"})
			}
		}
	}()
	return nil
}
func (s *Service) CancelScenePreviews(id string) {
	if cancel, ok := s.previewJobs.Load(id); ok {
		cancel.(context.CancelFunc)()
	}
}

func (s *Service) decorateScenePreviews(entries []SceneCatalogEntry) []SceneCatalogEntry {
	for index := range entries {
		e := &entries[index]
		e.Versions = append([]SceneCatalogVersion{}, e.Versions...)
		for vi := range e.Versions {
			v := &e.Versions[vi]
			key, root := s.previewLocation(*e, *v)
			v.Variants = append([]SceneCatalogVariant{}, v.Variants...)
			var result scenePreviewResult
			if data, err := os.ReadFile(filepath.Join(root, "result.json")); err == nil {
				_ = json.Unmarshal(data, &result)
			}
			if result.Description != "" {
				e.Description = result.Description
			}
			for i := range v.Variants {
				variant := &v.Variants[i]
				if variant.Preview != "" && !strings.HasPrefix(variant.Preview, "/") && !strings.Contains(variant.Preview, ":") {
					variant.Preview = "/simulation/scene-preview-assets/" + url.PathEscape(e.SceneID) + "?version=" + url.QueryEscape(v.Version) + "&variant=" + url.QueryEscape(variant.VariantID)
				}
				if item, ok := result.Variants[variant.VariantID]; ok {
					if variant.Preview == "" {
						variant.Preview = "/simulation/scene-previews/" + key + "/" + item.Preview
					}
					variant.Description = item.Description
					if e.Preview == "" {
						e.Preview = variant.Preview
					}
				}
			}
			if e.Preview != "" && !strings.HasPrefix(e.Preview, "/") && !strings.Contains(e.Preview, ":") {
				e.Preview = "/simulation/scene-preview-assets/" + url.PathEscape(e.SceneID)
			}
			if data, err := os.ReadFile(filepath.Join(root, "status.json")); err == nil {
				var status ScenePreviewStatus
				if json.Unmarshal(data, &status) == nil {
					e.PreviewPreparation = &status
				}
			}
		}
	}
	return entries
}

// 包内预览只允许读取目录清单已声明的图片，不接受客户端传入宿主路径。
func (s *Service) BundledScenePreviewFile(id, version, variant string) (string, error) {
	if s.sceneCatalog == nil {
		return "", ErrNotFound
	}
	entry, err := s.sceneCatalog.Get(id)
	if err != nil {
		return "", err
	}
	asset := entry.Preview
	if variant != "" {
		asset = ""
		for _, v := range entry.Versions {
			if v.Version == version {
				for _, item := range v.Variants {
					if item.VariantID == variant {
						asset = item.Preview
					}
				}
			}
		}
	}
	if asset == "" || filepath.IsAbs(asset) || strings.Contains(asset, ":") || entry.ContentRoot == "" {
		return "", ErrNotFound
	}
	root, err := filepath.EvalSymlinks(entry.ContentRoot)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, asset))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrNotFound
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".jpg" && ext != ".png" && ext != ".webp" {
		return "", ErrNotFound
	}
	return path, nil
}

func (s *Service) ScenePreviewFile(key, name string) (string, error) {
	if s.sceneCatalog == nil || len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" ||
		filepath.Base(name) != name || !strings.HasSuffix(name, ".jpg") {
		return "", ErrNotFound
	}
	return filepath.Join(s.sceneCatalog.directory, ".previews", key, name), nil
}
