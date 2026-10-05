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

// Package robot 实现 Semantic Server 对多个 Pilot、Robot Skill 包和 Robot
// Execution 的统一管理。Pilot 本地运行细节留在 internal/pilot。
package robot

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"insightos.cn/semantic-framework/internal/robotruntime"
	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
)

var (
	ErrSkillOutsideTask    = errors.New("Robot Skill 不在当前已批准 SubTask 范围内")
	ErrPilotOffline        = errors.New("Pilot 当前不在线")
	ErrRobotBusy           = errors.New("Robot 已有活动 Execution")
	ErrExecutionNotActive  = errors.New("Robot Execution 当前不可停止")
	ErrSkillUnavailable    = errors.New("Robot Skill 未在 Pilot 安装并启用")
	ErrAbilityTaskUnknown  = errors.New("Ability 实例未声明该调试 Task")
	ErrRequestConflict     = errors.New("request_key 已用于不同的 Robot Skill 请求")
	ErrSkillInputInvalid   = errors.New("Robot Skill 输入不满足声明模型")
	ErrUnsafePackage       = errors.New("Robot Skill 包包含非法路径或文件")
	ErrTransferUnavailable = errors.New("传输不存在或已经失效")
	ErrDirectRunScope      = errors.New("直接 Robot 请求不属于当前有效 Robot Agent Run")
)

const (
	maxSkillPackageBytes   int64 = 100 << 20
	maxArtifactUploadBytes int64 = 2 << 30
)

type EventSink interface {
	PublishRobotEvent(projectID, resourceType, resourceID, eventType string, revision int64, payload any)
}

type nopEventSink struct{}

func (nopEventSink) PublishRobotEvent(string, string, string, string, int64, any) {}

// PilotCommand 是 Server 通过已认证 Pilot 会话下发的内部命令信封。
// 类型公开给同一进程内的 Gateway 和 HTTP 契约测试使用；它不是新的网络协议。
type PilotCommand struct {
	Type      string `json:"type"`
	CommandID string `json:"command_id"`
	Payload   any    `json:"payload,omitempty"`
}

type pilotSession struct {
	pilot  store.RobotPilot
	send   chan PilotCommand
	closed chan struct{}
	once   sync.Once
}

type pendingCommand struct {
	PilotInstanceID string
	Command         PilotCommand
	Result          chan commandAck
}

type commandAck struct {
	OK     bool
	Error  string
	Result map[string]any
}

func (s *pilotSession) close() { s.once.Do(func() { close(s.closed) }) }

type Transfer struct {
	Token           string
	Mode            string
	PilotInstanceID string
	ExecutionID     string
	LocalArtifactID string
	ArtifactID      string
	PackagePath     string
	MediaType       string
	Summary         string
	ExpiresAt       time.Time
}

// ExecutionObserver 接收已经持久化的 Robot Execution 变化。Robot Service
// 只拥有 Pilot 协议与物理执行事实，不应直接修改 Workflow/Task/SubTask；由
// Workflow Service 实现该接口，才能保证 completed 只推进精确关联的 SubTask，
// 而 accepted/running 等中间态不会被误判为任务完成。
type ExecutionObserver interface {
	OnRobotExecutionChanged(context.Context, store.RobotExecution, string, map[string]any) error
}

// AvailabilityObserver 只接收“Robot 从不可调度变为可调度”的边沿事件。
// Workflow 以该事件重新评估 waiting_resource Task，避免用轮询器不断扫描设备；
// 具体能力、Skill 和批准范围仍由 TaskAssignmentResolver 在分配时重新核对。
type AvailabilityObserver interface {
	OnRobotAvailabilityChanged(context.Context, string)
}

// Service 是 Server Robot 主链的应用服务。所有执行事实先持久化，再下发命令。
type Service struct {
	// 发布锁保护“检查版本 → 写包 → 登记”的完整过程，已发布版本保持不可变。
	publishMu    sync.Mutex
	st           *store.Store
	events       EventSink
	now          func() time.Time
	observer     ExecutionObserver
	availability AvailabilityObserver
	stateReader  StateReader
	stateSource  string

	mu             sync.RWMutex
	sessions       map[string]*pilotSession
	transfers      map[string]Transfer
	commands       map[string]pendingCommand
	subscribers    map[chan DeviceEvent]struct{}
	deviceSequence atomic.Int64
	admissions     sync.Map // robot ID -> *sync.Mutex; serializes command admission with stop
}

func NewService(st *store.Store, events EventSink) *Service {
	if events == nil {
		events = nopEventSink{}
	}
	return &Service{st: st, events: events, now: time.Now,
		sessions: make(map[string]*pilotSession), transfers: make(map[string]Transfer),
		commands: make(map[string]pendingCommand), subscribers: make(map[chan DeviceEvent]struct{})}
}

func (s *Service) Store() *store.Store { return s.st }

// SetExecutionObserver 在 Bootstrap 完成 Workflow Service 装配后接线。使用
// setter 可以避免 Robot 与 Workflow 包互相依赖，同时保持事件先落库、再推进
// 业务任务的顺序。
func (s *Service) SetExecutionObserver(observer ExecutionObserver) { s.observer = observer }

func (s *Service) SetAvailabilityObserver(observer AvailabilityObserver) { s.availability = observer }

func (s *Service) Connect(pilot store.RobotPilot) (<-chan PilotCommand, func(), error) {
	return s.connect(pilot, nil, false)
}

// ConnectWithSkillSnapshot 是正式 Pilot Gateway 的注册入口。旧单元测试仍可用
// Connect 构造最小会话；生产连接必须先用本地 active 目录刷新 actual，再开始
// desired 对账，避免同一 Pilot ID 的新实例继承旧安装记录。
func (s *Service) ConnectWithSkillSnapshot(
	pilot store.RobotPilot, skills []store.RobotPilotSkill,
) (<-chan PilotCommand, func(), error) {
	return s.connect(pilot, skills, true)
}

func (s *Service) connect(
	pilot store.RobotPilot, skills []store.RobotPilotSkill, skillsReported bool,
) (<-chan PilotCommand, func(), error) {
	if strings.TrimSpace(pilot.PilotInstanceID) == "" || strings.TrimSpace(pilot.RobotID) == "" {
		return nil, nil, errors.New("pilot_instance_id 和 robot_id 必填")
	}
	now := s.now().UTC()
	pilot.RuntimeInstance = nil
	if runtime, runtimeErr := s.st.GetLatestRuntimeByRobot(context.Background(), pilot.RobotID); runtimeErr == nil &&
		runtime.PilotInstanceID == pilot.PilotInstanceID {
		pilot.RuntimeInstance = &runtime
	}
	if runtime := pilot.RuntimeInstance; runtime != nil {
		// 受管实例必须先允许精确绑定的 Pilot 在 starting 阶段注册，启动器才能
		// 观察到七类 Ability 与 desired Skill 已就绪，再把 Runtime 提升为
		// ready。若只允许 ready 注册，会形成“Runtime 等 Pilot、Pilot 等
		// Runtime”的循环等待。stopping/failed/interrupted 仍禁止新连接。
		if runtime.Status != robotruntime.StateStarting &&
			runtime.Status != robotruntime.StateReady &&
			runtime.Status != robotruntime.StateDegraded {
			return nil, nil, fmt.Errorf("Runtime Instance 不接受 Pilot 注册: %s", runtime.Status)
		}
	}
	if previous, err := s.st.GetRobotPilot(pilot.PilotInstanceID); err == nil {
		pilot.Revision = previous.Revision + 1
		if pilot.CurrentExecutionID == "" {
			pilot.CurrentExecutionID = previous.CurrentExecutionID
		}
	} else if pilot.Revision <= 0 {
		pilot.Revision = 1
	}
	pilot.Status = "online"
	pilot.LastSeenAt = now

	s.mu.Lock()
	if current := s.sessions[pilot.PilotInstanceID]; current != nil {
		current.close()
		s.dropPendingSkillCommandsLocked(pilot.PilotInstanceID)
	}
	// 同一 Robot 的旧 Pilot 如果仍有活动执行，只允许原实例重连，避免新进程
	// 在物理状态未知时直接接管。
	for id, session := range s.sessions {
		if id != pilot.PilotInstanceID && session.pilot.RobotID == pilot.RobotID {
			if session.pilot.CurrentExecutionID != "" {
				s.mu.Unlock()
				return nil, nil, fmt.Errorf("Robot %s 尚有未确认执行: %w", pilot.RobotID, ErrRobotBusy)
			}
			session.close()
			delete(s.sessions, id)
		}
	}
	session := &pilotSession{pilot: pilot, send: make(chan PilotCommand, 128), closed: make(chan struct{})}
	s.sessions[pilot.PilotInstanceID] = session
	s.mu.Unlock()
	if err := s.st.SaveRobotPilot(pilot); err != nil {
		return nil, nil, err
	}
	if skillsReported {
		for index := range skills {
			skills[index].PilotInstanceID = pilot.PilotInstanceID
			skills[index].UpdatedAt = now
		}
		if err := s.st.ReplaceRobotPilotSkills(pilot.PilotInstanceID, skills); err != nil {
			return nil, nil, err
		}
	}
	// RobotDeployment 只在该 Robot 首次接入时播种期望 Skill；之后 Server
	// 保存的 desired 状态是唯一来源，设备页修改不会被重连时的旧配置覆盖。
	desired, desiredErr := s.st.ListRobotDesiredSkills(pilot.RobotID)
	if desiredErr != nil {
		return nil, nil, desiredErr
	}
	if len(desired) == 0 {
		for _, item := range pilot.DesiredSkills {
			item.RobotID = pilot.RobotID
			item.UpdatedAt = now
			if err := s.st.SaveRobotDesiredSkill(item); err != nil {
				return nil, nil, err
			}
		}
	}
	go func() { _ = s.ReconcileDesiredSkills(pilot.RobotID) }()
	s.events.PublishRobotEvent("", "pilot", pilot.PilotInstanceID, "pilot.online", pilot.Revision, pilot)
	s.publishPilotView(pilot, "pilot.online", "robot")
	s.notifyRobotAvailable(pilot)

	return session.send, func() { s.disconnect(pilot.PilotInstanceID, session) }, nil
}

