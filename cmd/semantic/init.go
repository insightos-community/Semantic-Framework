package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	templatefs "insightos.cn/semantic-framework/configs"
	"insightos.cn/semantic-framework/pkg/config"
)

// initConfigHeader 标记配置文件来自安装副本，提醒维护者和用户不要把它
// 误当作仓库模板；后续机器重写时 settings API 会换成自己的文件头。
const initConfigHeader = "# 本文件由 semantic init 从内置模板生成；前端设置与热重载只修改本安装副本。\n"

// legacyDefaultTeamV014 是 v0.1.4-dev 曾安装的默认 Team。v0.2 删除了
// observer 运行模式和长驻模型 Monitor；只在安装副本与这份旧模板的 YAML
// 语义完全一致时自动升级，避免覆盖用户自行维护的 Team。
const legacyDefaultTeamV014 = `
name: default
leader:
  role: leader
members:
  - id: monitor-1
    role: monitor
  - id: query-1
    role: query
blackboard:
  retention_days: 7
`

// legacyDefaultTeamV020 与 legacyLeaderRoleV020 是 v0.2 安装副本中的
// 内置默认配置。只有 YAML 语义精确一致时才自动补齐 v0.3 的 Developer
// Worker 与 plan.suggest；用户增删过任一字段都视为自定义配置并完整保留。
const legacyDefaultTeamV020 = `
name: default
leader:
  role: leader
members:
  - id: query-1
    role: query
`

// legacyDefaultTeamV030 是 v0.3 安装副本中的内置默认 Team。它已经包含
// Query 与 Developer，但早于 Map/Monitor Worker 与动态 Robot 身份。只有
// YAML 语义精确一致时才自动升级；用户改过成员、顺序或其他字段时保持原样。
const legacyDefaultTeamV030 = `
name: default
leader:
  role: leader
members:
  - id: query-1
    role: query
  - id: developer-1
    role: developer
`

const legacyLeaderRoleV020 = `
name: leader
mode: coordinator
description: 团队指挥官：接收用户目标、预规划、分派执行并如实汇报结果
instruction_file: AGENT.md
tools:
  namespaces: [system.*, artifact.*, execute, execute_host]
  pinned: [system.time]
  tool_search: true
limits:
  max_turns: 10
  context_tokens: 120000
interrupt:
  approval_required: [artifact.put]
memory:
  long_term: false
skills:
  allowlist: [artifact-usage, echo-guide, data-profile, semantic-diagnostics]
`

// runInit 安装一份独立的运行配置和 Agent/Skill 初始资源。仓库 configs
// 只作为编译进 CLI 的只读模板，运行期永远写 target 指向的安装副本。
func runInit(args []string) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	targetFlag := flags.String("c", "", "安装配置文件路径（默认 ~/.semantic/configs/semantic-server.yaml）")
	force := flags.Bool("force", false, "覆盖已有配置文件（Agent/Skill 文件仍不覆盖）")
	resetData := flags.Bool("reset-data", false, "备份并重建数据库、Artifact 与工作区数据")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	target, err := config.ResolvePath(*targetFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析安装路径失败:", err)
		return 1
	}
	if *resetData {
		backup, resetErr := resetRuntimeData(target, time.Now().UTC())
		if resetErr != nil {
			fmt.Fprintln(os.Stderr, "重置数据失败:", resetErr)
			return 1
		}
		if backup != "" {
			fmt.Printf("旧数据已备份：%s\n", backup)
		}
	}
	created, skipped, err := installRuntime(target, *force)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败:", err)
		return 1
	}
	if created {
		fmt.Printf("初始化完成：%s\n", target)
	} else {
		fmt.Printf("主配置已存在，保持不变：%s\n", target)
	}
	if skipped > 0 {
		fmt.Printf("保留了 %d 个已有 Agent/Skill 文件\n", skipped)
	}
	return 0
}

// runtimeRoot 返回安装配置对应的根目录。默认布局是 root/configs/*.yaml，
// 自定义配置直接放在某目录时则以该目录作为根。
func runtimeRoot(target string) string {
	configDir := filepath.Dir(target)
	if filepath.Base(configDir) == "configs" {
		return filepath.Dir(configDir)
	}
	return configDir
}

