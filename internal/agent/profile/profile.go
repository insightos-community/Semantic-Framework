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

package profile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Mode 是角色的交互模式（架构文档 04 §3），决定运行时的装配方式。
type Mode string

const (
	// ModeCoordinator 协调者：用户-facing，接指令、规划、分派、决策、汇报。
	ModeCoordinator Mode = "coordinator"

	// ModeWorker 工作者：长任务执行，被任务系统分派。
	ModeWorker Mode = "worker"

	// ModeService 服务者：请求-响应，短调用出结果。
	ModeService Mode = "service"
)

// validModes 是合法的交互模式集合。
var validModes = map[Mode]struct{}{
	ModeCoordinator: {},
	ModeWorker:      {},
	ModeService:     {},
}

const (
	// defaultInstructionFile 是默认的角色提示词文件名。
	defaultInstructionFile = "AGENT.md"

	// safetyFile 是可选的全局安全总则文件名（存在则加载，非必须）。
	safetyFile = "SAFETY.md"

	// defaultMaxTurns 是默认的 ReAct 最大轮次。
	defaultMaxTurns = 10

	// defaultContextTokens 是默认的上下文 token 预算。
	defaultContextTokens = 120000
)

// Limits 定义角色的运行限额。
type Limits struct {
	// MaxTurns ReAct 最大轮次（模型 ↔ 工具循环上限）。
	MaxTurns int

	// ContextTokens 上下文 token 预算；历史压缩按预算比例触发。
	ContextTokens int
}

// ToolsConfig 定义角色可用的工具范围。
type ToolsConfig struct {
	// Namespaces 允许的 MCP 命名空间（如 system.* / artifact.*）。
	Namespaces []string

	// Pinned 常驻工具清单（精确工具名）：启用 ToolSearch 时这些工具
	// 不经检索始终注入（runtime.buildToolchain 消费：pinned/动态集切分）。
	Pinned []string

	// ToolSearch 启用 ToolSearch 动态检索。开启时非 pinned 工具始终进入
	// 动态集；关闭时全部工具直通，行为不随工具数量变化。
	ToolSearch bool
}

// InterruptConfig 定义需要人工审批的工具范围。
type InterruptConfig struct {
	// ApprovalRequired 这些命名空间的调用需人工审批后才执行。
	ApprovalRequired []string
}

// MemoryConfig 定义角色的记忆能力开关。
type MemoryConfig struct {
	// LongTerm 启用长期知识库检索（Phase 2 落地）。
	LongTerm bool
}

// SkillsConfig 定义角色能够使用的 Skill 白名单。Project 可以在会话运行时
// 继续收窄该集合，但不能给 Agent 增加 Profile 未授权的 Skill。
type SkillsConfig struct {
	// Allowlist 是允许加载的 Skill 名称；空列表表示该 Agent 不使用 Skill。
	Allowlist []string
}

// SubAgentConfig 定义角色作为 SubAgent（agent-as-tool 委派目标，
// 架构文档 04 §7.3）的对外呈现。仅 service 模式角色可启用——委派是
// 请求-响应短调用语义，coordinator/worker 的装配形态与
// "被 leader 同步调用出结果"不兼容（加载期校验，见 buildProfile）。
type SubAgentConfig struct {
	// Enabled 启用后该成员在 Team 组建时注册进 SubAgent 注册表，
	// leader 装配时包装为委派工具注入工具集。
	Enabled bool

	// ToolName 委派工具名（leader 模型侧看到的函数名，如 ask_query）；
	// 启用时必填，且必须满足模型端点的函数名约束 [a-zA-Z0-9_-]+。
	ToolName string

	// ToolDescription 委派工具描述：leader 模型判断委派时机的唯一依据，
	// 启用时必填（空描述 = 委派能力对模型不可发现）。
	ToolDescription string

	// MaxTurns 委派执行的 ReAct 轮次上限；≤0 时沿用 limits.max_turns。
	MaxTurns int
}