func (s *Service) disconnect(pilotID string, expected *pilotSession) {
	s.mu.Lock()
	current := s.sessions[pilotID]
	if current != expected {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, pilotID)
	s.dropPendingSkillCommandsLocked(pilotID)
	current.close()
	pilot := current.pilot
	pilot.Status = "offline"
	pilot.RobotStatus = "offline"
	pilot.AbilityFrameworkStatus = "offline"
	pilot.Revision++
	pilot.LastSeenAt = s.now().UTC()
	s.mu.Unlock()
	_ = s.st.SaveRobotPilot(pilot)
	s.events.PublishRobotEvent("", "pilot", pilotID, "pilot.offline", pilot.Revision, pilot)
	s.publishPilotView(pilot, "pilot.offline", "robot")
}

func (s *Service) Heartbeat(pilotID string, update store.RobotPilot) error {
	s.mu.Lock()
	session := s.sessions[pilotID]
	if session == nil {
		s.mu.Unlock()
		return ErrPilotOffline
	}
	previous := session.pilot
	update.PilotInstanceID = pilotID
	update.RobotID = previous.RobotID
	if update.DisplayName == "" {
		update.DisplayName = previous.DisplayName
	}
	if update.RobotModel == "" {
		update.RobotModel = previous.RobotModel
	}
	if update.Backend == "" {
		update.Backend = previous.Backend
	}
	if update.PilotVersion == "" {
		update.PilotVersion = previous.PilotVersion
	}
	if update.RobotStatus == "" {
		update.RobotStatus = previous.RobotStatus
	}
	if update.AbilityFrameworkStatus == "" {
		update.AbilityFrameworkStatus = previous.AbilityFrameworkStatus
	}
	if update.Abilities == nil {
		update.Abilities = previous.Abilities
	}
	if update.Sensors == nil {
		update.Sensors = previous.Sensors
	}
	if update.DesiredSkills == nil {
		update.DesiredSkills = previous.DesiredSkills
	}
	if update.RuntimeInstance == nil {
		update.RuntimeInstance = previous.RuntimeInstance
	}
	if update.SkillCatalogRevision < previous.SkillCatalogRevision {
		update.SkillCatalogRevision = previous.SkillCatalogRevision
	}
	if update.AbilityCatalogRevision < previous.AbilityCatalogRevision {
		update.AbilityCatalogRevision = previous.AbilityCatalogRevision
	}
	if update.CurrentExecutionID == "" && previous.CurrentExecutionID != "" {
		update.CurrentExecutionID = previous.CurrentExecutionID
	}
	update.Status = "online"
	statusChanged := pilotStatusChanged(previous, update)
	update.Revision = previous.Revision
	if statusChanged {
		update.Revision++
	}
	update.LastSeenAt = s.now().UTC()
	session.pilot = update
	s.mu.Unlock()
	if err := s.st.SaveRobotPilot(update); err != nil {
		return err
	}
	if statusChanged {
		s.events.PublishRobotEvent("", "pilot", pilotID, "pilot.status", update.Revision, update)
		s.publishPilotView(update, "pilot.status", "robot")
	}
	if !robotAvailable(previous) && robotAvailable(update) {
		s.notifyRobotAvailable(update)
	}
	return nil
}

func pilotStatusChanged(previous, current store.RobotPilot) bool {
	// LastSeenAt只证明现有WebSocket仍存活，不是设备状态变化。心跳仍更新
	// robot_pilots中的在线时间，但不能每秒把完整Ability/Skill目录再次写入
	// Project事件并推给Web；真正的状态、目录或当前Execution变化仍正常发布。
	previous.LastSeenAt = time.Time{}
	current.LastSeenAt = time.Time{}
	previous.Revision = 0
	current.Revision = 0
	return !reflect.DeepEqual(previous, current)
}

func (s *Service) IsOnline(pilotID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[pilotID] != nil
}

func (s *Service) sendCommand(pilotID, typ string, payload any) (string, error) {
	id := "cmd-" + uuid.NewString()
	command := PilotCommand{Type: typ, CommandID: id, Payload: payload}
	s.mu.RLock()
	session := s.sessions[pilotID]
	s.mu.RUnlock()
	if session == nil {
		return "", ErrPilotOffline
	}
	select {
	case session.send <- command:
		s.mu.Lock()
		s.commands[id] = pendingCommand{PilotInstanceID: pilotID, Command: command}
		s.mu.Unlock()
		return id, nil
	case <-session.closed:
		return "", ErrPilotOffline
	default:
		return "", errors.New("Pilot 命令队列已满")
	}
}

func (s *Service) sendCommandAndWait(ctx context.Context, pilotID, typ string,
	payload any) (map[string]any, error) {
	id := "cmd-" + uuid.NewString()
	command := PilotCommand{Type: typ, CommandID: id, Payload: payload}
	waiter := make(chan commandAck, 1)
	s.mu.Lock()
	session := s.sessions[pilotID]
	if session == nil {
		s.mu.Unlock()
		return nil, ErrPilotOffline
	}
	select {
	case session.send <- command:
		s.commands[id] = pendingCommand{PilotInstanceID: pilotID, Command: command, Result: waiter}
		s.mu.Unlock()
	case <-session.closed:
		s.mu.Unlock()
		return nil, ErrPilotOffline
	default:
		s.mu.Unlock()
		return nil, errors.New("Pilot 命令队列已满")
	}
	select {
	case ack := <-waiter:
		if !ack.OK {
			return nil, errors.New(ack.Error)
		}
		return ack.Result, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.commands, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-session.closed:
		s.mu.Lock()
		delete(s.commands, id)
		s.mu.Unlock()
		return nil, ErrPilotOffline
	}
}

func (s *Service) dropPendingSkillCommandsLocked(pilotID string) {
	// WebSocket 断开后旧连接不可能再完成这些命令。重连注册会提交本地
	// packages/active 快照，随后按真实 actual 重新对账；保留旧 pending
	// 反而会让新连接永远跳过所需安装或启用。
	for id, pending := range s.commands {
		if pending.PilotInstanceID == pilotID && isSkillCommand(pending.Command.Type) {
			delete(s.commands, id)
		}
	}
}

func isSkillCommand(typ string) bool {
	switch typ {
	case "skill.install", "skill.enable", "skill.disable", "skill.uninstall":
		return true
	default:
		return false
	}
}

// sendSkillCommandOnce 利用现有 pending command 作为在途事实，不再引入
// 另一套 Skill 操作状态。skill.status 会触发下一次 desired/actual 对账；如果
// 不先排除同一包尚未 ack 的命令，三个安装事件会相互触发全量重装并形成循环。
func (s *Service) sendSkillCommandOnce(
	pilotID, typ string, payload map[string]any,
) (string, bool, error) {
	name, version := stringValue(payload["name"]), stringValue(payload["version"])
	id := "cmd-" + uuid.NewString()
	command := PilotCommand{Type: typ, CommandID: id, Payload: payload}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[pilotID]
	if session == nil {
		return "", false, ErrPilotOffline
	}
	for _, pending := range s.commands {
		if pending.PilotInstanceID != pilotID || !isSkillCommand(pending.Command.Type) {
			continue
		}
		current, ok := pending.Command.Payload.(map[string]any)
		if ok && stringValue(current["name"]) == name && stringValue(current["version"]) == version {
			return pending.Command.CommandID, false, nil
		}
	}
	select {
	case session.send <- command:
		s.commands[id] = pendingCommand{PilotInstanceID: pilotID, Command: command}
		return id, true, nil
	case <-session.closed:
		return "", false, ErrPilotOffline
	default:
		return "", false, errors.New("Pilot 命令队列已满")
	}
}

// HandleCommandAck 把 Pilot 的命令失败转成持久业务状态。ack 成功只表示
// Pilot 已处理命令；Skill/Execution 的成功仍由后续业务事件推进。
func (s *Service) HandleCommandAck(pilotID, commandID string, ok bool, errorText string) error {
	return s.HandleCommandAckResult(pilotID, commandID, ok, errorText, nil)
}

func (s *Service) HandleCommandAckResult(pilotID, commandID string, ok bool,
	errorText string, result map[string]any) error {
	s.mu.Lock()
	pending, exists := s.commands[commandID]
	if exists {
		delete(s.commands, commandID)
	}
	s.mu.Unlock()
	if !exists || pending.PilotInstanceID != pilotID {
		return errors.New("Pilot command.ack 无法匹配已下发命令")
	}
	if pending.Result != nil {
		pending.Result <- commandAck{OK: ok, Error: errorText, Result: result}
		return nil
	}
	if ok {
		if isSkillCommand(pending.Command.Type) {
			// Pilot 先上报 actual，再 ack 当前命令。actual 事件发生时当前命令
			// 仍在 pending，因而会被上面的去重正确跳过；ack 后必须再评估
			// 一次，才能从 install 自然推进到 enable。
			if pilot, pilotErr := s.st.GetRobotPilot(pilotID); pilotErr == nil {
				go func(robotID string) { _ = s.ReconcileDesiredSkills(robotID) }(pilot.RobotID)
			}
		}
		return nil
	}
	if strings.TrimSpace(errorText) == "" {
		errorText = "Pilot 拒绝命令"
	}
	if payload, payloadOK := pending.Command.Payload.(map[string]any); payloadOK {
		switch pending.Command.Type {
		case "skill.install", "skill.enable", "skill.disable", "skill.uninstall":
			item := store.RobotPilotSkill{PilotInstanceID: pilotID,
				Name: stringValue(payload["name"]), Version: stringValue(payload["version"]),
				Status: "failed", Error: errorText, UpdatedAt: s.now().UTC()}
			if saveErr := s.st.SaveRobotPilotSkill(item); saveErr != nil {
				return saveErr
			}
		case "execution.start":
			// Pilot 在创建 Worker 前已经明确拒绝启动时不存在未知物理动作。
			// 这里必须把 queued 记录收敛为 failed 并释放 Robot；保留为 active
			// 会让后续调试一直返回 busy，execution.stop 又必然找不到它。
			executionID := executionIDFromCommandPayload(payload)
			if executionID != "" {
				if saveErr := s.failRejectedExecution(pilotID, executionID, errorText); saveErr != nil {
					return saveErr
				}
			}
		case "execution.stop":
			executionID := stringValue(payload["execution_id"])
			if executionID != "" {
				if saveErr := s.interruptRejectedStop(pilotID, executionID, errorText); saveErr != nil {
					return saveErr
				}
			}
		}
	}
	return fmt.Errorf("Pilot 命令 %s (%s) 失败: %s", pending.Command.Type, commandID, errorText)
}

