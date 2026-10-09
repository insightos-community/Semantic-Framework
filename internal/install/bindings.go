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
	"os"
	"path/filepath"
	"slices"
	"time"
)

// RobotBinding 是安装器与实例启动器之间的文件协议。只引用已经安装并校验的
// 制品；绑定的修改不会触碰运行中实例。实例启动时将此文件复制为执行快照。
type RobotBinding struct {
	RobotID    string                  `json:"robot_id"`
	RobotModel string                  `json:"robot_model"`
	Revision   string                  `json:"revision"`
	Abilities  map[string]BoundAbility `json:"abilities"`
	Model      *BoundModel             `json:"model,omitempty"`
}
type BoundAbility struct {
	Name           string `json:"name,omitempty"`
	Version        string `json:"version,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
	ComponentID    string `json:"component_id"`
	Template       string `json:"template"`
	AbilityName    string `json:"ability_name"`
	Package        string `json:"package"`
	Python         string `json:"python"`
}
type BoundModel struct {
	Name        string `json:"name,omitempty"`
	Version     string `json:"version,omitempty"`
	ComponentID string `json:"component_id"`
	Config      string `json:"config"`
}

func BindingPath(root, robotID string) string {
	// Robot ID 可能含冒号，摘要作为目录名，同时在文件内保留完整业务身份。
	return filepath.Join(root, "robot-bindings", Digest([]byte(robotID)), "current.json")
}

func (s *ComponentStore) Bind(id, robotID, model string) (RobotBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Get(id)
	if err != nil {
		return RobotBinding{}, err
	}
	if robotID == "" || !slices.Contains(item.RobotModels, model) {
		return RobotBinding{}, fmt.Errorf("组件 %s 不适用于 Robot 型号 %s", item.Name, model)
	}
	path := BindingPath(s.Root, robotID)
	binding := RobotBinding{RobotID: robotID, RobotModel: model, Abilities: map[string]BoundAbility{}}
	if body, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(body, &binding); err != nil {
			return binding, err
		}
	} else if !os.IsNotExist(err) {
		return binding, err
	}
	if binding.RobotID != robotID || binding.RobotModel != model {
		return binding, fmt.Errorf("Robot 绑定身份不一致")
	}
	if binding.Abilities == nil {
		binding.Abilities = map[string]BoundAbility{}
	}
	switch item.Kind {
	case "robot_ability":
		for _, a := range item.Abilities {
			binding.Abilities[a.Role] = BoundAbility{Name: item.Name, Version: item.Version, SourceRevision: item.SourceRevision, ComponentID: id, Template: a.Template, AbilityName: a.AbilityName, Package: filepath.Join(item.Root, a.Package), Python: item.PythonExecutable}
		}
	case "model":
		binding.Model = &BoundModel{Name: item.Name, Version: item.Version, ComponentID: id, Config: filepath.Join(item.Root, item.ModelConfig)}
	default:
		return binding, fmt.Errorf("%s 不需要 Robot 组件绑定", item.Kind)
	}
	binding.Revision = time.Now().UTC().Format("20060102T150405.000000000")
	body, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return binding, err
	}
	if err := writeAtomic(filepath.Join(filepath.Dir(path), "history", binding.Revision+".json"), body); err != nil {
		return binding, err
	}
	return binding, writeAtomic(path, body)
}

// 独立组件的型号默认绑定属于 Project。首次创建 Robot 时复制为独立绑定，
// 后续导入不会改动已存在 Robot 的执行快照或显式版本选择。
func projectDefaultKey(projectID, model string) string {
	return "project-default:" + projectID + ":" + model
}

func (s *ComponentStore) ProjectDefault(projectID, model string) (*RobotBinding, error) {
	body, err := os.ReadFile(BindingPath(s.Root, projectDefaultKey(projectID, model)))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var binding RobotBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		return nil, err
	}
	return &binding, nil
}
func (s *ComponentStore) SetProjectDefault(projectID, componentID string) error {
	item, err := s.Get(componentID)
	if err != nil {
		return err
	}
	for _, model := range item.RobotModels {
		if _, err := s.Bind(componentID, projectDefaultKey(projectID, model), model); err != nil {
			return err
		}
	}
	return nil
}
func (s *ComponentStore) EnsureRobotBinding(projectID, robotID, model string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := BindingPath(s.Root, robotID)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	body, err := os.ReadFile(BindingPath(s.Root, projectDefaultKey(projectID, model)))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var binding RobotBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		return "", err
	}
	binding.RobotID = robotID
	body, err = json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeAtomic(filepath.Join(filepath.Dir(path), "history", binding.Revision+".json"), body); err != nil {
		return "", err
	}
	return path, writeAtomic(path, body)
}

func (s *ComponentStore) Get(id string) (InstalledComponent, error) {
	if len(id) != 64 || !componentSegment(id) {
		return InstalledComponent{}, fmt.Errorf("组件 ID 无效")
	}
	body, err := os.ReadFile(filepath.Join(s.Root, "receipts", id+".json"))
	if err != nil {
		return InstalledComponent{}, err
	}
	var item InstalledComponent
	err = json.Unmarshal(body, &item)
	return item, err
}

func (s *ComponentStore) Rollback(robotID, revision string) (RobotBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !componentSegment(revision) {
		return RobotBinding{}, fmt.Errorf("绑定修订无效")
	}
	path := BindingPath(s.Root, robotID)
	body, err := os.ReadFile(filepath.Join(filepath.Dir(path), "history", revision+".json"))
	if err != nil {
		return RobotBinding{}, err
	}
	var binding RobotBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		return binding, err
	}
	if binding.RobotID != robotID {
		return binding, fmt.Errorf("历史绑定的 Robot 不匹配")
	}
	for _, a := range binding.Abilities {
		if _, err := s.Get(a.ComponentID); err != nil {
			return binding, err
		}
	}
	if binding.Model != nil {
		if _, err := s.Get(binding.Model.ComponentID); err != nil {
			return binding, err
		}
	}
	return binding, writeAtomic(path, body)
}
