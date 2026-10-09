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

package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"insightos.cn/semantic-framework/internal/install"
	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
)

// componentInstaller 只编排已有安装器：Runtime 仍执行 Pack 校验和 smoke，
// Robot Skill 仍经发布服务对账，Robot 进程仍由原 supervisor 管理。
// 全局目录变更串行提交；包上传与模型推理不持有该锁。
func componentInstaller(app *App, cfg *config.Config, st *store.Store, robots *robotdomain.Service, sim *simulation.Service) install.Installer {
	components := app.components
	var mu sync.Mutex
	var apply func(context.Context, string, install.Record, string, install.Options, func(string)) ([]string, error)
	apply = func(ctx context.Context, projectID string, record install.Record, archive string, options install.Options, progress func(string)) ([]string, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch record.Kind {
		case "package":
			resources, err := install.BatchInstall(ctx, projectID, archive, options, apply, progress)
			if err != nil {
				return resources, err
			}
			if options.ApplyNow && options.RobotID != "" {
				if app.managedRobots == nil {
					return resources, fmt.Errorf("组件已安装，当前未启用受管 Robot")
				}
				if err := app.managedRobots.applyInstalledComponents(ctx, projectID, options.RobotID, sim); err != nil {
					return resources, fmt.Errorf("组件已安装，Robot 生效未完成: %w", err)
				}
			}
			return resources, nil
		case "robot_ability_source":
			root := filepath.Join(components.Root, "sources", record.ID)
			output := filepath.Join(components.Root, "source-builds", record.ID+".zip")
			if err := install.BuildUploadedSource(ctx, archive, root, output, progress); err != nil {
				return nil, err
			}
			id, err := install.ArchiveDigest(output)
			if err != nil {
				return nil, err
			}
			pkg, err := install.InspectArchive(output)
			if err != nil {
				return nil, err
			}
			return apply(ctx, projectID, install.Record{Package: pkg, ID: id}, output, options, progress)
		case "scene":
			body, err := os.ReadFile(archive)
			if err != nil {
				return nil, err
			}
			return app.imports.ApplyPackage(ctx, projectID, record.Package, body)
		case "runtime":
			root := filepath.Join(components.Root, "runtime-staging", record.ID)
			if err := install.ExtractRuntimeArchive(ctx, archive, root); err != nil {
				return nil, err
			}
			pack, err := simulation.LoadRuntimePack(root)
			if err != nil {
				return nil, err
			}
			if err := sim.CheckRuntimeInstallIdle(ctx, pack.Profile.RuntimeProfileID); err != nil {
				return nil, err
			}
			// Server 只调用自身发行目录的 semantic CLI，包内容无法指定可执行命令。
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			args := []string{"install", "runtime", "--pack-dir", root, "-c", app.settingsCtl.ConfigPath(), "--replace"}
			for _, pair := range [][2]string{{"--asset-root", options.AssetRoot}, {"--model-root", options.ModelRoot}, {"--libero-root", options.LiberoRoot}, {"--libero-pro-root", options.LiberoProRoot}, {"--endpoint", options.Endpoint}} {
				if pair[1] != "" {
					args = append(args, pair[0], pair[1])
				}
			}
			for _, license := range options.AcceptedLicenses {
				args = append(args, "--accept-license", license)
			}
			progress("安装 Runtime 依赖并执行启动检查")
			if err := install.RunCommand(ctx, progress, filepath.Join(filepath.Dir(exe), "semantic"), args...); err != nil {
				return nil, err
			}
			if err := sim.ReloadResources(ctx, cfg.Simulation.RuntimesDir, cfg.Simulation.CatalogDir); err != nil {
				return nil, err
			}
			return []string{record.Name + "@" + record.Version}, nil
		case "robot_skill":
			body, err := os.ReadFile(archive)
			if err != nil {
				return nil, err
			}
			item, err := robots.PublishSkillArchive(bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			if options.RobotID != "" {
				instance, err := st.GetLatestRuntimeByRobot(ctx, options.RobotID)
				if err != nil || instance.ProjectID != projectID {
					return nil, fmt.Errorf("请选择本项目的受管 Robot")
				}
				// 复用既有 desired Skill 对账；正在执行的版本由原有机制保留。
				if _, err := robots.SetDesiredSkill(options.RobotID, item.Name, item.Version, true); err != nil {
					return nil, err
				}
			}
			return []string{item.Name + "@" + item.Version}, nil
		}
		item, err := components.Install(ctx, archive, record.ID, progress)
		if err != nil {
			return nil, err
		}
		resources := []string{item.Name + "@" + item.Version}
		switch item.Kind {
		case "robot_ability", "model":
			if options.ProjectDefault {
				if err := components.SetProjectDefault(projectID, item.ID); err != nil {
					return nil, err
				}
				progress("已设为项目对应型号的默认组件，首次启动 Robot 时生效")
			}
			if options.RobotID != "" {
				instance, err := st.GetLatestRuntimeByRobot(ctx, options.RobotID)
				if err != nil {
					return nil, fmt.Errorf("请选择本项目的受管 Robot: %w", err)
				}
				if instance.ProjectID != projectID {
					return nil, fmt.Errorf("Robot 不属于当前项目")
				}
				if _, err := components.Bind(item.ID, options.RobotID, instance.RobotModel); err != nil {
					return nil, err
				}
				progress("已绑定组件版本；目标 Robot 下次启动时生效，当前执行保持原版本")
				if options.ApplyNow {
					if app.managedRobots == nil {
						return nil, fmt.Errorf("组件已安装，当前未启用受管 Robot")
					}
					if err := app.managedRobots.applyInstalledComponents(ctx, projectID, options.RobotID, sim); err != nil {
						return nil, fmt.Errorf("组件已安装，Robot 生效未完成: %w", err)
					}
					progress("目标 Robot 已启动并通过就绪检查，新绑定已经生效")
				}
			} else {
				progress("组件安装完成，可选择项目 Robot 绑定")
			}
		case "scene_catalog":
			source := filepath.Dir(filepath.Join(item.Root, item.SceneCatalog))
			// 目录组件将目录与原生引用文件放在独立子目录，保持原有相对路径。
			if source == item.Root {
				return nil, fmt.Errorf("scene_catalog 请放在包内独立的 catalog 子目录")
			}
			catalog, err := simulation.LoadSceneCatalog(source)
			if err != nil {
				return nil, err
			}
			destination := filepath.Join(cfg.Simulation.CatalogDir, "component-"+item.Name)
			for _, entry := range catalog.List("") {
				if err := sim.CheckRuntimeInstallIdle(ctx, entry.CompatibleRuntimeProfile); err != nil {
					return nil, err
				}
			}
			if err := install.ReplaceDirectoryAndApply(source, destination, func() error {
				if err := os.WriteFile(filepath.Join(destination, ".component-id"), []byte(item.ID), 0640); err != nil {
					return err
				}
				return sim.ReloadResources(ctx, cfg.Simulation.RuntimesDir, cfg.Simulation.CatalogDir)
			}); err != nil {
				return nil, err
			}
			// 场景包可包含整套原生任务；登记到可用目录后由用户选择加入项目，
			// 避免导入一个 LIBERO 数据包就给项目增加全部 benchmark 场景。
			progress("场景包已安装，请在场景配置中选择任务与初态加入项目")
			if options.GeneratePreviews == nil || *options.GeneratePreviews {
				ids := options.SceneIDs
				if len(ids) == 0 {
					for _, entry := range catalog.List("") {
						ids = append(ids, entry.SceneID)
					}
				}
				// 只准备本次包内选择的任务，不能通过安装参数操作其他场景。
				for _, id := range ids {
					if _, err := catalog.Get(id); err != nil {
						return resources, err
					}
				}
				sim.PrepareScenePreviews(ctx, ids, progress)
			}
		case "robot_base":
			if item.BundleManifest != "" {
				source := filepath.Dir(filepath.Join(item.Root, item.BundleManifest))
				if source == item.Root {
					return nil, fmt.Errorf("bundle_manifest 请放在独立的 robot 子目录")
				}
				if err := robotruntime.RegisterInstalledBundle(cfg.RobotRuntime.BundlesDir, source); err != nil {
					return nil, err
				}
				if app.managedRobots != nil {
					if err := app.managedRobots.orchestrator.ReloadCatalog(cfg.RobotRuntime.BundlesDir); err != nil {
						return nil, err
					}
				}
			}
			// 底座只登记 Robot 的启动配置；Ability、模型和 Skill 独立安装。
			if options.ApplyNow && options.RobotID != "" && app.managedRobots != nil {
				if err := app.managedRobots.applyInstalledComponents(ctx, projectID, options.RobotID, sim); err != nil {
					return resources, err
				}
			}
		default:
			return nil, fmt.Errorf("不支持安装组件类型 %s，请使用独立组件包", item.Kind)
		}
		return resources, nil
	}
	return func(ctx context.Context, projectID string, record install.Record, archive string, options install.Options, progress func(string)) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		project, err := st.GetProject(projectID)
		if err != nil {
			return nil, err
		}
		if project.ArchivedAt != nil || project.Mode != store.ProjectModeDevelopment {
			return nil, fmt.Errorf("请在开发模式项目中安装组件")
		}
		return apply(ctx, projectID, record, archive, options, progress)
	}
}