func executionIDFromCommandPayload(payload map[string]any) string {
	switch execution := payload["execution"].(type) {
	case store.RobotExecution:
		return execution.ID
	case map[string]any:
		if id := stringValue(execution["id"]); id != "" {
			return id
		}
		return stringValue(execution["execution_id"])
	default:
		return ""
	}
}

func (s *Service) failRejectedExecution(pilotID, executionID, message string) error {
	execution, err := s.st.GetRobotExecution(executionID)
	if err != nil || execution.PilotInstanceID != pilotID || !activeRobotStatus(execution.Status) {
		return err
	}
	execution.Status = "failed"
	execution.Error = map[string]any{"code": "PILOT_START_REJECTED", "message": message}
	execution.Revision++
	execution.UpdatedAt = s.now().UTC()
	if err := s.st.SaveRobotExecution(execution); err != nil {
		return err
	}
	if pilot, pilotErr := s.st.GetRobotPilot(pilotID); pilotErr == nil &&
		pilot.CurrentExecutionID == execution.ID {
		s.setPilotExecution(pilot, "")
	}
	payload := cloneMap(execution.Error)
	s.publishExecution(execution, "robot.execution.failed", payload)
	if s.observer != nil {
		return s.observer.OnRobotExecutionChanged(context.Background(), execution,
			"robot.execution.failed", payload)
	}
	return nil
}

func (s *Service) interruptRejectedStop(pilotID, executionID, message string) error {
	execution, err := s.st.GetRobotExecution(executionID)
	if err != nil || execution.PilotInstanceID != pilotID || !activeRobotStatus(execution.Status) {
		return err
	}
	// stop 被 Pilot 拒绝时不能继续展示 stopping，也不能谎报 stopped。此时
	// 物理状态未获确认，保留 interrupted 与 Robot 锁，等待 reconcile 或人工
	// 处理；这与 start 在 Worker 创建前被拒绝的 failed 语义不同。
	execution.Status = "interrupted"
	execution.Error = map[string]any{"code": "PILOT_STOP_REJECTED", "message": message}
	execution.Revision++
	execution.UpdatedAt = s.now().UTC()
	if err := s.st.SaveRobotExecution(execution); err != nil {
		return err
	}
	if pilot, pilotErr := s.st.GetRobotPilot(pilotID); pilotErr == nil {
		s.setPilotInterrupted(pilot, execution.ID)
	}
	payload := cloneMap(execution.Error)
	s.publishExecution(execution, "robot.execution.interrupted", payload)
	if s.observer != nil {
		return s.observer.OnRobotExecutionChanged(context.Background(), execution,
			"robot.execution.interrupted", payload)
	}
	return nil
}

type RunRequest struct {
	ProjectID    string
	WorkflowID   string
	TaskID       string
	SubtaskID    string
	RunID        string
	AgentID      string
	RobotID      string
	SkillName    string
	SkillVersion string
	RequestKey   string
	Input        map[string]any
	ArtifactRefs []string
}

func (s *Service) GetRobot(robotID string) (store.RobotPilot, []store.RobotPilotSkill, error) {
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if err != nil {
		return pilot, nil, err
	}
	skills, err := s.st.ListRobotPilotSkills(pilot.PilotInstanceID)
	return pilot, skills, err
}

func activeRobotStatus(status string) bool {
	switch status {
	case "queued", "starting", "running", "waiting_agent", "stopping":
		return true
	default:
		return false
	}
}

func sameRunRequest(existing store.RobotExecution, request RunRequest) bool {
	if existing.RequestDigest != "" {
		return existing.RequestDigest == robotRequestDigest(request)
	}
	if existing.RobotID != request.RobotID || existing.SkillName != request.SkillName || existing.SkillVersion != request.SkillVersion {
		return false
	}
	left, _ := json.Marshal(existing.Input)
	right, _ := json.Marshal(request.Input)
	return bytes.Equal(left, right)
}

