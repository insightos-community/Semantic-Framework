package pilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

// RobotDeployment 是 Pilot、七类 Ability 与 Robot SDK 共用的一份部署配置。
// Ability 的 CR 只引用这份文件，不再逐个保存 Robot endpoint。
type RobotDeployment struct {
	APIVersion int `yaml:"api_version"`
	Robot      struct {
		ID          string `yaml:"id"`
		DisplayName string `yaml:"display_name"`
		Model       string `yaml:"model"`
		Backend     string `yaml:"backend"`
		SDK         struct {
			Package         string            `yaml:"package"`
			Endpoint        string            `yaml:"endpoint"`
			BackendProfile  string            `yaml:"backend_profile"`
			FirmwareProfile string            `yaml:"firmware_profile"`
			SceneInstanceID string            `yaml:"scene_instance_id"`
			Providers       map[string]string `yaml:"providers"`
			Options         map[string]any    `yaml:"options"`
		} `yaml:"sdk"`
		Frames     map[string]any   `yaml:"frames"`
		Tools      []map[string]any `yaml:"tools"`
		Kinematics map[string]any   `yaml:"kinematics"`
		Safety     map[string]any   `yaml:"safety"`
	} `yaml:"robot"`
	AbilityFramework struct {
		Endpoint          string `yaml:"endpoint"`
		ManagedByInstance bool   `yaml:"managed_by_instance"`
	} `yaml:"ability_framework"`
	Abilities   map[string]AbilityDeployment `yaml:"abilities"`
	RobotSkills []RobotSkillDeployment       `yaml:"robot_skills"`
	Pilot       struct {
		ID                      string `yaml:"id"`
		RobotSkillDirectory     string `yaml:"robot_skill_directory"`
		WorkerTimeoutSeconds    int    `yaml:"worker_timeout_seconds"`
		HeartbeatIntervalSecond int    `yaml:"heartbeat_interval_seconds"`
		AllowAbilityDebug       bool   `yaml:"allow_ability_debug"`
	} `yaml:"pilot"`
}

// RobotSkillDeployment 是 Robot 启动后的期望状态，不包含 Skill 源码。Server
// 根据 Registry 下发精确版本，Pilot 只上报实际安装结果。
type RobotSkillDeployment struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Enabled bool   `yaml:"enabled"`
}

func (d RobotDeployment) DesiredSkills() []store.RobotDesiredSkill {
	result := make([]store.RobotDesiredSkill, 0, len(d.RobotSkills))
	for _, item := range d.RobotSkills {
		if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.Version) == "" {
			continue
		}
		result = append(result, store.RobotDesiredSkill{RobotID: d.Robot.ID, Name: item.Name, Version: item.Version, Enabled: item.Enabled})
	}
	return result
}

// AbilityDeployment 可按能力名称选择拆分能力，并可进一步固定 instance UUID。
// 未指定名称时沿用语义角色选择；同一选择条件下有多个健康实例时不自动绑定。
type AbilityDeployment struct {
	AbilityName string `yaml:"ability_name"`
	InstanceID  string `yaml:"instance_id"`
}

func LoadRobotDeployment(path string) (RobotDeployment, error) {
	var result RobotDeployment
	content, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err := yaml.Unmarshal(content, &result); err != nil {
		return result, fmt.Errorf("解析 RobotDeployment 失败: %w", err)
	}
	if result.APIVersion != 1 {
		return result, fmt.Errorf("不支持 RobotDeployment api_version %d", result.APIVersion)
	}
	if strings.TrimSpace(result.Robot.ID) == "" || strings.TrimSpace(result.Robot.Model) == "" {
		return result, errors.New("RobotDeployment robot.id 和 robot.model 必填")
	}
	if _, err := url.ParseRequestURI(result.AbilityFramework.Endpoint); err != nil {
		return result, fmt.Errorf("RobotDeployment ability_framework.endpoint 无效: %w", err)
	}
	// 同一主机启动多台仿真 Robot 时，每个实例可以覆盖自己的 SDK Endpoint。
	// 覆盖只改变进程内的有效配置，不要求逐个修改七份 Ability CR。
	if endpoint := strings.TrimSpace(os.Getenv("SEMANTIC_ROBOT_SDK_ENDPOINT")); endpoint != "" {
		result.Robot.SDK.Endpoint = endpoint
	}
	return result, nil
}