// resetRuntimeData 将整个 data 目录原子移动到同一安装根的 backups 目录。
// data 中包含 SQLite、WAL/SHM、Artifact 和 Project workspace，整体移动可以
// 避免只备份数据库却留下不匹配文件。不存在数据时只创建空目录。
func resetRuntimeData(target string, now time.Time) (string, error) {
	root := runtimeRoot(target)
	dataDir := filepath.Join(root, "data")
	if _, err := os.Stat(dataDir); errors.Is(err, os.ErrNotExist) {
		return "", os.MkdirAll(dataDir, 0o700)
	} else if err != nil {
		return "", fmt.Errorf("检查数据目录失败: %w", err)
	}
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("创建备份目录失败: %w", err)
	}
	backup := filepath.Join(backupDir, "data-"+now.UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(dataDir, backup); err != nil {
		return "", fmt.Errorf("备份数据目录失败: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("重建数据目录失败（旧数据位于 %s）: %w", backup, err)
	}
	return backup, nil
}

// installRuntime 把内置模板安装到 target 所在目录。默认不覆盖任何用户文件；
// force 只允许重建主配置，Agent/Skill 仍逐文件保留。返回 created 表示本次
// 是否写入主配置，skipped 表示保留了多少个已有资源文件。
func installRuntime(target string, force bool) (created bool, skipped int, err error) {
	configDir := filepath.Dir(target)
	root := runtimeRoot(target)
	dataDir := filepath.Join(root, "data")
	runtimeDir := filepath.Join(root, "runtimes.d")
	contentDir := filepath.Join(root, "content")
	sceneCatalogDir := filepath.Join(contentDir, "scene-catalogs")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return false, 0, fmt.Errorf("创建配置目录失败: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return false, 0, fmt.Errorf("创建数据目录失败: %w", err)
	}

	for _, name := range []string{"agents", "skills"} {
		kept, copyErr := copyTemplateTree(name, filepath.Join(configDir, name))
		if copyErr != nil {
			return false, skipped, copyErr
		}
		skipped += kept
	}
	// Runtime 与场景目录由 `semantic runtime install` 从正式 Pack 建立。
	// init 不再复制需要源码路径/环境变量的开发清单，干净安装因此不会出现
	// “看似已安装、实际无法启动”的 Runtime。
	for label, path := range map[string]string{
		"runtimes.d": runtimeDir, "content": contentDir, "scene catalog": sceneCatalogDir,
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return false, skipped, fmt.Errorf("创建 %s 失败: %w", label, err)
		}
	}
	if err := migrateLegacyAgentResources(configDir); err != nil {
		return false, skipped, err
	}

	_, statErr := os.Stat(target)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, skipped, fmt.Errorf("检查配置文件失败: %w", statErr)
	}
	if exists && !force {
		// v0.4 之前生成的安装配置没有 simulation 段。配置加载器会为缺失
		// 字段填入仓库相对路径 configs/runtimes.d，导致从 worktree 启动时
		// 误读源码模板，甚至与本机安装清单产生重复 ID。这里只补齐确实缺失
		// 或为空的字段；管理员已经配置的目录必须原样保留。
		migrated, migrateErr := migrateInstalledSimulationPaths(
			target, runtimeDir, sceneCatalogDir,
		)
		return migrated, skipped, migrateErr
	}

	template, err := templatefs.Templates.ReadFile("semantic-server.yaml")
	if err != nil {
		return false, skipped, fmt.Errorf("读取内置配置模板失败: %w", err)
	}
	cfg, err := config.ParseYAML(template)
	if err != nil {
		return false, skipped, fmt.Errorf("内置配置模板无效: %w", err)
	}
	cfg.Store.SQLitePath = filepath.Join(dataDir, "semantic.db")
	cfg.Agents.ProfilesDir = filepath.Join(configDir, "agents")
	cfg.Agents.TeamsDir = filepath.Join(configDir, "agents", "teams")
	cfg.Simulation.RuntimesDir = runtimeDir
	cfg.Simulation.CatalogDir = sceneCatalogDir
	cfg.Skills.Dir = filepath.Join(configDir, "skills")
	// 新安装先启动空的受管目录，导入 Robot 包后可直接创建实例；所有路径
	// 跟随安装根目录，与执行 semantic-server 时的当前工作目录无关。
	cfg.RobotRuntime.Enabled = true
	cfg.RobotRuntime.DataRoot = filepath.Dir(dataDir)
	cfg.RobotRuntime.BundlesDir = filepath.Join(filepath.Dir(dataDir), "robot-bundles")
	if err := os.MkdirAll(cfg.RobotRuntime.BundlesDir, 0750); err != nil {
		return false, skipped, err
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return false, skipped, fmt.Errorf("生成安装配置失败: %w", err)
	}
	if err := writeInitFile(target, append([]byte(initConfigHeader), body...)); err != nil {
		return false, skipped, err
	}
	return true, skipped, nil
}