// Profile 是一个已加载的角色 profile：role.yaml 的配置 + AGENT.md 正文。
type Profile struct {
	// Name 角色名（与目录名一致，Load 校验）。
	Name string

	// Mode 交互模式。
	Mode Mode

	// Description 角色职责描述。
	Description string

	// InstructionFile 角色提示词文件名（相对 Dir）。
	InstructionFile string

	// Model 角色主模型：llm.providers 中的端点名；空值继承系统 Default。
	Model string

	// ReasoningEffort 当前模型的推理强度：auto/low/medium/high。
	ReasoningEffort string

	// ReasoningVisibility 服务商推理内容的展示策略：auto/show/hide。
	ReasoningVisibility string

	// Tools 工具范围配置。
	Tools ToolsConfig

	// Limits 运行限额。
	Limits Limits

	// Interrupt 审批配置。
	Interrupt InterruptConfig

	// Memory 记忆能力开关。
	Memory MemoryConfig

	// Skills 角色级 Skill 白名单。
	Skills SkillsConfig

	// SubAgent 作为委派目标（agent-as-tool）的对外呈现（零值 = 不启用）。
	SubAgent SubAgentConfig

	// Instruction AGENT.md 正文（系统提示 S1 的主体）。
	Instruction string

	// SafetyDoc SAFETY.md 正文（可选，空表示该角色无独立安全总则文件）。
	SafetyDoc string

	// Dir profile 目录的绝对或相对路径（agentsmd 瞬态注入定位文件用）。
	Dir string
}

// InstructionPath 返回角色提示词文件的完整路径（agentsmd 注入用）。
func (p *Profile) InstructionPath() string {
	return filepath.Join(p.Dir, p.InstructionFile)
}

// SummarizeThreshold 返回历史压缩的触发阈值：预算的 60%（10 §5 默认策略）。
func (p *Profile) SummarizeThreshold() int {
	return p.Limits.ContextTokens * 6 / 10
}

// roleYAML 是 role.yaml 的原始反序列化结构（字段零值 = 未声明，用于默认值回填）。
type roleYAML struct {
	Name                string `yaml:"name"`
	Mode                string `yaml:"mode"`
	Description         string `yaml:"description"`
	InstructionFile     string `yaml:"instruction_file"`
	Model               string `yaml:"model"`
	ReasoningEffort     string `yaml:"reasoning_effort"`
	ReasoningVisibility string `yaml:"reasoning_visibility"`
	Tools               struct {
		Namespaces []string `yaml:"namespaces"`
		Pinned     []string `yaml:"pinned"`
		ToolSearch bool     `yaml:"tool_search"`
	} `yaml:"tools"`
	Limits struct {
		MaxTurns      int `yaml:"max_turns"`
		ContextTokens int `yaml:"context_tokens"`
	} `yaml:"limits"`
	Interrupt struct {
		ApprovalRequired []string `yaml:"approval_required"`
	} `yaml:"interrupt"`
	Memory struct {
		LongTerm bool `yaml:"long_term"`
	} `yaml:"memory"`
	Skills struct {
		Allowlist []string `yaml:"allowlist"`
	} `yaml:"skills"`
	SubAgent struct {
		Enabled         bool   `yaml:"enabled"`
		ToolName        string `yaml:"tool_name"`
		ToolDescription string `yaml:"tool_description"`
		MaxTurns        int    `yaml:"max_turns"`
	} `yaml:"subagent"`
}

// Loader 是角色 profile 加载器：从根目录加载并缓存各角色。
// 为什么缓存：profile 在进程运行期不变（变更需重启生效），
// 缓存避免每个会话/每次构建重复读盘解析。
type Loader struct {
	// dir 角色 profile 根目录（如 configs/agents）。
	dir string

	// mu 保护 cache。
	mu sync.Mutex

	// cache 按角色名缓存已加载的 profile。
	cache map[string]*Profile
}

// NewLoader 创建角色 profile 加载器；dir 为角色根目录（configs/agents）。
func NewLoader(dir string) *Loader {
	return &Loader{dir: dir, cache: make(map[string]*Profile)}
}

