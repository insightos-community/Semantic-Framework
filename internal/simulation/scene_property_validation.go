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
	"encoding/json"
	"math"
)

// validateNodeProperties 只接受 native 编译器会实际消费的属性。
//
// 属性校验放在 Framework 边界，是为了让 Studio 可以把问题定位到具体节点；
// Plugin 仍会执行同样的独立校验，防止其他客户端绕过 Framework。
func validateNodeProperties(node SceneNode) []ValidationIssue {
	allowed := map[string]map[string]bool{
		"group": {},
		"object": {
			"model": true, "category": true, "size": true, "rgba": true,
			"color_rgba": true, "static": true, "interactive": true,
			"mass": true, "inertia": true, "friction": true,
			"material": true, "collision": true,
		},
		"robot": {"model": true, "robot_id": true, "sensor_names": true},
		"camera": {
			"robot_id": true, "sensor_kind": true, "width": true, "height": true,
			"fps": true, "intrinsics": true, "fovy": true,
		},
		"light": {
			"direction": true, "diffuse": true, "specular": true,
			"castshadow": true, "active": true,
		},
		"region": {
			"model": true, "category": true, "size": true, "rgba": true,
			"color_rgba": true, "static": true, "interactive": true,
			"material": true, "collision": true,
		},
	}
	properties, known := allowed[node.Kind]
	if !known {
		return nil
	}
	issues := make([]ValidationIssue, 0)
	problem := func(field, message string) {
		issues = append(issues, ValidationIssue{
			Level: "error", NodeID: node.ID, Field: "properties." + field, Message: message,
		})
	}
	for key := range node.Properties {
		if !properties[key] {
			problem(key, "该节点类型不支持属性 "+key)
		}
	}
	if value, ok := node.Properties["size"]; ok && !positiveNumberVector(value, 3) {
		problem("size", "size 必须是三个大于零的米制数值")
	}
	for _, field := range []string{"rgba", "color_rgba"} {
		if value, ok := node.Properties[field]; ok && !boundedNumberVector(value, 4, 0, 1) {
			problem(field, field+" 必须是四个 0 到 1 的数值")
		}
	}
	if value, ok := node.Properties["mass"]; ok && !positiveNumber(value) {
		problem("mass", "mass 必须是大于零的千克数值")
	}
	if value, ok := node.Properties["inertia"]; ok && !positiveNumberVector(value, 3) {
		problem("inertia", "inertia 必须是三个大于零的主惯量")
	}
	if value, ok := node.Properties["friction"]; ok &&
		!boundedNumberVector(value, 3, 0, math.Inf(1)) {
		problem("friction", "friction 必须是三个非负数值")
	}
	for _, field := range []string{"static", "interactive"} {
		if value, ok := node.Properties[field]; ok {
			if _, valid := value.(bool); !valid {
				problem(field, field+" 必须是布尔值")
			}
		}
	}
	if value, ok := node.Properties["sensor_names"]; ok && !stringList(value) {
		problem("sensor_names", "sensor_names 必须是字符串数组")
	}
	for _, field := range []string{"width", "height", "fps", "fovy"} {
		if value, ok := node.Properties[field]; ok && !positiveNumber(value) {
			problem(field, field+" 必须是大于零的数值")
		}
	}
	for _, field := range []string{"direction", "diffuse", "specular"} {
		if value, ok := node.Properties[field]; ok &&
			!boundedNumberVector(value, 3, -math.MaxFloat64, math.MaxFloat64) {
			problem(field, field+" 必须是三个数值")
		}
	}
	for _, field := range []string{"castshadow", "active"} {
		if value, ok := node.Properties[field]; ok {
			if _, valid := value.(bool); !valid {
				problem(field, field+" 必须是布尔值")
			}
		}
	}
	if value, ok := node.Properties["material"]; ok {
		material, valid := value.(map[string]any)
		if !valid {
			problem("material", "material 必须是对象")
		} else {
			for key := range material {
				if key != "rgba" {
					problem("material."+key, "不支持的材质属性 "+key)
				}
			}
			if rgba, present := material["rgba"]; present &&
				!boundedNumberVector(rgba, 4, 0, 1) {
				problem("material.rgba", "material.rgba 必须是四个 0 到 1 的数值")
			}
		}
	}
	if value, ok := node.Properties["collision"]; ok {
		collision, valid := value.(map[string]any)
		if !valid {
			problem("collision", "collision 必须是对象")
		} else {
			for key := range collision {
				if !map[string]bool{
					"enabled": true, "contype": true, "conaffinity": true, "friction": true,
				}[key] {
					problem("collision."+key, "不支持的碰撞属性 "+key)
				}
			}
			if enabled, present := collision["enabled"]; present {
				if _, valid := enabled.(bool); !valid {
					problem("collision.enabled", "collision.enabled 必须是布尔值")
				}
			}
			for _, field := range []string{"contype", "conaffinity"} {
				if value, present := collision[field]; present {
					number, valid := numeric(value)
					if !valid || number < 0 || number != math.Trunc(number) {
						problem("collision."+field, "碰撞分组必须是非负整数")
					}
				}
			}
			if friction, present := collision["friction"]; present &&
				!boundedNumberVector(friction, 3, 0, math.Inf(1)) {
				problem("collision.friction", "collision.friction 必须是三个非负数值")
			}
		}
	}
	return issues
}

func numeric(value any) (float64, bool) {
	switch item := value.(type) {
	case float64:
		return item, !math.IsNaN(item) && !math.IsInf(item, 0)
	case float32:
		value := float64(item)
		return value, !math.IsNaN(value) && !math.IsInf(value, 0)
	case int:
		return float64(item), true
	case int64:
		return float64(item), true
	case json.Number:
		result, err := item.Float64()
		return result, err == nil && !math.IsNaN(result) && !math.IsInf(result, 0)
	default:
		return 0, false
	}
}

func positiveNumber(value any) bool {
	item, ok := numeric(value)
	return ok && item > 0
}

func numberVector(value any) ([]float64, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var result []float64
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, false
	}
	for _, item := range result {
		if math.IsNaN(item) || math.IsInf(item, 0) {
			return nil, false
		}
	}
	return result, true
}

func boundedNumberVector(value any, size int, minimum, maximum float64) bool {
	items, ok := numberVector(value)
	if !ok || len(items) != size {
		return false
	}
	for _, item := range items {
		if item < minimum || item > maximum {
			return false
		}
	}
	return true
}

func positiveNumberVector(value any, size int) bool {
	items, ok := numberVector(value)
	if !ok || len(items) != size {
		return false
	}
	for _, item := range items {
		if item <= 0 {
			return false
		}
	}
	return true
}

func stringList(value any) bool {
	if _, ok := value.([]string); ok {
		return true
	}
	values, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range values {
		if _, valid := item.(string); !valid {
			return false
		}
	}
	return true
}