// DeviceConfiguration 只返回设备工作台需要的部署事实。Robot SDK 的状态文件、
// 模型路径等进程内路径不会下发给浏览器；连接 Endpoint、Provider、坐标系与
// 安全上限保留，便于判断当前 Robot 实际使用了哪套后端配置。
func (d RobotDeployment) DeviceConfiguration() map[string]any {
	return map[string]any{
		"sdk": map[string]any{
			"package": d.Robot.SDK.Package, "endpoint": d.Robot.SDK.Endpoint,
			"backend_profile":   d.Robot.SDK.BackendProfile,
			"firmware_profile":  d.Robot.SDK.FirmwareProfile,
			"scene_instance_id": d.Robot.SDK.SceneInstanceID,
			"providers":         d.Robot.SDK.Providers, "options": d.Robot.SDK.Options,
		},
		"frames": d.Robot.Frames, "tools": d.Robot.Tools,
		"kinematics": d.Robot.Kinematics, "safety": d.Robot.Safety,
		"ability_framework": map[string]any{"managed_by_instance": d.AbilityFramework.ManagedByInstance},
		"pilot":             map[string]any{"allow_ability_debug": d.Pilot.AllowAbilityDebug},
	}
}

type abilityActionSpec struct {
	ActionType    string
	TaskName      string
	SchemaVersion int
	Physical      bool
}

type abilityHeartbeat struct {
	ID           string `json:"id"`
	InstanceName string `json:"instanceName"`
	AbilityName  string `json:"abilityName"`
	Version      string `json:"version"`
	State        string `json:"state"`
}

type abilityManifest struct {
	Schema struct {
		Config struct {
			OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
		} `json:"config"`
	} `json:"schema"`
	Tasks []struct {
		TaskType      int              `json:"taskType"`
		TaskName      string           `json:"taskName"`
		AbilityRole   string           `json:"abilityRole"`
		ActionType    string           `json:"actionType"`
		SchemaVersion int              `json:"schemaVersion"`
		Physical      bool             `json:"physical"`
		InputModel    string           `json:"inputModel"`
		InputFields   []map[string]any `json:"inputFields"`
		Returns       []map[string]any `json:"returns"`
	} `json:"tasks"`
}

type discoveredAbility struct {
	heartbeat abilityHeartbeat
	role      string
	tasks     map[string]int
	actions   []abilityActionSpec
	manifest  abilityManifest
	errorText string
	selected  bool
}

// AbilityDiscoverySnapshot 是 heartbeat 上报和设备页使用的稳定视图。
type AbilityDiscoverySnapshot struct {
	Status    string
	Revision  int64
	Abilities []map[string]any
	LastError string
}

// AbilityFrameworkDiscovery 主动读取 heartbeat 与对应 CR，把实例 Task 精确
// 写入 AbilityFrameworkClient，并生成 Robot Action→instance UUID 的绑定。
type AbilityFrameworkDiscovery struct {
	Endpoint   string
	RobotID    string
	Desired    map[string]AbilityDeployment
	HTTPClient *http.Client
	Interval   time.Duration
	Client     *AbilityFrameworkClient
	Catalog    *Catalog
	Logger     *log.Logger

	mu       sync.RWMutex
	snapshot AbilityDiscoverySnapshot
}

func NewAbilityFrameworkDiscovery(deployment RobotDeployment, client *AbilityFrameworkClient, catalog *Catalog, logger *log.Logger) *AbilityFrameworkDiscovery {
	return &AbilityFrameworkDiscovery{
		Endpoint: strings.TrimRight(deployment.AbilityFramework.Endpoint, "/"), RobotID: deployment.Robot.ID,
		Desired: deployment.Abilities, HTTPClient: &http.Client{Timeout: 5 * time.Second}, Interval: 2 * time.Second,
		Client: client, Catalog: catalog, Logger: logger,
		snapshot: AbilityDiscoverySnapshot{Status: "starting"},
	}
}

func (d *AbilityFrameworkDiscovery) Run(ctx context.Context) {
	d.refreshAndRecord(ctx)
	interval := d.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshAndRecord(ctx)
		}
	}
}

func (d *AbilityFrameworkDiscovery) refreshAndRecord(ctx context.Context) {
	if err := d.Refresh(ctx); err != nil {
		d.mu.Lock()
		if d.snapshot.Status != "offline" || d.snapshot.LastError != err.Error() {
			d.snapshot.Revision++
		}
		d.snapshot.Status = "offline"
		d.snapshot.LastError = err.Error()
		d.mu.Unlock()
		if d.Logger != nil {
			d.Logger.WithError(err).Warn("AbilityFramework 状态发现失败")
		}
	}
}

func (d *AbilityFrameworkDiscovery) Snapshot() AbilityDiscoverySnapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return AbilityDiscoverySnapshot{Status: d.snapshot.Status, Revision: d.snapshot.Revision,
		Abilities: cloneMaps(d.snapshot.Abilities), LastError: d.snapshot.LastError}
}