func robotRequestDigest(request RunRequest) string {
	runID := ""
	if request.TaskID == "" {
		runID = request.RunID
	}
	data, _ := json.Marshal([]any{request.ProjectID, request.WorkflowID, request.TaskID,
		request.SubtaskID, runID, request.RobotID, request.SkillName, request.SkillVersion, request.Input})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// ValidateTaskSkillScope 在高风险工具真正下发前核对用户批准后的持久范围。
// 安装状态和 Robot 互斥属于 Run 的设备检查；这里专门保证模型不能借当前
// Robot Task 调用另一个 Skill，也不能在旧 Workflow 或旧 SubTask 上发命令。
func stringAllowed(allowed []string, actual string) bool {
	if len(allowed) == 0 {
		return true
	}
	actual = strings.TrimSpace(actual)
	for _, value := range allowed {
		if strings.TrimSpace(value) == actual {
			return true
		}
	}
	return false
}

func (s *Service) ValidateTaskSkillScope(request RunRequest) error {
	if request.ProjectID == "" || request.WorkflowID == "" || request.TaskID == "" ||
		request.SubtaskID == "" || request.RobotID == "" {
		return ErrSkillOutsideTask
	}
	workflow, err := s.st.GetWorkflow(request.WorkflowID)
	if err != nil || workflow.ProjectID != request.ProjectID ||
		workflow.Status != store.WorkflowStatusRunning ||
		workflow.ConfirmedRevision != workflow.Revision {
		return ErrSkillOutsideTask
	}
	var approved struct {
		RobotIDs      []string `json:"robot_ids"`
		AllowedSkills []string `json:"allowed_skills"`
	}
	if len(workflow.ApprovedScope) != 0 && json.Unmarshal(workflow.ApprovedScope, &approved) != nil {
		return ErrSkillOutsideTask
	}
	if !stringAllowed(approved.RobotIDs, request.RobotID) ||
		!stringAllowed(approved.AllowedSkills, request.SkillName) {
		return ErrSkillOutsideTask
	}
	task, err := s.st.GetTask(request.TaskID)
	if err != nil || task.WorkflowID != workflow.ID ||
		task.Status != store.TaskStatusRunning ||
		task.AssignedRobotID != request.RobotID ||
		task.RequiredRole != "robot" {
		return ErrSkillOutsideTask
	}
	subtask, err := s.st.GetSubTask(request.SubtaskID)
	if err != nil || subtask.TaskID != task.ID ||
		subtask.Status != store.TaskStatusRunning || subtask.Kind != "robot_skill" {
		return ErrSkillOutsideTask
	}
	var spec struct {
		SkillName    string `json:"skill_name"`
		SkillVersion string `json:"skill_version"`
	}
	if json.Unmarshal(subtask.Spec, &spec) != nil ||
		strings.TrimSpace(spec.SkillName) != strings.TrimSpace(request.SkillName) {
		return ErrSkillOutsideTask
	}
	// 未固定版本表示允许 Pilot 选择用户批准 Skill 的已启用版本；一旦 Proposal
	// 或 Task Agent 明确了版本，模型不能在执行时自行切换。
	if strings.TrimSpace(spec.SkillVersion) != "" &&
		strings.TrimSpace(spec.SkillVersion) != strings.TrimSpace(request.SkillVersion) {
		return ErrSkillOutsideTask
	}
	return nil
}

func (s *Service) Run(ctx context.Context, request RunRequest) (store.RobotExecution, error) {
	if request.ProjectID == "" || request.RobotID == "" || request.SkillName == "" || request.SkillVersion == "" || request.RequestKey == "" {
		return store.RobotExecution{}, errors.New("project_id、robot_id、skill_name、skill_version 和 request_key 必填")
	}
	if err := ctx.Err(); err != nil {
		return store.RobotExecution{}, err
	}
	if err := s.ValidateRobotProject(request.ProjectID, request.RobotID); err != nil {
		return store.RobotExecution{}, err
	}
	if request.TaskID == "" && request.RunID != "" {
		if request.WorkflowID != "" || request.SubtaskID != "" {
			return store.RobotExecution{}, ErrDirectRunScope
		}
		if err := s.ValidateDirectRunScope(request.ProjectID, request.RunID, request.AgentID, request.RobotID); err != nil {
			return store.RobotExecution{}, err
		}
	}
	if existing, err := s.st.GetRobotExecutionByRequest(request.ProjectID, request.RequestKey); err == nil {
		if !sameRunRequest(existing, request) {
			return existing, ErrRequestConflict
		}
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.RobotExecution{}, err
	}
	admission := s.robotAdmission(request.RobotID)
	if !admission.TryLock() {
		return store.RobotExecution{}, ErrRobotBusy
	}
	defer admission.Unlock()
	// Keep the fingerprint of the approved arguments before Pilot normalization.
	// Defaults supplied by input validation must not turn a retry into a new action.
	originalRequest := request
	if request.TaskID != "" || request.WorkflowID != "" || request.SubtaskID != "" {
		if err := s.ValidateTaskSkillScope(request); err != nil {
			return store.RobotExecution{}, err
		}
	}
	if err := s.st.CheckRobotAdmission(request.RobotID, request.TaskID, request.RunID); err != nil {
		if errors.Is(err, store.ErrRobotReserved) {
			return store.RobotExecution{}, fmt.Errorf("%w: %v", ErrRobotBusy, err)
		}
		return store.RobotExecution{}, err
	}

	pilot, err := s.st.GetActiveRobotPilot(request.RobotID)
	if err != nil {
		return store.RobotExecution{}, fmt.Errorf("读取Robot %s绑定的Pilot: %w", request.RobotID, err)
	}
	// Execution 列表只描述执行状态，真正的 Robot 物理锁由 Pilot 当前指针维护。
	// interrupted 会从活动状态集合退出，但在 hold 对账完成前必须继续保留该指针；
	// 否则直接调用 robot.run 可以绕过调度器，在状态未知的 Robot 上启动第二个动作。
	if pilot.CurrentExecutionID != "" || pilot.RobotStatus == "busy" ||
		pilot.RobotStatus == "stopping" || pilot.RobotStatus == "interrupted" {
		return store.RobotExecution{}, ErrRobotBusy
	}
	if !s.IsOnline(pilot.PilotInstanceID) {
		return store.RobotExecution{}, ErrPilotOffline
	}
	installed, err := s.st.GetRobotPilotSkill(pilot.PilotInstanceID, request.SkillName, request.SkillVersion)
	if err != nil || !installed.Enabled || installed.Status != "installed" {
		return store.RobotExecution{}, ErrSkillUnavailable
	}
	validation, err := s.sendCommandAndWait(ctx, pilot.PilotInstanceID,
		"skill.validate_input", map[string]any{"name": request.SkillName,
			"version": request.SkillVersion, "input": request.Input})
	if err != nil {
		return store.RobotExecution{}, fmt.Errorf("校验Robot Skill输入: %w", err)
	}
	valid, _ := validation["valid"].(bool)
	if !valid {
		return store.RobotExecution{}, fmt.Errorf("%w: %s", ErrSkillInputInvalid,
			stringValue(validation["error"]))
	}
	if normalized, ok := validation["input"].(map[string]any); ok {
		request.Input = normalized
	}
	if err := ctx.Err(); err != nil {
		return store.RobotExecution{}, err
	}

	now := s.now().UTC()
	execution := store.RobotExecution{ID: "rex-" + uuid.NewString(), ProjectID: request.ProjectID,
		WorkflowID: request.WorkflowID, TaskID: request.TaskID, SubtaskID: request.SubtaskID, RunID: request.RunID,
		RobotID: request.RobotID, PilotInstanceID: pilot.PilotInstanceID, SkillName: request.SkillName,
		SkillVersion: request.SkillVersion, RequestKey: request.RequestKey, RequestDigest: robotRequestDigest(originalRequest), Status: "queued",
		Input: cloneMap(request.Input), ArtifactRefs: append([]string(nil), request.ArtifactRefs...),
		Revision: 1, CreatedAt: now, UpdatedAt: now}
	execution, created, err := s.st.AdmitRobotExecution(execution, request.AgentID)
	if err != nil {
		if errors.Is(err, store.ErrRobotReserved) {
			return execution, fmt.Errorf("%w: %v", ErrRobotBusy, err)
		}
		if errors.Is(err, store.ErrInvalidState) && request.TaskID == "" && request.RunID != "" {
			return execution, ErrDirectRunScope
		}
		if errors.Is(err, store.ErrInvalidState) && request.TaskID != "" {
			return execution, ErrSkillOutsideTask
		}
		return execution, err
	}
	if !created {
		if !sameRunRequest(execution, originalRequest) {
			return execution, ErrRequestConflict
		}
		return execution, nil
	}
	downloads, err := s.authorizeArtifactDownloads(execution)
	if err != nil {
		execution.Status = "failed"
		execution.Revision++
		execution.Error = map[string]any{"code": "ARTIFACT_DOWNLOAD_FAILED", "message": err.Error()}
		_ = s.st.SaveRobotExecution(execution)
		return execution, err
	}
	dispatchErr := ctx.Err()
	if dispatchErr == nil && request.TaskID == "" && request.RunID != "" {
		dispatchErr = s.ValidateDirectRunScope(request.ProjectID, request.RunID, request.AgentID, request.RobotID)
	}
	if dispatchErr != nil {
		execution.Status = "stopped"
		execution.Revision++
		execution.Error = map[string]any{"code": "CANCELLED_BEFORE_DISPATCH", "message": dispatchErr.Error()}
		_ = s.st.SaveRobotExecution(execution)
		s.publishExecution(execution, "robot.execution.stopped", map[string]any{"reason": "cancelled_before_dispatch"})
		return execution, dispatchErr
	}
	payload := map[string]any{"execution": execution, "artifact_downloads": downloads}
	// Publish the admitted owner before the Pilot can report an immediate result;
	// otherwise a fast completion could be overwritten by a late busy pointer.
	s.setPilotExecution(pilot, execution.ID)
	s.publishExecution(execution, "robot.execution.queued", map[string]any{"status": execution.Status})
	if _, err := s.sendCommand(pilot.PilotInstanceID, "execution.start", payload); err != nil {
		execution.Status = "interrupted"
		execution.Revision++
		execution.UpdatedAt = s.now().UTC()
		execution.Error = map[string]any{"code": "PILOT_COMMAND_FAILED", "message": err.Error()}
		_ = s.st.SaveRobotExecution(execution)
		s.setPilotInterrupted(pilot, execution.ID)
		s.publishExecution(execution, "robot.execution.interrupted", map[string]any{"error": execution.Error})
		return execution, err
	}
	_ = ctx
	return execution, nil
}

func (s *Service) Stop(ctx context.Context, projectID, executionID, reason string) (store.RobotExecution, error) {
	execution, err := s.st.GetRobotExecution(executionID)
	if err != nil {
		return execution, err
	}
	if projectID != "" && execution.ProjectID != projectID {
		return execution, store.ErrNotFound
	}
	admission := s.robotAdmission(execution.RobotID)
	admission.Lock()
	defer admission.Unlock()
	// Re-read after admission: an execution.start already being prepared must be
	// sent before its stop, and a late stop cannot overwrite a terminal event.
	execution, err = s.st.GetRobotExecution(executionID)
	if err != nil {
		return execution, err
	}
	if !activeRobotStatus(execution.Status) {
		if execution.Status != "interrupted" {
			return execution, ErrExecutionNotActive
		}
		pilot, pilotErr := s.st.GetActiveRobotPilot(execution.RobotID)
		if pilotErr != nil {
			if errors.Is(pilotErr, store.ErrNotFound) {
				return execution, ErrExecutionNotActive
			}
			return execution, pilotErr
		}
		// interrupted 只有仍是 Pilot 权威当前执行时才允许重试安全停止。
		// 同一 Skill 的旧 interrupted 历史不能向新的场景实例发送 stop。
		if pilot.CurrentExecutionID != execution.ID {
			return execution, ErrExecutionNotActive
		}
		if !s.IsOnline(pilot.PilotInstanceID) {
			return execution, ErrPilotOffline
		}
	}
	if execution.TaskID == "" && execution.RunID != "" {
		if err := s.cancelDirectRun(execution.RunID); err != nil {
			return execution, err
		}
	}
	execution.Status = "stopping"
	execution.Revision++
	execution.UpdatedAt = s.now().UTC()
	if err := s.st.SaveRobotExecution(execution); err != nil {
		return execution, err
	}
	_, err = s.sendCommand(execution.PilotInstanceID, "execution.stop", map[string]any{"execution_id": execution.ID, "reason": reason})
	if err != nil {
		execution.Status = "interrupted"
		execution.Revision++
		execution.Error = map[string]any{"code": "PILOT_OFFLINE", "message": err.Error()}
		_ = s.st.SaveRobotExecution(execution)
	}
	s.publishExecution(execution, "robot.execution.stopping", map[string]any{"reason": reason, "status": execution.Status})
	if err != nil && s.observer != nil {
		_ = s.observer.OnRobotExecutionChanged(context.Background(), execution,
			"robot.execution.interrupted", map[string]any{"reason": reason})
	}
	_ = ctx
	return execution, err
}

// StopAndWait 用于 reset/场景切换等基础设施协调。它复用正式 execution.stop，
// 等待 Pilot 上报终态；这里只轮询已持久化事实，不触发模型，也不会重新发送 Action。
func (s *Service) StopAndWait(
	ctx context.Context, projectID, executionID, reason string,
) (store.RobotExecution, error) {
	execution, err := s.Stop(ctx, projectID, executionID, reason)
	if errors.Is(err, ErrExecutionNotActive) && execution.Status != "interrupted" {
		return execution, nil
	}
	if err != nil {
		return execution, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		execution, err = s.st.GetRobotExecution(executionID)
		if err != nil {
			return execution, err
		}
		if !activeRobotStatus(execution.Status) {
			switch execution.Status {
			case "interrupted":
				return execution, errors.New("Robot Execution 物理状态未知")
			case "stopped":
				if safe, _ := execution.Result["safe"].(bool); !safe {
					return execution, errors.New("Robot Execution 没有安全停止证据")
				}
			}
			return execution, nil
		}
		select {
		case <-ctx.Done():
			return execution, ctx.Err()
		case <-ticker.C:
		}
	}
}

// ConfirmManagedSimulationHold 只用于 Framework 管理的仿真场景：调用方必须先
// 从 Runtime 的同步 hold 接口取得底盘、双臂和工具已经停止的证据，并提供该 Pilot
// 持久化的唯一 current_execution_id。它不会扫描或批量改写同一 Robot 的其他历史。
//
// 真机、外部共享 Runtime 和普通 robot.stop 不能调用这个入口，不能把“进程已退”
// 或“设备离线”误当作 hold。
func (s *Service) ConfirmManagedSimulationHold(
	ctx context.Context, projectID, robotID, executionID, reason string,
) error {
	if strings.TrimSpace(executionID) == "" {
		return nil
	}
	execution, err := s.st.GetRobotExecution(executionID)
	if err != nil {
		return err
	}
	if execution.ProjectID != projectID || execution.RobotID != robotID {
		return store.ErrNotFound
	}
	if !activeRobotStatus(execution.Status) && execution.Status != "interrupted" {
		return nil
	}
	payload := map[string]any{
		"safe":               true,
		"hold_confirmed":     true,
		"managed_simulation": true,
		"reason":             reason,
	}
	if len(execution.Error) != 0 {
		payload["pre_stop_error"] = cloneMap(execution.Error)
	}
	execution.Status = "stopped"
	execution.Result = cloneMap(payload)
	execution.Error = nil
	execution.Revision++
	execution.UpdatedAt = s.now().UTC()
	if err := s.st.SaveRobotExecution(execution); err != nil {
		return err
	}
	s.publishExecution(execution, "robot.execution.stopped", payload)
	if pilot, pilotErr := s.st.GetActiveRobotPilot(robotID); pilotErr == nil &&
		pilot.CurrentExecutionID == executionID {
		s.setPilotExecution(pilot, "")
	}
	if s.observer != nil {
		return s.observer.OnRobotExecutionChanged(
			ctx, execution, "robot.execution.stopped", cloneMap(payload),
		)
	}
	return nil
}

// ConfirmOperatorStop 只接受 Workflow 已经判断为“执行状态未知”的人工恢复请求。
// expected 精确携带 Workflow/SubTask 当前引用；若旧版本遗留数据缺少 Execution
// 记录，也会补建一条 stopped 审计记录，而不是要求用户删除数据库中的任务。
// 这个入口不会向 Pilot 下发动作，调用方必须先取得现场安全确认和非空原因。
func (s *Service) ConfirmOperatorStop(
	ctx context.Context, expected store.RobotExecution, userID, reason string,
) (store.RobotExecution, error) {
	expected.ID = strings.TrimSpace(expected.ID)
	expected.ProjectID = strings.TrimSpace(expected.ProjectID)
	expected.RobotID = strings.TrimSpace(expected.RobotID)
	userID = strings.TrimSpace(userID)
	reason = strings.TrimSpace(reason)
	if expected.ID == "" || expected.ProjectID == "" || expected.RobotID == "" ||
		userID == "" || reason == "" {
		return store.RobotExecution{}, store.ErrInvalidState
	}

	now := s.now().UTC()
	execution, err := s.st.GetRobotExecution(expected.ID)
	if errors.Is(err, store.ErrNotFound) {
		// execution_ref 已经进入 Task 审计链后不能被替换。对缺失记录使用同一个
		// ID 补建最小恢复事实，使后续历史查看仍能解释是谁、何时、为何释放了
		// Robot，而不是只把 Task 强行改成 stopped。
		execution = expected
		execution.Status = "stopped"
		execution.RequestKey = "operator-confirm-stop:" + expected.ID
		execution.Revision = 1
		execution.CreatedAt = now
	} else if err != nil {
		return store.RobotExecution{}, err
	} else {
		if execution.ProjectID != expected.ProjectID || execution.RobotID != expected.RobotID ||
			(expected.WorkflowID != "" && execution.WorkflowID != expected.WorkflowID) ||
			(expected.TaskID != "" && execution.TaskID != expected.TaskID) ||
			(expected.SubtaskID != "" && execution.SubtaskID != expected.SubtaskID) {
			return store.RobotExecution{}, store.ErrNotFound
		}
		if execution.Status == "stopped" && execution.Result["safe"] == true &&
			execution.Result["confirmation_source"] == "operator" {
			return execution, nil
		}
		// 正常活动执行不能通过人工确认跳过 robot.stop。仅 stopping 以及已经
		// 失去可靠运行状态的终态/中断态可由上层 Workflow 恢复流程收敛。
		switch execution.Status {
		case "stopping", "interrupted", "failed", "stopped", "cancelled":
		default:
			return store.RobotExecution{}, store.ErrInvalidState
		}
		execution.Revision++
	}

	payload := map[string]any{
		"safe":                true,
		"hold_confirmed":      true,
		"confirmation_source": "operator",
		"confirmed_by":        userID,
		"confirmed_at":        now.Format(time.RFC3339Nano),
		"confirmation_reason": reason,
	}
	if len(execution.Error) != 0 {
		payload["pre_stop_error"] = cloneMap(execution.Error)
	}
	execution.Status = "stopped"
	execution.Result = cloneMap(payload)
	execution.Error = nil
	execution.UpdatedAt = now
	if err := s.st.SaveRobotExecution(execution); err != nil {
		return store.RobotExecution{}, err
	}
	s.publishExecution(execution, "robot.execution.stop_confirmed_by_operator", payload)

	// 只释放仍指向同一 execution_id 的锁。Robot 若已开始新执行，人工恢复
	// 旧 Workflow 绝不能把新执行误标为空闲。
	if pilot, pilotErr := s.st.GetActiveRobotPilot(execution.RobotID); pilotErr == nil &&
		pilot.CurrentExecutionID == execution.ID {
		s.setPilotExecution(pilot, "")
	}
	if s.observer != nil {
		if err := s.observer.OnRobotExecutionChanged(
			ctx, execution, "robot.execution.stop_confirmed_by_operator", cloneMap(payload),
		); err != nil {
			return execution, err
		}
	}
	return execution, nil
}

func (s *Service) ReplyAgentRequest(executionID string, payload map[string]any) error {
	execution, err := s.st.GetRobotExecution(executionID)
	if err != nil {
		return err
	}
	_, err = s.sendCommand(execution.PilotInstanceID, "agent.reply", payload)
	return err
}

func (s *Service) HandlePilotEvent(pilotID, typ string, sequence int64, payload map[string]any) error {
	switch typ {
	case "heartbeat":
		var pilot store.RobotPilot
		data, _ := json.Marshal(payload)
		if err := json.Unmarshal(data, &pilot); err != nil {
			return err
		}
		return s.Heartbeat(pilotID, pilot)
	case "skill.status":
		var item store.RobotPilotSkill
		data, _ := json.Marshal(payload)
		if err := json.Unmarshal(data, &item); err != nil {
			return err
		}
		item.PilotInstanceID = pilotID
		item.UpdatedAt = s.now().UTC()
		if err := s.st.SaveRobotPilotSkill(item); err != nil {
			return err
		}
		pilot, err := s.st.GetRobotPilot(pilotID)
		if err != nil {
			return err
		}
		pilot.SkillCatalogRevision++
		pilot.Revision++
		pilot.LastSeenAt = s.now().UTC()
		if err := s.st.SaveRobotPilot(pilot); err != nil {
			return err
		}
		s.mu.Lock()
		if session := s.sessions[pilotID]; session != nil {
			session.pilot = pilot
		}
		s.mu.Unlock()
		eventType := "skill." + item.Status
		if item.Status == "installed" && item.Enabled {
			eventType = "skill.enabled"
		}
		s.publishPilotView(pilot, eventType, "robot")
		if item.Status == "installed" && item.Enabled {
			s.notifyRobotAvailable(pilot)
		}
		go func() { _ = s.ReconcileDesiredSkills(pilot.RobotID) }()
		return nil
	case "artifact.announce":
		return s.announceArtifact(pilotID, payload)
	case "ability.debug.status":
		debug, ok := payload["debug"].(map[string]any)
		if !ok {
			return errors.New("ability.debug.status 缺少 debug")
		}
		pilot, err := s.st.GetRobotPilot(pilotID)
		if err != nil {
			return err
		}
		if robotID := stringValue(debug["robot_id"]); robotID != "" && robotID != pilot.RobotID {
			return errors.New("Ability debug 不属于当前 Pilot Robot")
		}
		id := stringValue(debug["id"])
		status := stringValue(debug["status"])
		if id == "" || status == "" {
			return errors.New("Ability debug 状态缺少 id 或 status")
		}
		revision := int64(numberValue(debug["revision"]))
		if revision <= 0 {
			revision = 1
		}
		s.publishDevice("ability_debug", id, "ability_debug."+status, revision, map[string]any{"debug": debug})
		return nil
	}
	executionID, _ := payload["execution_id"].(string)
	if executionID == "" {
		return fmt.Errorf("Pilot 事件 %s 缺少 execution_id", typ)
	}
	execution, err := s.st.GetRobotExecution(executionID)
	if err != nil {
		return err
	}
	if execution.PilotInstanceID != pilotID {
		return errors.New("Execution 不属于当前 Pilot")
	}
	if sequence <= 0 {
		sequence, _ = s.st.NextRobotExecutionEventSequence(executionID)
	}
	eventTime := s.now().UTC()
	if raw := stringValue(payload["occurred_at"]); raw != "" {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, raw); parseErr == nil {
			eventTime = parsed.UTC()
		}
	}
	event := store.RobotExecutionEvent{ExecutionID: executionID, Sequence: sequence, Type: typ, Payload: cloneMap(payload), CreatedAt: eventTime}
	if err := s.st.AppendRobotExecutionEvent(event); err != nil {
		return err
	}
	previousStatus := execution.Status
	// skill_status 是所有 Pilot 事件附带的快照，并非生命周期命令。
	// artifact.summary.announced 可能先携带 completed，却不包含 Skill result；
	// 只有正式生命周期或 Agent 等待事件可以推进 Execution，不能据快照提前
	// 释放 Robot 锁或完成 SubTask。Action/Stage 原始状态仍完整保留在事件流。
	if typ == "agent.requested" {
		// agent.requested 是 Worker 主动到达安全 checkpoint 的领域事件，不是
		// execution.* 生命周期事件；仍需将持久状态切到 waiting_agent，Server
		// 才能在重连后恢复同一个请求，而不是误以为 Skill 仍在运行。
		execution.Status = "waiting_agent"
	} else if typ == "agent.resolved" {
		execution.Status = "running"
	} else if executionLifecycleEvent(typ) {
		status := stringValue(payload["skill_status"])
		if status == "" {
			status = stringValue(payload["status"])
		}
		if status != "" {
			execution.Status = status
		}
	}
	if !activeRobotStatus(previousStatus) && activeRobotStatus(execution.Status) {
		// Robot Execution 的终态只能由新的显式执行重建，不能被迟到的 Worker
		// 进程通知回退。否则真实 Skill 已完成后，设备页仍会永久显示 running，
		// 并继续占用 Robot 的调试锁。
		execution.Status = previousStatus
	}
	if previousStatus == "stopping" && execution.Status != "stopping" &&
		activeRobotStatus(execution.Status) {
		// stopping 是 Server 已经接受停止意图后的单向边界。Pilot 在停止命令前
		// 产生的 queued/running/waiting_agent 事件可能迟到，但这些事件只描述
		// 旧执行进度，不能撤销停止意图；后续只允许正式终态或 interrupted
		// 推进该 Execution，避免 Workflow 永久失去停止入口。
		execution.Status = previousStatus
	}
	if stage, ok := payload["stage"].(string); ok {
		execution.Stage = stage
	}
	if progress, ok := payload["progress"].(float64); ok {
		execution.Progress = &progress
	}
	if execution.Status == "completed" {
		// Action Feedback 的 progress 只描述当前一次 Ability 调用，不能作为整个
		// Robot Skill 的最终进度。Skill 已经给出 completed 终态时由 Server
		// 收敛为 100%，避免最后一个 Action 的局部进度残留在设备页。
		completedProgress := 1.0
		execution.Progress = &completedProgress
	}
	if executionLifecycleEvent(typ) {
		// Action 的 result/error 只属于那次 Ability 调用，不是 Skill 的业务结果。
		// 尤其不能让末次采图或迟到 Action 覆盖 ctx.complete 的正式结果。
		if result, ok := payload["result"].(map[string]any); ok {
			execution.Result = cloneMap(result)
		}
		if failure, ok := payload["error"].(map[string]any); ok {
			execution.Error = cloneMap(failure)
		}
	}
	if typ == "skill.stop.finalized" {
		if outcome, ok := payload["stop_outcome"].(map[string]any); ok {
			// Worker 的 stop.outcome 是物理安全证据。把它收进 Execution result，
			// Workflow、场景 reset 和实例 supervisor 才能复用同一份事实。
			execution.Result = cloneMap(outcome)
		}
	}
	if execution.Status == "completed" {
		// Action 失败可能已由 Skill 在后续阶段恢复；完整诊断仍保存在事件流中。
		// 正式 completed 表示当前 Execution 已成功终结，不能继续携带旧的顶层
		// error，否则历史页会把已完成技能误显示成失败。
		execution.Error = nil
	}
	execution.Revision++
	execution.UpdatedAt = s.now().UTC()
	if err := s.st.SaveRobotExecution(execution); err != nil {
		return err
	}
	if execution.Status == "interrupted" {
		// interrupted 表示 Pilot/Ability/SDK 无法确认物理状态。此时必须保留
		// CurrentExecutionID 和 Robot 锁，禁止另一个 Task 抢占同一台 Robot。
		if pilot, pilotErr := s.st.GetRobotPilot(pilotID); pilotErr == nil {
			s.setPilotInterrupted(pilot, execution.ID)
		}
	} else if !activeRobotStatus(execution.Status) {
		if pilot, err := s.st.GetRobotPilot(pilotID); err == nil {
			s.setPilotExecution(pilot, "")
		}
	}
	eventPayload := cloneMap(payload)
	eventPayload["sequence"] = sequence
	s.publishExecution(execution, typ, eventPayload)
	if s.observer != nil {
		return s.observer.OnRobotExecutionChanged(context.Background(), execution, typ, eventPayload)
	}
	return nil
}

func executionLifecycleEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "execution.") ||
		eventType == "skill.started" || eventType == "skill.stop.finalized"
}

func (s *Service) recoverPersistedExecutionTerminal(execution store.RobotExecution) (
	store.RobotExecution, map[string]any, bool, error,
) {
	events, err := s.st.ListRobotExecutionEvents(execution.ID, 0, 1000)
	if err != nil {
		return execution, nil, false, err
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != "execution.terminal" {
			continue
		}
		status := stringValue(event.Payload["skill_status"])
		if status == "" {
			status = stringValue(event.Payload["status"])
		}
		if status == "" || activeRobotStatus(status) {
			continue
		}
		if execution.Status == status {
			// 投影已经与正式终态一致时无需重复写库和发布事件。Reconcile 会在
			// Pilot 每次连接时执行，这个判断避免把正常历史记录当成待迁移数据。
			return execution, nil, false, nil
		}
		// Pilot 重连时只恢复已经持久化的正式 Execution 终态。Worker complete/fail
		// 是进程返回通知，不具备推翻该事实的权限；这也让旧版本遗留的
		// terminal→running 投影能够在升级后自动收敛。
		execution.Status = status
		if stage := stringValue(event.Payload["stage"]); stage != "" {
			execution.Stage = stage
		}
		if result, ok := event.Payload["result"].(map[string]any); ok {
			execution.Result = cloneMap(result)
		}
		if failure, ok := event.Payload["error"].(map[string]any); ok {
			execution.Error = cloneMap(failure)
		}
		if status == "completed" {
			progress := 1.0
			execution.Progress = &progress
			execution.Error = nil
		}
		execution.Revision++
		execution.UpdatedAt = s.now().UTC()
		if err := s.st.SaveRobotExecution(execution); err != nil {
			return execution, nil, false, err
		}
		payload := cloneMap(event.Payload)
		payload["sequence"] = event.Sequence
		return execution, payload, true, nil
	}
	return execution, nil, false, nil
}

