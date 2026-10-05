// Package robotruntime 定义 Server 自动创建机器人运行实例时使用的稳定边界。
//
// 该包不知道 MuJoCo、厂商接口或具体进程命令。SimulationService 提供已经启动的
// 虚拟 Robot 描述；InstanceLauncher 只管理 Pilot、Ability 和 AbilityFramework。
// 场景 reset/stop 仍由 v0.4 SimulationService 负责。
package robotruntime

import (
	"context"
	"errors"
	"time"
)

var (
	ErrBundleNotFound    = errors.New("没有匹配的 Robot Runtime Bundle")
	ErrBundleAmbiguous   = errors.New("Robot Runtime Bundle 匹配不唯一")
	ErrInstanceNotFound  = errors.New("Robot Runtime Instance 不存在")
	ErrRobotAlreadyInUse = errors.New("Robot 已有活动 Runtime Instance")
	ErrPortUnavailable   = errors.New("AbilityFramework 端口不可用")
)

// State 是设备页和 Pilot/Server 对账共同使用的运行实例状态。
type State string

const (
	StateStarting    State = "starting"
	StateReady       State = "ready"
	StateDegraded    State = "degraded"
	StateStopping    State = "stopping"
	StateStopped     State = "stopped"
	StateFailed      State = "failed"
	StateInterrupted State = "interrupted"
)

func (s State) Active() bool {
	return s == StateStarting || s == StateReady || s == StateDegraded || s == StateStopping || s == StateInterrupted
}

// MatchKey 精确描述一个 Runtime Bundle 的适用范围。三个字段必须全部相等，
// 禁止只按 Robot 型号取第一个 Bundle。
type MatchKey struct {
	RobotModel     string `json:"robot_model" yaml:"robot_model"`
	Backend        string `json:"backend" yaml:"backend"`
	BackendProfile string `json:"backend_profile" yaml:"backend_profile"`
}

// RobotCommandType 是 Runtime 对外暴露的低层命令集合。它属于虚拟 Robot
// 描述，而不是 SimulationService 的生命周期，因此放在本包作为跨 Runtime、
// SDK 和 Server 的唯一契约。
type RobotCommandType string

const (
	RobotCommandJointTrajectory RobotCommandType = "joint_trajectory"
	RobotCommandBaseTrajectory  RobotCommandType = "base_trajectory"
	RobotCommandGripper         RobotCommandType = "gripper_command"
	RobotCommandStop            RobotCommandType = "stop"
	RobotCommandHold            RobotCommandType = "hold"
)

type RobotCapability struct {
	Commands []RobotCommandType `json:"commands"`
	Sensors  []string           `json:"sensors"`
	Frames   []string           `json:"frames"`
}

// ToolDescriptor 描述 Robot 当前装配的物理工具。左右周转箱夹具仍属于同一
// r1_pro_chassis Robot 型号；工具差异通过该字段表达，不能派生新的 Robot 型号
// 或另一套 SDK。force/travel 是 SDK 规划和 Ability 参数校验所需的物理边界。
type ToolDescriptor struct {
	ToolRef       string  `json:"tool_ref"`
	Side          string  `json:"side"`
	Kind          string  `json:"kind"`
	Frame         string  `json:"frame"`
	Joint         string  `json:"joint"`
	TravelM       float64 `json:"travel_m"`
	NormalForceN  float64 `json:"normal_force_n"`
	MaximumForceN float64 `json:"maximum_force_n"`
}

// VirtualRobotDescriptor 是场景平台交给 Server Orchestrator 的可运行 Robot
// 描述。Bundle 只能使用 MatchKey() 返回的完整三元组选择。
type VirtualRobotDescriptor struct {
	RobotID            string   `json:"robot_id"`
	SceneInstanceID    string   `json:"scene_instance_id,omitempty"`
	Model              string   `json:"model"`
	Backend            string   `json:"backend"`
	Kind               string   `json:"kind"`
	CoordinateFrame    string   `json:"coordinate_frame"`
	SDKPackage         string   `json:"sdk_package"`
	BackendProfile     string   `json:"backend_profile"`
	Endpoint           string   `json:"endpoint,omitempty"`
	URDFPath           string   `json:"urdf_path,omitempty"`
	PackageDirectories []string `json:"package_directories,omitempty"`

	JointNames    []string         `json:"joint_names"`
	EndEffectors  []string         `json:"end_effectors"`
	Grippers      []string         `json:"grippers,omitempty"`
	Tools         []ToolDescriptor `json:"tools,omitempty"`
	Capabilities  RobotCapability  `json:"capabilities"`
	Configuration map[string]any   `json:"configuration,omitempty"`
}

func (d VirtualRobotDescriptor) MatchKey() MatchKey {
	return MatchKey{RobotModel: d.Model, Backend: d.Backend, BackendProfile: d.BackendProfile}
}