// Load 加载指定角色的 profile：读取 role.yaml（校验 + 默认值回填）、
// AGENT.md（正文作为系统提示主体）与可选的 SAFETY.md。
// role.yaml 或 AGENT.md 缺失即报错——角色目录不完整不应静默降级。
func (l *Loader) Load(name string) (*Profile, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.cache[name]; ok {
		return p, nil
	}

	dir := filepath.Join(l.dir, name)
	raw, err := readRoleYAML(dir)
	if err != nil {
		return nil, err
	}
	p, err := buildProfile(name, dir, raw)
	if err != nil {
		return nil, err
	}
	if err := p.loadDocs(); err != nil {
		return nil, err
	}

	l.cache[name] = p
	return p, nil
}

// readRoleYAML 读取并解析角色目录下的 role.yaml。
func readRoleYAML(dir string) (*roleYAML, error) {
	path := filepath.Join(dir, "role.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取角色配置 %s 失败: %w", path, err)
	}
	var raw roleYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析角色配置 %s 失败: %w", path, err)
	}
	return &raw, nil
}

// buildProfile 把 role.yaml 原始结构映射为 Profile 并回填默认值、做必填校验。
func buildProfile(name, dir string, raw *roleYAML) (*Profile, error) {
	if raw.Name == "" {
		return nil, fmt.Errorf("角色 %q 的 role.yaml 缺少 name", name)
	}
	if raw.Name != name {
		return nil, fmt.Errorf("角色目录 %q 与 role.yaml 的 name %q 不一致", name, raw.Name)
	}
	// mode 必填：它决定运行时的装配方式（04 §3），缺省静默当协调者
	// 会让 service 角色的配置笔误被装配成错误形态，不如直接拒绝。
	if raw.Mode == "" {
		return nil, fmt.Errorf("角色 %q 的 role.yaml 缺少 mode（必填：coordinator/worker/service）", name)
	}
	if Mode(raw.Mode) == Mode("observer") {
		return nil, fmt.Errorf(
			"角色 %q 使用 v0.2 已移除的 mode %q（配置：%s）；请从 Team members 删除该角色引用，不要把 mode 改成其他值",
			name, raw.Mode, filepath.Join(dir, "role.yaml"),
		)
	}
	if _, ok := validModes[Mode(raw.Mode)]; !ok {
		return nil, fmt.Errorf("角色 %q 的 mode %q 非法：仅支持 coordinator/worker/service", name, raw.Mode)
	}
	if err := validateSubAgent(name, raw); err != nil {
		return nil, err
	}

	p := &Profile{
		Name:                raw.Name,
		Mode:                Mode(raw.Mode),
		Description:         raw.Description,
		InstructionFile:     raw.InstructionFile,
		Model:               raw.Model,
		ReasoningEffort:     raw.ReasoningEffort,
		ReasoningVisibility: raw.ReasoningVisibility,
		Tools: ToolsConfig{
			Namespaces: raw.Tools.Namespaces,
			Pinned:     raw.Tools.Pinned,
			ToolSearch: raw.Tools.ToolSearch,
		},
		Limits: Limits{
			MaxTurns:      raw.Limits.MaxTurns,
			ContextTokens: raw.Limits.ContextTokens,
		},
		Interrupt: InterruptConfig{ApprovalRequired: raw.Interrupt.ApprovalRequired},
		Memory:    MemoryConfig{LongTerm: raw.Memory.LongTerm},
		Skills:    SkillsConfig{Allowlist: normalizedStrings(raw.Skills.Allowlist)},
		SubAgent: SubAgentConfig{
			Enabled:         raw.SubAgent.Enabled,
			ToolName:        raw.SubAgent.ToolName,
			ToolDescription: raw.SubAgent.ToolDescription,
			MaxTurns:        raw.SubAgent.MaxTurns,
		},
		Dir: dir,
	}

	if p.InstructionFile == "" {
		p.InstructionFile = defaultInstructionFile
	}
	if p.Limits.MaxTurns <= 0 {
		p.Limits.MaxTurns = defaultMaxTurns
	}
	if p.Limits.ContextTokens <= 0 {
		p.Limits.ContextTokens = defaultContextTokens
	}
	if p.ReasoningEffort == "" {
		p.ReasoningEffort = "auto"
	}
	if p.ReasoningEffort != "auto" && p.ReasoningEffort != "low" &&
		p.ReasoningEffort != "medium" && p.ReasoningEffort != "high" {
		return nil, fmt.Errorf("角色 %q 的 reasoning_effort %q 非法：仅支持 auto/low/medium/high", name, p.ReasoningEffort)
	}
	if p.ReasoningVisibility == "" {
		p.ReasoningVisibility = "auto"
	}
	if p.ReasoningVisibility != "auto" && p.ReasoningVisibility != "show" &&
		p.ReasoningVisibility != "hide" {
		return nil, fmt.Errorf("角色 %q 的 reasoning_visibility %q 非法：仅支持 auto/show/hide", name, p.ReasoningVisibility)
	}
	return p, nil
}

