package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"insightos.cn/semantic-framework/internal/install"
	"insightos.cn/semantic-framework/internal/simulation"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/config"
)

// 卸载先检查当前绑定和活动执行，再撤销目录索引。历史执行快照保留，
// 不将历史版本的引用误判为仍在运行；需要重放旧版本时从保留的原包重装。
func componentRemovalCheck(app *App, cfg *config.Config, st *store.Store, sim *simulation.Service) func(context.Context, install.InstalledComponent) error {
	return func(ctx context.Context, item install.InstalledComponent) error {
		instances, err := st.ListRuntimeInstances(ctx)
		if err != nil {
			return err
		}
		for _, instance := range instances {
			if !instance.Status.Active() {
				continue
			}
			if item.Kind == "robot_base" && instance.BundleName == item.Name && instance.BundleVersion == item.Version {
				return fmt.Errorf("Robot %s 正在使用此底座，请先停止场景", instance.RobotID)
			}
			body, err := os.ReadFile(filepath.Join(instance.DataDirectory, "run", "components.json"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			var binding install.RobotBinding
			if err := json.Unmarshal(body, &binding); err != nil {
				return err
			}
			if install.BindingUses(binding, item.ID) {
				return fmt.Errorf("Robot %s 的活动实例仍使用此组件，请先停止场景", instance.RobotID)
			}
		}
		switch item.Kind {
		case "robot_ability", "model":
			return nil
		case "scene_catalog":
			directory := filepath.Join(cfg.Simulation.CatalogDir, "component-"+item.Name)
			owner, err := os.ReadFile(filepath.Join(directory, ".component-id"))
			if os.IsNotExist(err) {
				if _, err := os.Stat(directory); os.IsNotExist(err) {
					return nil
				}
				return fmt.Errorf("早期场景目录尚无独立组件归属，请先重新导入该场景包")
			}
			if err != nil {
				return err
			}
			// 同名的新版本已经接管目录时，只回收旧版本安装文件。
			if string(owner) != item.ID {
				return nil
			}
			catalog, err := simulation.LoadSceneCatalog(filepath.Dir(filepath.Join(item.Root, item.SceneCatalog)))
			if err != nil {
				return err
			}
			for _, entry := range catalog.List("") {
				ids, err := st.ProjectsUsingCatalogScene(entry.SceneID)
				if err != nil {
					return err
				}
				if len(ids) > 0 {
					return fmt.Errorf("场景 %s 仍被项目 %v 引用，请先移除项目场景引用", entry.Name, ids)
				}
				if err := sim.CheckRuntimeInstallIdle(ctx, entry.CompatibleRuntimeProfile); err != nil {
					return err
				}
			}
			// 先移出索引目录，目录重载成功后才回收数据；重载失败恢复原位置。
			retired := filepath.Join(app.components.Root, "retired-catalog-"+item.ID)
			if err := os.Rename(directory, retired); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := sim.ReloadResources(ctx, cfg.Simulation.RuntimesDir, cfg.Simulation.CatalogDir); err != nil {
				_ = os.Rename(retired, directory)
				return err
			}
			return os.RemoveAll(retired)
		case "robot_base":
			// 底座目录索引按发布名称登记，只撤销仍指向本修订的条目。
			path := filepath.Join(cfg.RobotRuntime.BundlesDir, "installed-bundles.json")
			body, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			var index map[string]string
			if err := json.Unmarshal(body, &index); err != nil {
				return err
			}
			root := filepath.Dir(filepath.Join(item.Root, item.BundleManifest))
			for name, value := range index {
				if value == root {
					delete(index, name)
				}
			}
			next, err := json.MarshalIndent(index, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, next, 0640); err != nil {
				return err
			}
			if app.managedRobots != nil {
				if err := app.managedRobots.orchestrator.ReloadCatalog(cfg.RobotRuntime.BundlesDir); err != nil {
					_ = os.WriteFile(path, body, 0640)
					return err
				}
			}
			return nil
		default:
			return fmt.Errorf("旧组合安装记录仅保留历史，不支持作为独立组件卸载")
		}
	}
}
