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

package config

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidationError 聚合配置 schema 校验发现的全部问题，
// 每条问题都带 yaml 路径，便于一次性修完而不是逐个试错。
type ValidationError struct {
	// Problems 全部校验问题，格式为 "<yaml 路径>: <问题描述>"。
	Problems []string
}

// Error 返回聚合后的可读错误（每行一条问题）。
func (e *ValidationError) Error() string {
	return fmt.Sprintf("配置 schema 校验未通过，共 %d 处问题:\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// validateYAML 对 yaml 原文做 fail-closed 严格校验，发现问题聚合返回：
//  1. 未知键——按 Config 结构树逐段比对（llm.providers 的条目名是动态 key，
//     不参与比对，但条目内部字段仍按 LLMProviderConfig 校验）；
//  2. 类型错误——标量无法解码到字段类型（含 Duration 解析失败）。
//
// 为什么不用 yaml.Decoder.KnownFields：它只在首个未知字段处报错且不含
// 完整路径，无法满足"聚合全部问题 + yaml 路径"的要求，因此自行遍历节点树。
func validateYAML(data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("yaml 语法错误: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil // 空文件等价于全默认值
	}

	var problems []string
	walkYAMLNode(doc.Content[0], reflect.TypeOf(Config{}), "", &problems)
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// walkYAMLNode 用结构体类型 t 的 yaml tag 白名单递归比对映射节点：
// node 必须是映射，且每个键都能命中 t 的某个字段，否则记一条问题。
func walkYAMLNode(node *yaml.Node, t reflect.Type, path string, problems *[]string) {
	if node.Kind != yaml.MappingNode {
		*problems = append(*problems, fmt.Sprintf("%s: 类型错误：应为键值映射，实际为标量或列表", displayPath(path)))
		return
	}

	fields := yamlFieldTypes(t)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		childPath := key.Value
		if path != "" {
			childPath = path + "." + key.Value
		}

		fieldType, ok := fields[key.Value]
		if !ok {
			*problems = append(*problems, fmt.Sprintf("%s: 未知配置键", childPath))
			continue
		}
		checkYAMLValue(value, fieldType, childPath, problems)
	}
}

// checkYAMLValue 校验单个值节点与字段类型是否匹配：
// 结构体/结构体 map/结构体切片递归下钻，其余类型直接试解码判定。
func checkYAMLValue(node *yaml.Node, t reflect.Type, path string, problems *[]string) {
	switch {
	case t.Kind() == reflect.Struct:
		walkYAMLNode(node, t, path, problems)

	case t.Kind() == reflect.Map && t.Elem().Kind() == reflect.Struct:
		// llm.providers：条目名是动态 key，逐条按元素类型校验。
		if node.Kind != yaml.MappingNode {
			*problems = append(*problems, fmt.Sprintf("%s: 类型错误：应为键值映射", path))
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			checkYAMLValue(node.Content[i+1], t.Elem(), path+"."+node.Content[i].Value, problems)
		}

	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct:
		// mcp_servers：列表元素是结构体，逐个下钻校验元素内部字段，
		// 未知键同样 fail-closed（路径形如 mcp_servers[0]）。
		if node.Kind != yaml.SequenceNode {
			*problems = append(*problems, fmt.Sprintf("%s: 类型错误：应为列表", path))
			return
		}
		for i, elem := range node.Content {
			walkYAMLNode(elem, t.Elem(), fmt.Sprintf("%s[%d]", path, i), problems)
		}

	default:
		// 字符串类字段（含 Duration）显式拦截 yaml 推断的非字符串标量：
		// yaml.v3 会把 !!int 8080 宽松解码成 "8080"，不拦则类型笔误被吞掉。
		if isStringLikeType(t) && node.Tag != "!!str" {
			*problems = append(*problems, fmt.Sprintf("%s: 类型错误：应为字符串，实际为 %s", path, node.Tag))
			return
		}
		// 标量与宽松容器（[]string、map[string]any 等）统一试解码：
		// Duration 的自定义 UnmarshalYAML 也在此触发，解析失败即类型错误。
		if err := node.Decode(reflect.New(t).Interface()); err != nil {
			*problems = append(*problems, fmt.Sprintf("%s: 类型错误：%v", path, err))
		}
	}
}

// isStringLikeType 判定字段类型是否按字符串标量消费
// （string 本体，或以字符串为 yaml 表示的 Duration）。
func isStringLikeType(t reflect.Type) bool {
	return t.Kind() == reflect.String || t == reflect.TypeOf(Duration(0))
}

// yamlFieldTypes 提取结构体的 yaml 键 → 字段类型映射（取 tag 首段，忽略 "-"）。
func yamlFieldTypes(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = f.Type
	}
	return fields
}

// displayPath 处理根路径为空时的展示（根节点问题显示为 "<root>"）。
func displayPath(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}
