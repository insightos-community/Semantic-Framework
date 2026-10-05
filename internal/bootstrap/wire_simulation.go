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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"insightos.cn/semantic-framework/configs"
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
)

type projectWorkspaceResolver struct {
	store *store.Store
}

func (r projectWorkspaceResolver) ProjectWorkspace(projectID string) (string, error) {
	project, err := r.store.GetProject(projectID)
	if err != nil {
		return "", err
	}
	return project.WorkspaceRoot, nil
}

// newConfiguredSimulationServices 是生产装配入口。Runtime、场景目录与资产目录
// 任一配置错误都会阻止 Server 以“看似可用”的半配置状态启动。
func newConfiguredSimulationServices(
	st *store.Store, cfg *config.Config, bus *event.Bus,
) (*simulation.Service, *simulation.SceneAuthoringService, error) {
	assetCatalog, err := configuredSceneAssetCatalog()
	if err != nil {
		return nil, nil, err
	}
	installations, err := configuredRuntimeInstallations(cfg.Simulation.RuntimesDir)
	if err != nil {
		return nil, nil, err
	}
	sceneCatalog, err := configuredSceneCatalog(cfg.Simulation.CatalogDir)
	if err != nil {
		return nil, nil, err
	}
	bindings := make([]simulation.RuntimeBinding, 0)
	for _, installation := range installations.List() {
		if installation.Enabled {
			bindings = append(bindings, installation.Binding())
		}
	}
	registry, err := simulation.NewRuntimeRegistry(bindings...)
	if err != nil {
		return nil, nil, err
	}
	fileStore := simulation.NewFileStore(projectWorkspaceResolver{store: st})
	service := simulation.NewServiceWithRegistry(registry, fileStore, simulationMapSync{store: st, bus: bus})
	service.ConfigureProjectResources(installations, sceneCatalog,
		projectSimulationResolver{store: st})
	authoring, err := simulation.NewSceneAuthoringServiceWithCatalog(fileStore, assetCatalog)
	if err != nil {
		return nil, nil, fmt.Errorf("初始化 Scene Authoring 失败: %w", err)
	}
	return service, authoring, nil
}

// 未运行 semantic init 的测试和源码开发进程可以使用编译期内置只读目录。
// 只有配置仍是默认相对路径且磁盘目录确实不存在时才回退；显式配置错误必须
// 直接失败，不能被内置模板掩盖。
func configuredRuntimeInstallations(dir string) (*simulation.RuntimeInstallationCatalog, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) &&
		filepath.Clean(dir) == filepath.FromSlash("configs/runtimes.d") {
		return simulation.LoadRuntimeInstallationsFS(configs.Templates, "runtimes.d")
	}
	return simulation.LoadRuntimeInstallations(dir)
}

func configuredSceneCatalog(dir string) (*simulation.SceneCatalogService, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) &&
		filepath.Clean(dir) == filepath.FromSlash("configs/scenes.d") {
		return simulation.LoadSceneCatalogFS(configs.Templates, "scenes.d")
	}
	return simulation.LoadSceneCatalog(dir)
}

func configuredSceneAssetCatalog() (simulation.SceneAssetCatalog, error) {
	filePath := strings.TrimSpace(os.Getenv("SEMANTIC_SIMULATION_ASSET_CATALOG"))
	if filePath == "" {
		return simulation.DefaultSceneAssetCatalog(), nil
	}
	catalog, err := simulation.LoadSceneAssetCatalog(filePath)
	if err != nil {
		return simulation.SceneAssetCatalog{}, fmt.Errorf(
			"加载 SEMANTIC_SIMULATION_ASSET_CATALOG=%q 失败: %w", filePath, err,
		)
	}
	return catalog, nil
}
