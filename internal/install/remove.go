package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// List 读取安装收据，而非扫描组件目录猜测安装是否完成。
func (s *ComponentStore) List() ([]InstalledComponent, error) {
	paths, err := filepath.Glob(filepath.Join(s.Root, "receipts", "*.json"))
	if err != nil {
		return nil, err
	}
	items := []InstalledComponent{}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var item InstalledComponent
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func BindingUses(binding RobotBinding, id string) bool {
	if binding.Model != nil && binding.Model.ComponentID == id {
		return true
	}
	for _, ability := range binding.Abilities {
		if ability.ComponentID == id {
			return true
		}
	}
	return false
}

// Remove 只回收指定摘要的安装目录和环境。导入原包与历史绑定保留，方便
// 重新安装后恢复；历史绑定本身不代表组件仍在运行，回退时会重新检查收据。
// beforeRemove 由应用层核对活动实例并撤销场景/底座索引，不引入执行业务。
func (s *ComponentStore) Remove(ctx context.Context, id string, beforeRemove func(context.Context, InstalledComponent) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(id)
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(s.Root, "robot-bindings", "*", "current.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var binding RobotBinding
		if err := json.Unmarshal(body, &binding); err != nil {
			return err
		}
		if BindingUses(binding, id) {
			return fmt.Errorf("组件仍绑定到 %s，请先选择替代版本", binding.RobotID)
		}
	}
	if beforeRemove != nil {
		if err := beforeRemove(ctx, item); err != nil {
			return err
		}
	}
	// 使用本地收据的摘要定位固定子目录，不接受客户端或收据中的任意递归删除路径。
	for _, directory := range []string{"environments", "components"} {
		if err := os.RemoveAll(filepath.Join(s.Root, directory, id)); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(s.Root, "receipts", id+".json"))
}