// migrateInstalledSimulationPaths 为旧安装配置补充 v0.4 引入的 Runtime 和
// 场景目录。使用 yaml.Node 是为了保留未知配置段与注释；已有非空值属于用户
// 配置，不能被 semantic init 静默改写。
func migrateInstalledSimulationPaths(target, runtimeDir, sceneCatalogDir string) (bool, error) {
	data, err := os.ReadFile(target)
	if err != nil {
		return false, fmt.Errorf("读取已有配置失败: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return false, fmt.Errorf("解析已有配置失败: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return false, errors.New("已有配置顶层必须是 YAML 对象")
	}
	root := document.Content[0]
	simulationNode := mappingValue(root, "simulation")
	if simulationNode == nil {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "simulation"},
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"},
		)
		simulationNode = root.Content[len(root.Content)-1]
	} else if simulationNode.Kind != yaml.MappingNode {
		return false, errors.New("已有配置 simulation 必须是 YAML 对象")
	}

	changed := setMissingMappingScalar(simulationNode, "runtimes_dir", runtimeDir)
	changed = setMissingMappingScalar(simulationNode, "catalog_dir", sceneCatalogDir) || changed
	if !changed {
		return false, nil
	}
	body, err := yaml.Marshal(&document)
	if err != nil {
		return false, fmt.Errorf("编码迁移后配置失败: %w", err)
	}
	if err := writeInitFile(target, body); err != nil {
		return false, err
	}
	return true, nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func setMissingMappingScalar(mapping *yaml.Node, key, value string) bool {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value != key {
			continue
		}
		current := mapping.Content[index+1]
		if current.Kind != yaml.ScalarNode || strings.TrimSpace(current.Value) != "" {
			return false
		}
		current.Kind = yaml.ScalarNode
		current.Tag = "!!str"
		current.Value = value
		return true
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
	return true
}

type installedTeam struct {
	Leader struct {
		Role string `yaml:"role"`
	} `yaml:"leader"`
	Members []struct {
		ID   string `yaml:"id"`
		Role string `yaml:"role"`
	} `yaml:"members"`
}

type installedRole struct {
	Mode string `yaml:"mode"`
}

// migrateLegacyAgentResources 只升级精确匹配的历史内置模板：v0.1.4 的
// observer Team、v0.2 缺 Developer/plan.suggest 的默认配置，以及 v0.3
// 缺 Map/Monitor Worker 的默认 Team。任何自定义 Team/Profile 都保持原样；
// observer 自定义 Team 仍要求用户显式处理。
func migrateLegacyAgentResources(configDir string) error {
	teamPath := filepath.Join(configDir, "agents", "teams", "default.yaml")
	current, err := os.ReadFile(teamPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取默认 Team 配置失败: %w", err)
	}

	if yamlEquivalent(current, []byte(legacyDefaultTeamV014)) {
		next, readErr := templatefs.Templates.ReadFile("agents/teams/default.yaml")
		if readErr != nil {
			return fmt.Errorf("读取 v0.3 默认 Team 模板失败: %w", readErr)
		}
		if writeErr := writeInitFile(teamPath, next); writeErr != nil {
			return fmt.Errorf("升级旧默认 Team 配置失败: %w", writeErr)
		}
		// v0.1.4 内置 monitor 是已删除的长驻 observer；v0.5 又复用了同名
		// 角色作为按 Task 启动的 Worker。只有内容仍精确等于旧内置 profile
		// 时才替换，避免把一个合法的自定义 monitor 静默改写。
		monitorPath := filepath.Join(configDir, "agents", "monitor", "role.yaml")
		oldMonitor, monitorErr := os.ReadFile(monitorPath)
		if monitorErr == nil &&
			yamlEquivalent(oldMonitor, []byte("name: monitor\nmode: observer\n")) {
			nextMonitor, templateErr := templatefs.Templates.ReadFile("agents/monitor/role.yaml")
			if templateErr != nil {
				return fmt.Errorf("读取 Monitor Worker 模板失败: %w", templateErr)
			}
			if writeErr := writeInitFile(monitorPath, nextMonitor); writeErr != nil {
				return fmt.Errorf("升级旧 Monitor profile 失败: %w", writeErr)
			}
		}
		fmt.Printf("已将旧默认 Team 更新为 v0.3 配置：%s\n", teamPath)
	} else if yamlEquivalent(current, []byte(legacyDefaultTeamV020)) {
		next, readErr := templatefs.Templates.ReadFile("agents/teams/default.yaml")
		if readErr != nil {
			return fmt.Errorf("读取 v0.3 默认 Team 模板失败: %w", readErr)
		}
		if writeErr := writeInitFile(teamPath, next); writeErr != nil {
			return fmt.Errorf("升级 v0.2 默认 Team 配置失败: %w", writeErr)
		}
		fmt.Printf("已为 v0.2 默认 Team 增加持久 Task Worker：%s\n", teamPath)
	} else if yamlEquivalent(current, []byte(legacyDefaultTeamV030)) {
		next, readErr := templatefs.Templates.ReadFile("agents/teams/default.yaml")
		if readErr != nil {
			return fmt.Errorf("读取当前默认 Team 模板失败: %w", readErr)
		}
		if writeErr := writeInitFile(teamPath, next); writeErr != nil {
			return fmt.Errorf("升级 v0.3 默认 Team 配置失败: %w", writeErr)
		}
		fmt.Printf("已为 v0.3 默认 Team 增加 Map 与 Monitor Worker：%s\n", teamPath)
	}

	leaderPath := filepath.Join(configDir, "agents", "leader", "role.yaml")
	leader, readErr := os.ReadFile(leaderPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("读取 Leader 配置失败: %w", readErr)
	}
	if readErr == nil && yamlEquivalent(leader, []byte(legacyLeaderRoleV020)) {
		next, templateErr := templatefs.Templates.ReadFile("agents/leader/role.yaml")
		if templateErr != nil {
			return fmt.Errorf("读取 v0.3 Leader 模板失败: %w", templateErr)
		}
		if writeErr := writeInitFile(leaderPath, next); writeErr != nil {
			return fmt.Errorf("升级 v0.2 Leader 配置失败: %w", writeErr)
		}
		fmt.Printf("已为 v0.2 默认 Leader 增加 Plan 建议工具：%s\n", leaderPath)
	}

	return validateNoObserverTeamMembers(configDir)
}

