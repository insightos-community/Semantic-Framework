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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"insightos.cn/semantic-framework/internal/install"
)

func runInstall(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: semantic install <包.zip|Skill源码目录> --project <项目ID> [--robot <Robot ID>] [--server <地址>]\nRuntime Pack: semantic install runtime <安装参数>")
		return 2
	}
	// Runtime 复用现有安装器；项目组件复用 Server 导入服务，保持相同校验与记录。
	if args[0] == "runtime" {
		return runRuntime(append([]string{"install"}, args[1:]...))
	}
	source := args[0]
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	server := serverFlag(fs)
	projectID := fs.String("project", "", "目标 Project ID")
	robotID := fs.String("robot", "", "导入 Robot Skill 后将安装请求发送到此 Robot")
	importOnly := fs.Bool("import-only", false, "仅导入包，稍后在 Web 确认安装")
	assetRoot := fs.String("asset-root", "", "Runtime 资产目录")
	modelRoot := fs.String("model-root", "", "模型缓存目录")
	liberoRoot := fs.String("libero-root", "", "已安装 LIBERO 源码目录")
	liberoProRoot := fs.String("libero-pro-root", "", "可选 LIBERO-Pro 源码目录")
	endpoint := fs.String("endpoint", "", "Runtime 服务监听地址")
	applyNow := fs.Bool("apply", false, "空闲时重启目标 Robot，使新组件生效")
	previews := fs.Bool("generate-previews", true, "安装场景时自动补全任务信息和初态预览")
	var sceneIDs stringListFlag
	fs.Var(&sceneIDs, "scene", "仅生成指定场景的预览，可重复；默认包内全部场景")
	projectDefault := fs.Bool("project-default", false, "将 Ability 或模型设为项目对应 Robot 型号的默认版本")
	wheelDir := fs.String("wheel-dir", "", "源码构建复用的离线 Wheel 目录")
	offline := fs.Bool("offline", false, "源码构建仅从 Wheel 缓存收集依赖")
	var licenses stringListFlag
	var selectedComponents stringListFlag
	fs.Var(&selectedComponents, "component", "只安装包内指定组件及其依赖，可重复；省略时安装全部")
	fs.Var(&licenses, "accept-license", "已接受的内容许可，可重复")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *projectID == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "请指定 --project <项目ID>")
		return 2
	}
	if *wheelDir != "" {
		_ = os.Setenv("SEMANTIC_BUILD_WHEELHOUSE", *wheelDir)
	}
	if *offline {
		_ = os.Setenv("SEMANTIC_BUILD_OFFLINE", "1")
	}
	if err := installSourceOptions(source, *projectID, *robotID, *server, *importOnly, install.Options{GeneratePreviews: previews, SceneIDs: sceneIDs, Components: selectedComponents, ConfirmCode: true, ProjectDefault: *projectDefault, ApplyNow: *applyNow, RobotID: *robotID, AssetRoot: *assetRoot, ModelRoot: *modelRoot, LiberoRoot: *liberoRoot, LiberoProRoot: *liberoProRoot, Endpoint: *endpoint, AcceptedLicenses: licenses}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func installSourceOptions(source, projectID, robotID, server string, importOnly bool, options install.Options) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	filename := filepath.Base(filepath.Clean(source))
	if info.IsDir() {
		temp, err := os.MkdirTemp("", "semantic-source-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temp)
		archive := filepath.Join(temp, "component.zip")
		// 源码目录要先打成临时组件包；打包失败必须终止，不能带着不存在的
		// archive 继续走导入流程。
		if err := buildSourcePackage(context.Background(), source, archive, true); err != nil {
			return err
		}
		filename += ".zip"
		source = archive
	}
	pkg, err := install.InspectArchive(source)
	if err != nil {
		return err
	}
	if pkg.Kind == "package" {
		entries, err := install.SelectPackageEntries(pkg.Components, options.Components)
		if err != nil {
			return err
		}
		fmt.Println("将安装以下组件（包含所需依赖）：")
		for _, entry := range entries {
			fmt.Println(" -", entry.ID)
		}
	} else if len(options.Components) != 0 {
		return fmt.Errorf("--component 仅用于包含多个组件的安装包")
	}
	creds, err := loadValidCredentials()
	if err != nil {
		return err
	}
	client, err := newClient(resolveServer(server, creds), creds.Token)
	if err != nil {
		return err
	}
	client.hc.Timeout = 30 * time.Minute
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	item, err := client.importPackageReader(projectID, filename, file)
	if err != nil {
		return err
	}
	fmt.Printf("已导入 %s %s %s（记录 %s）\n", item.Kind, item.Name, item.Version, item.ID)
	if pkg.Kind != "robot_skill" && pkg.Kind != "scene" && !importOnly {
		path := "/api/v1/projects/" + url.PathEscape(projectID) + "/imports/" + url.PathEscape(item.ID) + "/install"
		if err := client.doJSON(http.MethodPost, path, options, nil); err != nil {
			return err
		}
		fmt.Println("安装已开始，可在 Web 查看进度；等待安装结果…")
		lastProgress := ""
		for {
			var response struct {
				Items []install.Record `json:"items"`
			}
			if err := client.doJSON(http.MethodGet, "/api/v1/projects/"+url.PathEscape(projectID)+"/imports", nil, &response); err != nil {
				return err
			}
			for _, record := range response.Items {
				if record.ID != item.ID {
					continue
				}
				if record.Progress != lastProgress {
					fmt.Println(record.Progress)
					lastProgress = record.Progress
				}
				switch record.InstallationStatus {
				case "installed":
					fmt.Println("安装完成；运行中的 Robot 保持原版本。")
					return nil
				case "failed":
					return fmt.Errorf("安装失败: %s", record.Error)
				}
			}
			time.Sleep(time.Second)
		}
	}
	if robotID != "" && pkg.Kind == "robot_skill" && !importOnly {
		path := "/api/v1/devices/" + url.PathEscape(robotID) + "/skills/" + url.PathEscape(item.Name) + "/" + url.PathEscape(item.Version) + "/install"
		if err := client.doJSON(http.MethodPost, path, nil, nil); err != nil {
			return err
		}
		fmt.Println("已提交 Robot 安装请求；请在设备页查看实际安装及就绪状态。")
	} else if item.Kind == "robot_skill" {
		fmt.Println("可在设备页选择此版本安装，或增加 --robot <Robot ID> 提交安装请求。")
	}
	return nil
}

func (c *Client) importPackage(projectID, filename string, data []byte) (install.Record, error) {
	return c.importPackageReader(projectID, filename, bytes.NewReader(data))
}
func (c *Client) importPackageReader(projectID, filename string, data io.Reader) (install.Record, error) {
	endpoint := c.httpBase + "/api/v1/projects/" + url.PathEscape(projectID) + "/imports?filename=" + url.QueryEscape(filename)
	req, err := http.NewRequest(http.MethodPost, endpoint, data)
	if err != nil {
		return install.Record{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/zip")
	response, err := c.hc.Do(req)
	if err != nil {
		return install.Record{}, err
	}
	defer response.Body.Close()
	var body struct {
		Item  install.Record `json:"item"`
		Error apiError       `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return install.Record{}, err
	}
	if response.StatusCode >= 300 {
		return body.Item, fmt.Errorf("导入失败（HTTP %d）: %s", response.StatusCode, body.Error.Message)
	}
	if body.Item.Status != "imported" {
		return body.Item, fmt.Errorf("导入记录 %s: %s %s", body.Item.ID, body.Item.Status, body.Item.Error)
	}
	return body.Item, nil
}
