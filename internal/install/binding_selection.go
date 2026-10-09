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
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

// SelectBinding 原子保存用户选择的整套组件。不重新解包、不准备新环境，也不启动
// Robot。先校验全部选择，再写历史和待生效绑定，避免更换 Ability+模型时出现半套组合。
// 旧清单仍可沿用原安装入口；新的可切换模型必须声明兼容性，避免“同型号即兼容”。
func (s *ComponentStore) SelectBinding(ids []string, robotID, model, profile string) (RobotBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := RobotBinding{RobotID: robotID, RobotModel: model, Abilities: map[string]BoundAbility{}}
	if robotID == "" || model == "" || len(ids) == 0 {
		return binding, fmt.Errorf("请选择 Robot 型号及已安装组件")
	}
	abilities := map[string]AbilityComponent{}
	var requirement *ModelCompatibility
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return binding, fmt.Errorf("组件选择重复")
		}
		seen[id] = true
		item, err := s.Get(id)
		if err != nil {
			return binding, err
		}
		if !slices.Contains(item.RobotModels, model) {
			return binding, fmt.Errorf("%s 不适用于 %s", item.Name, model)
		}
		switch item.Kind {
		case "robot_ability":
			for _, a := range item.Abilities {
				if _, exists := abilities[a.Role]; exists {
					return binding, fmt.Errorf("角色 %s 只能选择一个 Ability 实现", a.Role)
				}
				abilities[a.Role] = a
				binding.Abilities[a.Role] = BoundAbility{Name: item.Name, Version: item.Version, SourceRevision: item.SourceRevision, ComponentID: id, Template: a.Template, AbilityName: a.AbilityName, Package: filepath.Join(item.Root, a.Package), Python: item.PythonExecutable}
			}
		case "model":
			if binding.Model != nil {
				return binding, fmt.Errorf("一个 Robot 当前只能选择一个策略模型")
			}
			if item.ModelCompatibility == nil {
				return binding, fmt.Errorf("模型 %s 缺少兼容性声明，请从源码重新构建模型包", item.Name)
			}
			requirement = item.ModelCompatibility
			binding.Model = &BoundModel{Name: item.Name, Version: item.Version, ComponentID: id, Config: filepath.Join(item.Root, item.ModelConfig)}
		default:
			return binding, fmt.Errorf("请选择 Ability 或模型；运行支持通过安装登记，Skill 使用设备页安装")
		}
	}
	if requirement != nil {
		a, ok := abilities[requirement.Role]
		if !ok || a.AbilityName != requirement.AbilityName || !slices.Contains(a.ModelBackends, requirement.Backend) {
			return binding, fmt.Errorf("模型需要 %s 角色的 %s，且 Ability 支持 %s 后端", requirement.Role, requirement.AbilityName, requirement.Backend)
		}
		if profile == "" || !slices.Contains(requirement.RuntimeProfiles, profile) {
			return binding, fmt.Errorf("模型不兼容 Runtime %s", profile)
		}
	}
	for role, a := range abilities {
		if len(a.ModelBackends) > 0 && (requirement == nil || requirement.Role != role) {
			return binding, fmt.Errorf("请为 %s Ability 选择兼容的模型", role)
		}
	}
	binding.Revision = time.Now().UTC().Format("20060102T150405.000000000")
	body, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return binding, err
	}
	path := BindingPath(s.Root, robotID)
	if err := writeAtomic(filepath.Join(filepath.Dir(path), "history", binding.Revision+".json"), body); err != nil {
		return binding, err
	}
	return binding, writeAtomic(path, body)
}

// 项目默认只影响后续首次建立绑定的 Robot；现有 Robot 的显式选择保持不变。
func (s *ComponentStore) SelectProjectDefault(ids []string, projectID, model, profile string) (RobotBinding, error) {
	return s.SelectBinding(ids, projectDefaultKey(projectID, model), model, profile)
}