func yamlEquivalent(left, right []byte) bool {
	var leftValue any
	if err := yaml.Unmarshal(left, &leftValue); err != nil {
		return false
	}
	var rightValue any
	if err := yaml.Unmarshal(right, &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

// validateNoObserverTeamMembers 在 Server 装配前指出旧配置的准确位置。
// 未被 Team 引用的旧 profile 属于安装目录中的用户资源，不自动删除。
func validateNoObserverTeamMembers(configDir string) error {
	teamsDir := filepath.Join(configDir, "agents", "teams")
	entries, err := os.ReadDir(teamsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 Team 配置目录失败: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		teamPath := filepath.Join(teamsDir, entry.Name())
		data, readErr := os.ReadFile(teamPath)
		if readErr != nil {
			return fmt.Errorf("读取 Team 配置 %s 失败: %w", teamPath, readErr)
		}
		var team installedTeam
		if parseErr := yaml.Unmarshal(data, &team); parseErr != nil {
			// init 只迁移已知 observer，不接管 Team 的完整合法性校验。
			// 用户自定义文件仍由 Server 既有装配流程给出原始错误。
			continue
		}
		roles := make([]string, 0, len(team.Members)+1)
		roles = append(roles, team.Leader.Role)
		for _, member := range team.Members {
			roles = append(roles, member.Role)
		}
		for _, roleName := range roles {
			if strings.TrimSpace(roleName) == "" {
				continue
			}
			rolePath := filepath.Join(configDir, "agents", roleName, "role.yaml")
			roleData, readErr := os.ReadFile(rolePath)
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			if readErr != nil {
				return fmt.Errorf("读取角色配置 %s 失败: %w", rolePath, readErr)
			}
			var role installedRole
			if parseErr := yaml.Unmarshal(roleData, &role); parseErr != nil {
				// 保持 --force 不覆盖、也不预先拒绝用户自定义 Profile 的行为。
				continue
			}
			if strings.TrimSpace(role.Mode) != "observer" {
				continue
			}
			return fmt.Errorf(
				"Team 配置 %s 仍引用已移除的 observer 角色（%s）；请从 members 中删除该角色引用，不要把 mode 改成其他值",
				teamPath, rolePath,
			)
		}
	}
	return nil
}

// copyTemplateTree 把 embed.FS 中的一个目录递归复制到安装目录。目录权限
// 使用 0700、文件使用 0600；目标文件已存在时计入 skipped 并保持原内容，
// 从而保证重复 init 不会覆盖用户定制的 Agent 或 Skill。
func copyTemplateTree(source, destination string) (skipped int, err error) {
	err = fs.WalkDir(templatefs.Templates, source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := strings.TrimPrefix(path, source)
		target := filepath.Join(destination, filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if _, err := os.Stat(target); err == nil {
			skipped++
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		data, err := templatefs.Templates.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	return skipped, err
}

// writeInitFile 通过“同目录临时文件 + rename”原子安装主配置，避免 CLI
// 中途退出时留下只写入一部分、下次启动又无法解析的 YAML。
func writeInitFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("安装配置文件失败: %w", err)
	}
	return nil
}