func (d *AbilityFrameworkDiscovery) Refresh(ctx context.Context) error {
	var heartbeats []abilityHeartbeat
	if err := d.getJSON(ctx, "/api/ability-heartbeat", &heartbeats); err != nil {
		return err
	}
	items := make([]discoveredAbility, 0, len(heartbeats))
	taskCatalogs := make([]AbilityTaskCatalog, 0, len(heartbeats))
	for _, heartbeat := range heartbeats {
		item := discoveredAbility{heartbeat: heartbeat}
		var manifest abilityManifest
		if heartbeat.ID == "" {
			item.errorText = "heartbeat 缺少 instance UUID"
		} else if heartbeat.AbilityName == "" || heartbeat.Version == "" {
			item.errorText = "heartbeat 缺少 Ability 名称或版本"
		} else if err := d.getJSON(ctx, "/api/manifest/"+url.PathEscape(heartbeat.AbilityName)+"/"+url.PathEscape(heartbeat.Version), &manifest); err != nil {
			item.errorText = err.Error()
		} else {
			item.manifest = manifest
			// 新版 AbilityFramework 把 Task 定义保存在包的 Manifest 中，实例
			// CR 只保存部署配置。Pilot 必须按 heartbeat 中的精确 Ability
			// 名称和版本读取 Manifest，不能再从 CR 猜测 Task 编号。
			item.tasks = make(map[string]int, len(manifest.Tasks))
			actionTypes := make(map[string]struct{})
			for _, task := range manifest.Tasks {
				if task.TaskName != "" {
					item.tasks[task.TaskName] = task.TaskType
				}
				if strings.TrimSpace(task.ActionType) == "" {
					continue
				}
				if strings.TrimSpace(task.AbilityRole) == "" || task.SchemaVersion <= 0 {
					item.errorText = fmt.Sprintf("Task %s 的语义 Action 元数据不完整", task.TaskName)
					continue
				}
				if item.role != "" && item.role != task.AbilityRole {
					item.errorText = "同一 Ability Manifest 不能声明多个语义角色"
					continue
				}
				if _, duplicated := actionTypes[task.ActionType]; duplicated {
					item.errorText = fmt.Sprintf("Ability Manifest 重复声明 Action %s", task.ActionType)
					continue
				}
				item.role = task.AbilityRole
				actionTypes[task.ActionType] = struct{}{}
				item.actions = append(item.actions, abilityActionSpec{
					ActionType: task.ActionType, TaskName: task.TaskName,
					SchemaVersion: task.SchemaVersion, Physical: task.Physical,
				})
			}
			if len(item.tasks) == 0 {
				item.errorText = "Ability Manifest 未声明 Task"
			} else if len(item.actions) == 0 && item.errorText == "" {
				item.errorText = "Ability Manifest 未声明可路由的语义 Action"
			} else {
				taskCatalogs = append(taskCatalogs, AbilityTaskCatalog{InstanceID: heartbeat.ID, TaskTypes: item.tasks})
			}
		}
		items = append(items, item)
	}

	profile := RobotProfile{RobotID: d.RobotID, Bindings: make(map[string]AbilityBinding)}
	readyRoles := 0
	ambiguousActions := make(map[string]bool)
	roles := make([]string, 0, len(d.Desired))
	for role := range d.Desired {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		selected := selectAbilityInstance(items, role, d.Desired[role])
		if selected < 0 {
			continue
		}
		items[selected].selected = true
		complete := items[selected].errorText == "" && hasTask(items[selected].tasks, "GetExecution") && hasTask(items[selected].tasks, "StopExecution")
		for _, action := range items[selected].actions {
			if !hasTask(items[selected].tasks, action.TaskName) {
				complete = false
				continue
			}
			binding := AbilityBinding{Action: ActionRef{Type: action.ActionType, SchemaVersion: action.SchemaVersion},
				AbilityName: items[selected].heartbeat.AbilityName, TaskName: action.TaskName,
				InstanceID: items[selected].heartbeat.ID, Physical: action.Physical}
			key := binding.Action.Key()
			if previous, exists := profile.Bindings[key]; exists && previous.InstanceID != binding.InstanceID {
				ambiguousActions[key] = true
				delete(profile.Bindings, key)
			}
			if ambiguousActions[key] {
				complete = false
				continue
			}
			profile.Bindings[key] = binding
		}
		if complete {
			readyRoles++
		}
	}

	views := abilityViews(items)
	status := "degraded"
	if len(roles) > 0 && readyRoles == len(roles) {
		status = "ready"
	}
	d.Client.ReplaceCatalogs(taskCatalogs)
	d.Catalog.Replace(profile)
	d.mu.Lock()
	if !reflect.DeepEqual(d.snapshot.Abilities, views) || d.snapshot.Status != status {
		d.snapshot.Revision++
	}
	d.snapshot.Status = status
	d.snapshot.Abilities = views
	d.snapshot.LastError = ""
	d.mu.Unlock()
	return nil
}