func (s *Service) Reconcile(pilotID string, remote []map[string]any) ([]store.RobotExecution, error) {
	pilot, err := s.st.GetRobotPilot(pilotID)
	if err != nil {
		return nil, err
	}
	local, err := s.st.ListRobotExecutions("", pilot.RobotID, 100)
	if err != nil {
		return nil, err
	}
	remoteByID := make(map[string]map[string]any, len(remote))
	for _, item := range remote {
		if id, _ := item["execution_id"].(string); id != "" {
			remoteByID[id] = item
		}
	}
	recoverable := make([]store.RobotExecution, 0, len(remoteByID))
	terminalCurrentExecutionID := ""
	for index := range local {
		if local[index].Status != "completed" {
			recovered, payload, restored, recoverErr := s.recoverPersistedExecutionTerminal(local[index])
			if recoverErr != nil {
				return nil, recoverErr
			}
			if restored {
				local[index] = recovered
				// 旧版本可能在正式 completed 之后又把 Worker 的进程 complete
				// 或 shutdown stop 写进投影。正式 execution.terminal 是 Skill
				// 的领域事实，升级后应在一次 Pilot 重连内修正历史；只有该记录
				// 仍占用当前 Robot 时才清理指针，不能影响另一个活动 Execution。
				if pilot.CurrentExecutionID == recovered.ID {
					if recovered.Status == "interrupted" {
						s.setPilotInterrupted(pilot, recovered.ID)
					} else {
						terminalCurrentExecutionID = recovered.ID
					}
				}
				s.publishExecution(recovered, "robot.execution.reconciled", payload)
				if s.observer != nil {
					if observeErr := s.observer.OnRobotExecutionChanged(context.Background(), recovered,
						"execution.terminal", payload); observeErr != nil {
						return nil, observeErr
					}
				}
			}
		}
		// CurrentExecutionID 是 Robot 的互斥占用指针，不是 Execution 状态的
		// 第二份真相。Pilot 完成重连对账后，如果该指针仍指向 Server 已确认的
		// 普通终态，就应释放它，否则 ready Task 会永久停在 waiting_resource。
		// interrupted 代表物理状态未知，必须继续保留锁，禁止自动重放或改派。
		if pilot.CurrentExecutionID == local[index].ID &&
			!activeRobotStatus(local[index].Status) && local[index].Status != "interrupted" {
			terminalCurrentExecutionID = local[index].ID
		}

		if !activeRobotStatus(local[index].Status) {
			continue
		}
		remoteItem, exists := remoteByID[local[index].ID]
		if !exists {
			local[index].Status = "interrupted"
			local[index].Revision++
			local[index].UpdatedAt = s.now().UTC()
			local[index].Error = map[string]any{"code": "PILOT_EXECUTION_MISSING", "message": "Pilot 重连后没有该活动 Execution"}
			_ = s.st.SaveRobotExecution(local[index])
			s.publishExecution(local[index], "robot.execution.interrupted", local[index].Error)
			s.setPilotInterrupted(pilot, local[index].ID)
			if s.observer != nil {
				if observeErr := s.observer.OnRobotExecutionChanged(context.Background(), local[index],
					"robot.execution.interrupted", cloneMap(local[index].Error)); observeErr != nil {
					return nil, observeErr
				}
			}
			continue
		}
		if status, _ := remoteItem["status"].(string); status != "" && status != local[index].Status {
			local[index].Status = status
			local[index].Revision++
			local[index].UpdatedAt = s.now().UTC()
			_ = s.st.SaveRobotExecution(local[index])
		}
		if activeRobotStatus(local[index].Status) {
			recoverable = append(recoverable, local[index])
		}
	}
	// Pilot 有而 Server 无的执行不会被重建或重放；这里只返回双方仍需恢复的
	// 活动执行。历史终态由 Server 持久化即可，不应在每次重连时整批下发。
	if terminalCurrentExecutionID != "" {
		latestPilot, getErr := s.st.GetRobotPilot(pilotID)
		if getErr != nil {
			return nil, getErr
		}
		// 对账期间可能已有新 Execution 取得 Robot；只有占用指针仍未变化时
		// 才释放旧终态，避免清掉新任务刚建立的互斥锁。
		if latestPilot.CurrentExecutionID == terminalCurrentExecutionID {
			s.setPilotExecution(latestPilot, "")
		}
	}

	return recoverable, nil
}