// normalizedStrings 对 Profile 中的名称列表去空、去重并保持首次出现顺序，
// 避免重复 Skill 扩大提示词，同时保留配置文件中便于阅读的人工排序。
func normalizedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// UpdateModels 原子更新角色 role.yaml 的模型相关字段并清除缓存。
func (l *Loader) UpdateModels(name, model, reasoningEffort, reasoningVisibility string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.dir, name, "role.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取角色配置 %s 失败: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("解析角色配置 %s 失败: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("角色配置 %s 根节点不是映射", path)
	}
	root := doc.Content[0]
	if model == "" {
		removeYAMLValue(root, "model")
	} else {
		setYAMLValue(root, "model", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: model})
	}
	removeYAMLValue(root, "depth_models")
	setYAMLValue(root, "reasoning_effort", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: reasoningEffort})
	setYAMLValue(root, "reasoning_visibility", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: reasoningVisibility})
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("编码角色配置失败: %w", err)
	}
	_ = enc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".role-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err = tmp.Write(buf.Bytes()); err == nil {
		err = tmp.Chmod(0o644)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换角色配置失败: %w", err)
	}
	delete(l.cache, name)
	return nil
}

// removeYAMLValue 删除已经废弃的配置字段。更新旧安装副本时主动清理该键，
// 避免用户误以为运行时仍会消费 Depth Model 配置。
func removeYAMLValue(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
		return
	}
}

func setYAMLValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// validateSubAgent 校验 subagent 段：启用仅对 service 模式开放（委派是
// 请求-响应短调用语义，其他模式的装配形态不兼容）；tool_name/tool_description
// 必填，tool_name 必须满足模型端点的函数名约束（它直接作为 function name
// 出现在模型协议里，非法字符会被端点 400 拒绝）。
func validateSubAgent(name string, raw *roleYAML) error {
	if !raw.SubAgent.Enabled {
		return nil
	}
	if Mode(raw.Mode) != ModeService {
		return fmt.Errorf("角色 %q 的 mode %q 不允许启用 subagent（仅 service 模式可作为委派目标）", name, raw.Mode)
	}
	if !validToolName(raw.SubAgent.ToolName) {
		return fmt.Errorf("角色 %q 的 subagent.tool_name %q 非法：必填且仅含 [a-zA-Z0-9_-]", name, raw.SubAgent.ToolName)
	}
	if raw.SubAgent.ToolDescription == "" {
		return fmt.Errorf("角色 %q 启用 subagent 时缺少 tool_description（leader 模型判断委派时机的依据）", name)
	}
	return nil
}

// validToolName 报告工具名是否满足模型端点的函数名约束 ^[a-zA-Z0-9_-]+$。
func validToolName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// loadDocs 读取 AGENT.md（必须）与 SAFETY.md（可选）正文。
func (p *Profile) loadDocs() error {
	instruction, err := os.ReadFile(p.InstructionPath())
	if err != nil {
		return fmt.Errorf("读取角色提示词 %s 失败: %w", p.InstructionPath(), err)
	}
	p.Instruction = string(instruction)

	safety, err := os.ReadFile(filepath.Join(p.Dir, safetyFile))
	switch {
	case err == nil:
		p.SafetyDoc = string(safety)
	case errors.Is(err, fs.ErrNotExist):
		// SAFETY.md 可选：缺失时 SafetyDoc 为空，不视为错误。
	default:
		return fmt.Errorf("读取安全总则 %s 失败: %w", filepath.Join(p.Dir, safetyFile), err)
	}
	return nil
}