func selectAbilityInstance(items []discoveredAbility, role string, desired AbilityDeployment) int {
	selected := -1
	for index := range items {
		item := items[index]
		matches := item.role == role
		if desired.AbilityName != "" {
			matches = item.heartbeat.AbilityName == desired.AbilityName
		}
		if !matches || !abilityStateReady(item.heartbeat.State) || item.errorText != "" {
			continue
		}
		if desired.InstanceID != "" {
			if item.heartbeat.ID == desired.InstanceID {
				return index
			}
			continue
		}
		if selected >= 0 {
			// 未显式配置且同角色存在多个健康实例时，不按顺序选第一个。
			return -1
		}
		selected = index
	}
	return selected
}

func abilityStateReady(state string) bool {
	switch strings.ToLower(state) {
	case "standby", "running":
		return true
	default:
		return false
	}
}

func hasTask(tasks map[string]int, name string) bool {
	_, ok := tasks[name]
	return ok
}

func abilityViews(items []discoveredAbility) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tasks := make([]string, 0, len(item.tasks))
		debugTasks := make([]map[string]any, 0, len(item.tasks))
		manifestTasks := make(map[string]struct {
			InputModel  string
			InputFields []map[string]any
			Returns     []map[string]any
		}, len(item.manifest.Tasks))
		for _, task := range item.manifest.Tasks {
			manifestTasks[task.TaskName] = struct {
				InputModel  string
				InputFields []map[string]any
				Returns     []map[string]any
			}{InputModel: task.InputModel, InputFields: task.InputFields, Returns: task.Returns}
		}
		for task := range item.tasks {
			tasks = append(tasks, task)
			// GetExecution/StopExecution 是 AbilityFramework 的执行管理入口，
			// 不是用户要调试的业务 Task。设备页只展示可直接启动的业务项，
			// 输入暂时使用 JSON 对象；后续 Manifest 提供 schema 时再渲染字段表单。
			if task != "GetExecution" && task != "StopExecution" {
				metadata := manifestTasks[task]
				debugTasks = append(debugTasks, map[string]any{
					"name": task, "task_type": item.tasks[task],
					"input_model": metadata.InputModel, "input_fields": metadata.InputFields,
					"returns": metadata.Returns,
				})
			}
		}
		sort.Strings(tasks)
		sort.Slice(debugTasks, func(i, j int) bool {
			return stringValue(debugTasks[i]["name"]) < stringValue(debugTasks[j]["name"])
		})
		actions := make([]string, 0, len(item.actions))
		actionDetails := make([]map[string]any, 0, len(item.actions))
		for _, action := range item.actions {
			metadata := manifestTasks[action.TaskName]
			actions = append(actions, action.ActionType)
			actionDetails = append(actionDetails, map[string]any{
				"type": action.ActionType, "task_name": action.TaskName,
				"schema_version": action.SchemaVersion, "physical": action.Physical,
				"input_model": metadata.InputModel, "input_fields": metadata.InputFields,
				"returns": metadata.Returns,
			})
		}
		sort.Strings(actions)
		sort.Slice(actionDetails, func(i, j int) bool {
			return stringValue(actionDetails[i]["type"]) < stringValue(actionDetails[j]["type"])
		})
		healthy := abilityStateReady(item.heartbeat.State) && item.errorText == ""
		status := strings.ToLower(item.heartbeat.State)
		if healthy {
			status = "ready"
		}
		view := map[string]any{"instance_id": item.heartbeat.ID, "instance_name": item.heartbeat.InstanceName,
			"ability_name": item.heartbeat.AbilityName, "role": item.role, "version": item.heartbeat.Version,
			"state": item.heartbeat.State, "status": status, "health": map[bool]string{true: "healthy", false: "degraded"}[healthy],
			"healthy": healthy, "selected": item.selected, "tasks": tasks,
			"actions": actions, "action_details": actionDetails, "debug_tasks": debugTasks,
			"config_schema": item.manifest.Schema.Config.OpenAPIV3Schema}
		if item.errorText != "" {
			view["error"] = item.errorText
		}
		result = append(result, view)
	}
	sort.Slice(result, func(i, j int) bool {
		return stringValue(result[i]["instance_id"]) < stringValue(result[j]["instance_id"])
	})
	return result
}

func (d *AbilityFrameworkDiscovery) getJSON(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Endpoint+path, nil)
	if err != nil {
		return err
	}
	response, err := d.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("AbilityFramework GET %s HTTP %d: %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("解析 AbilityFramework GET %s 失败: %w", path, err)
	}
	return nil
}