func (s *Service) InstallSkill(pilotID, name, version string) error {
	item, err := s.st.GetRobotSkillPackage(name, version)
	if err != nil {
		return err
	}
	token := s.addTransfer(Transfer{Mode: "package_download", PilotInstanceID: pilotID, PackagePath: item.PackagePath, ExpiresAt: s.now().Add(30 * time.Minute)})
	_, sent, err := s.sendSkillCommandOnce(pilotID, "skill.install", map[string]any{"name": name, "version": version, "package_url": "/pilot/v1/transfers/" + token})
	if !sent {
		s.mu.Lock()
		delete(s.transfers, token)
		s.mu.Unlock()
	}
	return err
}
func (s *Service) SetSkillEnabled(pilotID, name, version string, enabled bool) error {
	typ := "skill.disable"
	if enabled {
		typ = "skill.enable"
	}
	_, _, err := s.sendSkillCommandOnce(pilotID, typ, map[string]any{"name": name, "version": version})
	return err
}
func (s *Service) UninstallSkill(pilotID, name, version string) error {
	_, _, err := s.sendSkillCommandOnce(pilotID, "skill.uninstall", map[string]any{"name": name, "version": version})
	return err
}

func (s *Service) StartAbilityDebug(robotID, instanceID, taskName string, input map[string]any) (map[string]any, error) {
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if err != nil {
		return nil, err
	}
	if !s.IsOnline(pilot.PilotInstanceID) {
		return nil, ErrPilotOffline
	}
	if pilot.CurrentExecutionID != "" || pilot.RobotStatus == "busy" || pilot.RobotStatus == "stopping" {
		return nil, ErrRobotBusy
	}
	found, taskAllowed := false, false
	for _, ability := range pilot.Abilities {
		if stringValue(ability["instance_id"]) == instanceID {
			found = true
			for _, task := range stringList(ability["tasks"]) {
				if task == taskName && task != "GetExecution" && task != "StopExecution" {
					taskAllowed = true
					break
				}
			}
			break
		}
	}
	if !found {
		return nil, store.ErrNotFound
	}
	if !taskAllowed {
		return nil, ErrAbilityTaskUnknown
	}
	debugID := "debug-" + uuid.NewString()
	debug := map[string]any{"id": debugID, "robot_id": robotID, "ability_instance_id": instanceID,
		"task_name": taskName, "status": "starting", "input": cloneMap(input), "revision": int64(1), "started_at": s.now().UTC()}
	if _, err := s.sendCommand(pilot.PilotInstanceID, "ability.debug.start", debug); err != nil {
		return nil, err
	}
	s.publishDevice("ability_debug", debugID, "ability_debug.started", 1, map[string]any{"debug": debug})
	return debug, nil
}

func (s *Service) StopAbilityDebug(robotID, debugID string) (map[string]any, error) {
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if err != nil {
		return nil, err
	}
	if !s.IsOnline(pilot.PilotInstanceID) {
		return nil, ErrPilotOffline
	}
	debug := map[string]any{"id": debugID, "robot_id": robotID, "status": "stopping", "revision": int64(2)}
	if _, err := s.sendCommand(pilot.PilotInstanceID, "ability.debug.stop", map[string]any{"debug_id": debugID}); err != nil {
		return nil, err
	}
	s.publishDevice("ability_debug", debugID, "ability_debug.stopping", 2, map[string]any{"debug": debug})
	return debug, nil
}

func (s *Service) PublishSkillArchive(reader io.Reader) (store.RobotSkillPackage, error) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	root, err := os.MkdirTemp("", "robot-skill-publish-")
	if err != nil {
		return store.RobotSkillPackage{}, err
	}
	defer os.RemoveAll(root)
	archive := filepath.Join(root, "package.zip")
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return store.RobotSkillPackage{}, err
	}
	written, copyErr := io.Copy(file, io.LimitReader(reader, maxSkillPackageBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return store.RobotSkillPackage{}, copyErr
	}
	if closeErr != nil {
		return store.RobotSkillPackage{}, closeErr
	}
	if written > maxSkillPackageBytes {
		return store.RobotSkillPackage{}, errors.New("Robot Skill 包超过 100MB")
	}
	extracted := filepath.Join(root, "content")
	if err := extractSkillArchive(archive, extracted); err != nil {
		return store.RobotSkillPackage{}, err
	}
	manifest, err := findSkillManifest(extracted)
	if err != nil {
		return store.RobotSkillPackage{}, err
	}
	parsed, err := skill.LoadFile(manifest)
	if err != nil {
		return store.RobotSkillPackage{}, err
	}
	if parsed.Category != "robot_skill" {
		return store.RobotSkillPackage{}, errors.New("SKILL.md category 必须为 robot_skill")
	}
	version, _ := parsed.Extensions["version"].(string)
	if version == "" {
		return store.RobotSkillPackage{}, errors.New("Robot Skill SKILL.md 缺少 version")
	}
	required := actionList(parsed.Extensions["required_actions"])
	stop := actionList(parsed.Extensions["stop_actions"])
	if len(required) == 0 || len(stop) == 0 {
		return store.RobotSkillPackage{}, errors.New("Robot Skill 缺少 required_actions 或 stop_actions")
	}
	target := filepath.Join(s.st.RobotSkillPackageDir(), safeSegment(parsed.Name), safeSegment(version), "package.zip")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return store.RobotSkillPackage{}, err
	}
	content, err := os.ReadFile(archive)
	if err != nil {
		return store.RobotSkillPackage{}, err
	}
	// 同一版本可以重复导入同一归档；修改后的源码需产生新版本，避免安装记录
	// 仍指向旧版本号而实际包内容已经变化。此检查先于任何已发布文件写入。
	if old, err := s.st.GetRobotSkillPackage(parsed.Name, version); err == nil {
		previous, err := os.ReadFile(old.PackagePath)
		if err != nil {
			return store.RobotSkillPackage{}, err
		}
		if !sameSkillArchive(previous, content) {
			return store.RobotSkillPackage{}, fmt.Errorf("Robot Skill %s@%s 已发布不同内容，请更新版本后导入", parsed.Name, version)
		}
		return old, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.RobotSkillPackage{}, err
	}
	if err := os.WriteFile(target, content, 0o640); err != nil {
		return store.RobotSkillPackage{}, err
	}
	item := store.RobotSkillPackage{Name: parsed.Name, Version: version, Description: parsed.Description, Category: parsed.Category,
		PackagePath: target, RequiredActions: required, StopActions: stop, PublishedAt: s.now().UTC()}
	if values, ok := parsed.Extensions["applicable_models"].([]any); ok {
		for _, value := range values {
			if text, ok := value.(string); ok {
				item.ApplicableModels = append(item.ApplicableModels, text)
			}
		}
	}
	return item, s.st.SaveRobotSkillPackage(item)
}

func extractSkillArchive(path, destination string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	for _, entry := range reader.File {
		name := filepath.Clean(filepath.FromSlash(entry.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return ErrUnsafePackage
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePackage
		}
		target := filepath.Join(destination, name)
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if !entry.Mode().IsRegular() {
			return ErrUnsafePackage
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			source.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(source, maxSkillPackageBytes+1))
		closeErr := output.Close()
		source.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func findSkillManifest(root string) (string, error) {
	var result string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafePackage
		}
		if !entry.IsDir() && entry.Name() == "SKILL.md" {
			if result != "" {
				return errors.New("Robot Skill 包只能包含一个 SKILL.md")
			}
			result = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if result == "" {
		return "", errors.New("Robot Skill 包缺少 SKILL.md")
	}
	return result, nil
}
func actionList(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if action, ok := item.(map[string]any); ok {
			if action["type"] != "" {
				result = append(result, action)
			}
		}
	}
	return result
}
func safeSegment(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, "\\", "_")
	return value
}

func (s *Service) setPilotExecution(pilot store.RobotPilot, id string) {
	pilot.CurrentExecutionID = id
	if id == "" {
		pilot.RobotStatus = "idle"
	} else {
		pilot.RobotStatus = "busy"
	}
	pilot.LastSeenAt = s.now().UTC()
	pilot.Revision++
	_ = s.st.SaveRobotPilot(pilot)
	s.mu.Lock()
	if session := s.sessions[pilot.PilotInstanceID]; session != nil {
		session.pilot = pilot
	}
	s.mu.Unlock()
	eventType := "robot.idle"
	if id != "" {
		eventType = "robot.busy"
	}
	s.publishPilotView(pilot, eventType, "robot")
	if id == "" {
		s.notifyRobotAvailable(pilot)
	}
}

func robotAvailable(pilot store.RobotPilot) bool {
	return pilot.Status == "online" && pilot.RobotStatus == "idle" &&
		pilot.CurrentExecutionID == "" && pilot.AbilityFrameworkStatus == "ready"
}

func (s *Service) notifyRobotAvailable(pilot store.RobotPilot) {
	if !robotAvailable(pilot) || s.availability == nil {
		return
	}
	// 这里只发送一个资源变化提示，不在 Robot Service 内挑选 Task。真正分配
	// 时 Workflow 仍会读取最新 Pilot、Ability 与 Skill actual 目录，并通过
	// Store 的原子条件更新取得 Robot 保留，避免心跳与调度形成双重真相。
	observer := s.availability
	robotID := pilot.RobotID
	go observer.OnRobotAvailabilityChanged(context.Background(), robotID)
}

func (s *Service) setPilotInterrupted(pilot store.RobotPilot, executionID string) {
	pilot.CurrentExecutionID = executionID
	pilot.RobotStatus = "interrupted"
	pilot.LastSeenAt = s.now().UTC()
	pilot.Revision++
	_ = s.st.SaveRobotPilot(pilot)
	s.mu.Lock()
	if session := s.sessions[pilot.PilotInstanceID]; session != nil {
		session.pilot = pilot
	}
	s.mu.Unlock()
	s.publishPilotView(pilot, "robot.interrupted", "robot")
}
func (s *Service) publishExecution(execution store.RobotExecution, typ string, payload any) {
	sequence := int64(0)
	if values, ok := payload.(map[string]any); ok {
		sequence = int64(numberValue(values["sequence"]))
	}
	event := map[string]any{"type": typ, "sequence": sequence, "payload": payload, "created_at": s.now().UTC()}
	projectPayload := map[string]any{"execution": execution, "event": event}
	s.events.PublishRobotEvent(execution.ProjectID, "robot_execution", execution.ID, typ, execution.Revision, projectPayload)
	s.publishDevice("robot_execution", execution.ID, typ, execution.Revision, map[string]any{"execution": execution})
}
func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	body, _ := json.Marshal(source)
	var result map[string]any
	_ = json.Unmarshal(body, &result)
	return result
}