// Bundle Catalog 只保存包身份、路径和匹配条件。进程路径、Wheel、SDK 与能力清单
// 启动模板由 semantic-robot-deployment 的 bundle.yaml 自己解释。
type Bundle struct {
	Name    string   `json:"name" yaml:"name"`
	Version string   `json:"version" yaml:"version"`
	Path    string   `json:"path" yaml:"path"`
	Match   MatchKey `json:"match" yaml:"match"`

	// ReadinessTimeout 来自 bundle.yaml 的 spec.runtime.readinessTimeout。
	// 冷启动要加载大模型的 Bundle（如 Franka + SmolVLA）比普通仿真 Bundle
	// 需要更长的收敛时间，所以由包自带而不是全局限定；零值表示未声明，
	// 由调用方回退到默认值。
	ReadinessTimeout time.Duration `json:"readiness_timeout,omitempty" yaml:"readiness_timeout,omitempty"`
}

// StartRequest 是上层根据 RobotDeployment 形成的启动输入。InstanceID 可省略，
// Orchestrator 会生成稳定 ID；测试和恢复流程可以显式传入。
type StartRequest struct {
	InstanceID      string
	PilotInstanceID string
	Descriptor      VirtualRobotDescriptor
	ProjectID       string
}

// RuntimeInstance 由 Server 在启动 Pilot 前持久化，并作为设备快照的一部分上报。
// Web 只消费这里的状态，不管理端口和进程。
type RuntimeInstance struct {
	InstanceID               string    `json:"instance_id"`
	PilotInstanceID          string    `json:"pilot_instance_id"`
	RobotID                  string    `json:"robot_id"`
	SceneInstanceID          string    `json:"scene_instance_id,omitempty"`
	BundleName               string    `json:"bundle_name"`
	BundleVersion            string    `json:"bundle_version"`
	RobotModel               string    `json:"robot_model"`
	Backend                  string    `json:"backend"`
	Kind                     string    `json:"kind"`
	BackendProfile           string    `json:"backend_profile"`
	Status                   State     `json:"status"`
	FailureReason            string    `json:"failure_reason,omitempty"`
	AbilityFrameworkPort     int       `json:"ability_framework_port,omitempty"`
	AbilityFrameworkEndpoint string    `json:"ability_framework_endpoint,omitempty"`
	DataDirectory            string    `json:"data_directory,omitempty"`
	ProjectID                string    `json:"project_id,omitempty"`
	Revision                 int64     `json:"revision"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

type LaunchRequest struct {
	Instance   RuntimeInstance
	Bundle     Bundle
	Descriptor VirtualRobotDescriptor
}

// LaunchResult 只有在 AbilityFramework 与所需 Ability 都通过就绪检查后返回。
// 因此 Orchestrator 进入 ready 后 Pilot 才可以向 Server 注册这台 Robot。
type LaunchResult struct {
	AbilityFrameworkEndpoint string
}

type StopEvidence struct {
	Confirmed bool           `json:"confirmed"`
	Details   map[string]any `json:"details,omitempty"`
}

// Event 的 ProjectID 非空时，Server 还会把状态发布到 Project Studio。
type Event struct {
	Type      string          `json:"type"`
	RobotID   string          `json:"robot_id"`
	ProjectID string          `json:"project_id,omitempty"`
	Instance  RuntimeInstance `json:"runtime_instance"`
}

type Store interface {
	SaveRuntimeInstance(context.Context, RuntimeInstance) error
	GetRuntimeInstance(context.Context, string) (RuntimeInstance, error)
	GetActiveRuntimeByRobot(context.Context, string) (RuntimeInstance, error)
	ListRuntimeInstances(context.Context) ([]RuntimeInstance, error)
}

type PortLeaser interface {
	AcquireAbilityFrameworkPort(context.Context, string, int, int) (int, error)
	ReleaseAbilityFrameworkPort(context.Context, string) error
}

// InstanceLauncher 启停一个 Robot 类型包实例；它内部执行 Pilot 安全停止、
// Ability 与 AbilityFramework 关闭，但不停止或 reset 场景 Runtime。
type InstanceLauncher interface {
	Start(context.Context, LaunchRequest) (LaunchResult, error)
	Stop(context.Context, RuntimeInstance, string) (StopEvidence, error)
}

// InterruptedSimulationCleaner 只用于 Framework 本机受管的仿真实例恢复。
// 正常停止以及真机停止仍必须经过 InstanceLauncher.Stop 的 Robot hold 证据；
// 只有旧 Scene 已被 Runtime 明确销毁、supervisor 也已经退出时，启动层才可以
// 回收遗留的 Pilot/AbilityFramework 进程和端口租约。
type InterruptedSimulationCleaner interface {
	ReclaimInterruptedSimulation(context.Context, RuntimeInstance, string) (StopEvidence, error)
}

type EventSink interface {
	PublishRuntimeEvent(context.Context, Event) error
}

type nopEventSink struct{}

func (nopEventSink) PublishRuntimeEvent(context.Context, Event) error { return nil }