func transferToken() string {
	buffer := make([]byte, 24)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)
}
func (s *Service) addTransfer(transfer Transfer) string {
	if transfer.Token == "" {
		transfer.Token = transferToken()
	}
	s.mu.Lock()
	s.transfers[transfer.Token] = transfer
	s.mu.Unlock()
	return transfer.Token
}
func (s *Service) takeTransfer(token, mode string) (Transfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.transfers[token]
	if !ok || item.Mode != mode || s.now().After(item.ExpiresAt) {
		delete(s.transfers, token)
		return Transfer{}, ErrTransferUnavailable
	}
	if mode == "artifact_upload" {
		delete(s.transfers, token)
	}
	return item, nil
}

func (s *Service) authorizeArtifactDownloads(execution store.RobotExecution) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(execution.ArtifactRefs))
	for _, ref := range execution.ArtifactRefs {
		id := strings.TrimPrefix(ref, "artifact://")
		if id == ref {
			return nil, fmt.Errorf("非法 ArtifactRef %s", ref)
		}
		if _, err := s.st.GetArtifactMeta(id); err != nil {
			return nil, err
		}
		token := s.addTransfer(Transfer{Mode: "artifact_download", PilotInstanceID: execution.PilotInstanceID, ExecutionID: execution.ID, ArtifactID: id, ExpiresAt: s.now().Add(30 * time.Minute)})
		result = append(result, map[string]any{"ref": ref, "url": "/pilot/v1/transfers/" + token})
	}
	return result, nil
}

func (s *Service) announceArtifact(pilotID string, payload map[string]any) error {
	executionID, _ := payload["execution_id"].(string)
	localID, _ := payload["local_artifact_id"].(string)
	if executionID == "" || localID == "" {
		return errors.New("artifact.announce 缺少 execution_id 或 local_artifact_id")
	}
	execution, err := s.st.GetRobotExecution(executionID)
	if err != nil {
		return err
	}
	if execution.PilotInstanceID != pilotID {
		return errors.New("Artifact 不属于当前 Pilot")
	}
	item := store.RobotArtifactMapping{PilotInstanceID: pilotID, LocalArtifactID: localID, ExecutionID: executionID,
		ActionID: stringValue(payload["action_id"]), ObservationID: stringValue(payload["observation_id"]), MediaType: stringValue(payload["media_type"]), Summary: stringValue(payload["summary"]), SizeBytes: int64(numberValue(payload["size_bytes"])), Status: "pending", UpdatedAt: s.now().UTC()}
	if err := s.st.SaveRobotArtifactMapping(item); err != nil {
		return err
	}
	// Artifact mappings own upload state. Never save this lifecycle snapshot:
	// a terminal event may already have advanced the same Execution concurrently.
	pending := artifactSyncView(item)
	s.publishDevice("artifact_sync", pilotID+"/"+localID, "artifact_sync.announced", 1, map[string]any{"artifact_sync": pending})
	s.events.PublishRobotEvent(execution.ProjectID, "artifact_sync", localID, "artifact_sync.announced", execution.Revision, map[string]any{"artifact_sync": pending})
	token := s.addTransfer(Transfer{Mode: "artifact_upload", PilotInstanceID: pilotID, ExecutionID: executionID, LocalArtifactID: localID, MediaType: item.MediaType, Summary: item.Summary, ExpiresAt: s.now().Add(30 * time.Minute)})
	_, err = s.sendCommand(pilotID, "artifact.upload", map[string]any{"execution_id": executionID, "local_artifact_id": localID, "upload_url": "/pilot/v1/transfers/" + token})
	return err
}

func stringValue(value any) string { result, _ := value.(string); return result }

func stringList(value any) []string {
	items, ok := value.([]string)
	if ok {
		return items
	}
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok && text != "" {
			result = append(result, text)
		}
	}
	return result
}

func artifactSyncView(item store.RobotArtifactMapping) map[string]any {
	view := map[string]any{"pilot_instance_id": item.PilotInstanceID, "local_artifact_id": item.LocalArtifactID,
		"server_artifact_id": item.ServerArtifactID, "execution_id": item.ExecutionID, "media_type": item.MediaType,
		"summary": item.Summary, "size_bytes": item.SizeBytes, "status": item.Status, "updated_at": item.UpdatedAt}
	if item.ServerArtifactID != "" {
		view["ref"] = "artifact://" + item.ServerArtifactID
	}
	return view
}

func numberValue(value any) float64 {
	switch item := value.(type) {
	case float64:
		return item
	case int:
		return float64(item)
	case int64:
		return float64(item)
	}
	return 0
}

// TransferHandler 流式传输包和 Artifact。token 同时限定方向、Pilot、Execution
// 和对象，控制 WebSocket 因此只传元数据。
func (s *Service) TransferHandler(w http.ResponseWriter, r *http.Request, pilotID string) {
	token := strings.TrimPrefix(r.URL.Path, "/pilot/v1/transfers/")
	if token == "" {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	boundTransfer, exists := s.transfers[token]
	s.mu.RUnlock()
	if !exists || boundTransfer.PilotInstanceID != pilotID || s.now().After(boundTransfer.ExpiresAt) {
		http.Error(w, ErrTransferUnavailable.Error(), http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		transfer, ok := s.transfers[token]
		s.mu.RUnlock()
		if !ok || s.now().After(transfer.ExpiresAt) || (transfer.Mode != "package_download" && transfer.Mode != "artifact_download") {
			http.Error(w, ErrTransferUnavailable.Error(), http.StatusNotFound)
			return
		}
		if transfer.Mode == "package_download" {
			http.ServeFile(w, r, transfer.PackagePath)
			return
		}
		artifact, content, err := s.st.OpenArtifactContent(transfer.ArtifactID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer content.Close()
		w.Header().Set("Content-Type", artifact.MediaType)
		w.Header().Set("X-Artifact-Ref", "artifact://"+artifact.ID)
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, content)
	case http.MethodPut:
		transfer, err := s.takeTransfer(token, "artifact_upload")
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		execution, err := s.st.GetRobotExecution(transfer.ExecutionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		project, err := s.st.GetProject(execution.ProjectID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.ContentLength > maxArtifactUploadBytes {
			http.Error(w, "Artifact 超过大小限制", http.StatusRequestEntityTooLarge)
			return
		}
		artifact, err := s.st.PutUserArtifactStream(project.OwnerID, transfer.MediaType, transfer.Summary, `{"source":"pilot"}`, r.Body, maxArtifactUploadBytes)
		if err != nil {
			http.Error(w, "Artifact 上传失败或超过限制", http.StatusRequestEntityTooLarge)
			return
		}
		mapping, err := s.st.GetRobotArtifactMapping(transfer.PilotInstanceID, transfer.LocalArtifactID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mapping.ServerArtifactID = artifact.ID
		mapping.Status = "synced"
		mapping.SizeBytes = artifact.Size
		mapping.UpdatedAt = s.now().UTC()
		if err := s.st.SaveRobotArtifactMapping(mapping); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Reading the request body can overlap Skill completion. The upload must
		// not write back the pre-upload running snapshot or lose another upload's
		// refs. Re-read the lifecycle plus its authoritative mapping projection for
		// the sync event; GetRobotExecution merges every synced reference.
		execution, err = s.st.GetRobotExecution(transfer.ExecutionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		response := map[string]any{"local_ref": "pilot-artifact://" + transfer.PilotInstanceID + "/" + transfer.LocalArtifactID, "server_ref": "artifact://" + artifact.ID, "sync_status": "synced"}
		syncView := artifactSyncView(mapping)
		s.publishDevice("artifact_sync", transfer.PilotInstanceID+"/"+transfer.LocalArtifactID, "artifact_sync.synced", 2, map[string]any{"artifact_sync": syncView})
		s.events.PublishRobotEvent(execution.ProjectID, "artifact_sync", artifact.ID, "artifact_sync.synced", execution.Revision, map[string]any{"artifact_sync": syncView})
		s.publishExecution(execution, "robot.artifact.synced", map[string]any{"artifact_sync": syncView})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
