package pilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/websocket"

	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/pkg/log"
)

const pilotServerReadLimit = 2 << 20

type RemoteClientConfig struct {
	ServerWebSocketURL string
	ServerHTTPBaseURL  string
	AccessToken        string
	Pilot              store.RobotPilot
	ReconnectDelay     time.Duration
	HeartbeatInterval  time.Duration
	AbilityStatus      func() AbilityDiscoverySnapshot
}

type RemoteClient struct {
	config     RemoteClientConfig
	runtime    *SkillRuntime
	executions SkillExecutionStore
	manager    *InstalledSkillManager
	artifacts  *ArtifactStore
	agent      *RemoteAgentGateway
	logger     *log.Logger
	debug      *AbilityDebugService

	mu   sync.Mutex
	conn *websocket.Conn
}

type serverCommand struct {
	Type      string          `json:"type"`
	CommandID string          `json:"command_id"`
	Payload   json.RawMessage `json:"payload"`
}

func NewRemoteClient(config RemoteClientConfig, runtime *SkillRuntime, executions SkillExecutionStore,
	manager *InstalledSkillManager, artifacts *ArtifactStore, agent *RemoteAgentGateway, logger *log.Logger) *RemoteClient {
	if config.ReconnectDelay <= 0 {
		config.ReconnectDelay = time.Second
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 2 * time.Second
	}
	client := &RemoteClient{config: config, runtime: runtime, executions: executions, manager: manager, artifacts: artifacts, agent: agent, logger: logger}
	if artifacts != nil {
		artifacts.PilotInstanceID = config.Pilot.PilotInstanceID
		artifacts.AccessToken = config.AccessToken
		artifacts.ServerBaseURL = config.ServerHTTPBaseURL
		artifacts.Announce = func(execution SkillExecution, item localArtifact) error {
			return client.SendEvent("artifact.announce", map[string]any{"execution_id": execution.ID, "local_artifact_id": item.LocalID, "media_type": item.MediaType, "summary": item.Summary, "size_bytes": item.SizeBytes})
		}
		runtime.SetArtifactService(artifacts)
	}
	if agent != nil {
		agent.Report = func(request AgentRequest) error {
			return client.SendEvent("agent.requested", map[string]any{"execution_id": request.ExecutionID, "skill_name": request.SkillName, "stage": request.Stage, "decision_key": request.DecisionKey, "decision_revision": request.DecisionRevision, "reason": request.Reason, "context": request.Context, "response_model": request.ResponseModel, "response_schema": request.ResponseSchema, "status": "waiting_agent"})
		}
	}
	return client
}

func (c *RemoteClient) SetAbilityDebugService(service *AbilityDebugService) {
	c.debug = service
	if service != nil {
		service.Events = func(execution AbilityDebugExecution) {
			_ = c.SendEvent("ability.debug.status", map[string]any{"debug": execution})
		}
	}
}

func (c *RemoteClient) Run(ctx context.Context) error {
	for {
		if err := c.runConnection(ctx); err != nil && ctx.Err() == nil {
			c.logger.WithError(err).Warn("Pilot 与 Server 连接中断，等待重连")
		}
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(c.config.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (c *RemoteClient) runConnection(ctx context.Context) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.config.AccessToken)
	conn, _, err := websocket.Dial(ctx, c.config.ServerWebSocketURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return err
	}
	// Pilot 注册和心跳包含七类 Ability 的 Manifest；Execution 恢复命令还会
	// 携带结构化 checkpoint。这里必须与 Server 的 Pilot 上行限制保持对称，
	// 否则合法消息超过 WebSocket 库默认的 32 KiB 后会形成每秒重连循环。
	conn.SetReadLimit(pilotServerReadLimit)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	stopContextCloser := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// 主动打断正在等待 Server 命令的 Read；仅取消 context 在某些
			// 网络状态下不能及时唤醒底层连接。
			_ = conn.CloseNow()
		case <-stopContextCloser:
		}
	}()
	defer func() {
		close(stopContextCloser)
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	// 注册消息携带 packages 与 active 的完整快照。packages 表示已经安装的
	// 精确版本，active 只决定 enabled；两者不能合并为一张 active 清单，否则
	// 安装完成、启用尚未完成时一旦重连，Server 会误判包不存在并重复下载。
	actualSkills := make([]store.RobotPilotSkill, 0)
	if c.manager != nil {
		for _, installed := range c.manager.ListInstalled() {
			definition := installed.Definition
			actualSkills = append(actualSkills, store.RobotPilotSkill{
				Name: definition.Name, Version: definition.Version,
				Enabled: installed.Enabled, Status: "installed",
			})
		}
	}
	if err := c.write(map[string]any{"type": "register", "pilot": c.pilotSnapshot(), "skills": actualSkills}); err != nil {
		return err
	}
	if c.debug != nil {
		_ = c.debug.ReportCurrent()
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.heartbeatLoop(heartbeatCtx)
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if kind != websocket.MessageText {
			return errors.New("Server 命令不是 JSON 文本")
		}
		var command serverCommand
		if err := json.Unmarshal(data, &command); err != nil {
			return err
		}
		if command.Type == "skill.validate_input" || command.Type == "skill.describe_input" {
			var result map[string]any
			var validateErr error
			if command.Type == "skill.describe_input" {
				result, validateErr = c.describeSkillInput(ctx, command.Payload)
			} else {
				result, validateErr = c.validateSkillInput(ctx, command.Payload)
			}
			if validateErr != nil {
				_ = c.write(map[string]any{"type": "command.ack", "command_id": command.CommandID,
					"ok": false, "error": validateErr.Error()})
				continue
			}
			_ = c.write(map[string]any{"type": "command.ack", "command_id": command.CommandID,
				"ok": true, "result": result})
			continue
		}
		if err := c.handleCommand(ctx, command); err != nil {
			_ = c.write(map[string]any{"type": "command.ack", "command_id": command.CommandID, "ok": false, "error": err.Error()})
			continue
		}
		_ = c.write(map[string]any{"type": "command.ack", "command_id": command.CommandID, "ok": true})
	}
}

func (c *RemoteClient) describeSkillInput(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var payload struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Name == "" || payload.Version == "" {
		return nil, errors.New("skill.describe_input requires exact name and version")
	}
	return c.runtime.DescribeInput(ctx, payload.Name, payload.Version)
}

func (c *RemoteClient) validateSkillInput(ctx context.Context,
	raw json.RawMessage) (map[string]any, error) {
	var payload struct {
		Name    string         `json:"name"`
		Version string         `json:"version"`
		Input   map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Name == "" || payload.Version == "" || payload.Input == nil {
		return nil, errors.New("skill.validate_input参数不完整")
	}
	return c.runtime.ValidateInput(ctx, payload.Name, payload.Version, payload.Input)
}

func (c *RemoteClient) handleCommand(ctx context.Context, command serverCommand) error {
	var payload map[string]any
	if len(command.Payload) > 0 {
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return err
		}
	}
	switch command.Type {
	case "reconcile.request":
		items, err := c.executions.ListRecoverableSkillExecutions()
		if err != nil {
			return err
		}
		snapshots := make([]map[string]any, 0, len(items))
		for _, item := range items {
			snapshots = append(snapshots, map[string]any{"execution_id": item.ID, "status": item.Status, "checkpoint": item.Checkpoint, "feedback_cursors": item.FeedbackCursors})
		}
		return c.write(map[string]any{"type": "reconcile", "executions": snapshots})
	case "reconcile.result":
		return nil
	case "execution.start":
		return c.startExecution(ctx, payload)
	case "execution.stop":
		id := stringValue(payload["execution_id"])
		reason := stringValue(payload["reason"])
		// Server 是停止命令的传输者，不是 Skill 能识别的操作主体。真正的
		// user/agent 来源保存在 Server 执行记录中；Pilot 侧按 runtime 请求
		// 传给 Worker，避免把部署组件名称泄漏进 Skill 公共模型。
		_, err := c.runtime.Stop(ctx, id, "runtime", reason, "safe")
		return err
	case "agent.reply":
		if c.agent == nil {
			return errors.New("Agent Gateway 未装配")
		}
		var reply AgentReply
		body, _ := json.Marshal(payload)
		if err := json.Unmarshal(body, &reply); err != nil {
			return err
		}
		return c.agent.Resolve(reply)
	case "skill.install":
		return c.installSkill(ctx, payload)
	case "skill.enable":
		definition, err := c.manager.Enable(stringValue(payload["name"]), stringValue(payload["version"]))
		if err == nil {
			c.reportSkill(definition, true, "installed", "")
		}
		return err
	case "skill.disable":
		name, version := stringValue(payload["name"]), stringValue(payload["version"])
		err := c.manager.Disable(name, version)
		if err == nil {
			c.reportSkill(SkillDefinition{Name: name, Version: version}, false, "installed", "")
		}
		return err
	case "skill.uninstall":
		name, version := stringValue(payload["name"]), stringValue(payload["version"])
		err := c.manager.Uninstall(name, version)
		if err == nil {
			c.reportSkill(SkillDefinition{Name: name, Version: version}, false, "uninstalled", "")
		}
		return err
	case "artifact.upload":
		if c.artifacts == nil {
			return errors.New("Pilot Artifact Store 未装配")
		}
		_, err := c.artifacts.Upload(ctx, stringValue(payload["local_artifact_id"]), stringValue(payload["upload_url"]))
		return err
	case "ability.debug.start":
		if c.debug == nil {
			return errors.New("Ability debug 未装配")
		}
		body, _ := json.Marshal(payload)
		var request AbilityDebugExecution
		if err := json.Unmarshal(body, &request); err != nil {
			return err
		}
		_, err := c.debug.Start(ctx, request)
		return err
	case "ability.debug.stop":
		if c.debug == nil {
			return errors.New("Ability debug 未装配")
		}
		_, err := c.debug.Stop(ctx, stringValue(payload["debug_id"]), stringValue(payload["reason"]))
		return err
	default:
		return fmt.Errorf("不支持的 Server 命令 %s", command.Type)
	}
}

func (c *RemoteClient) startExecution(ctx context.Context, payload map[string]any) error {
	value, ok := payload["execution"].(map[string]any)
	if !ok {
		return errors.New("execution.start 缺少 execution")
	}
	input, _ := value["input"].(map[string]any)
	executionID := stringValue(value["execution_id"])
	if executionID == "" {
		executionID = stringValue(value["id"])
	}
	request := SkillStartRequest{ExecutionID: executionID, ProjectID: stringValue(value["project_id"]),
		TaskID: stringValue(value["task_id"]), SubtaskID: stringValue(value["subtask_id"]), RobotID: stringValue(value["robot_id"]),
		SkillName: stringValue(value["skill_name"]), Version: stringValue(value["skill_version"]), Input: input}
	if skillValue, ok := value["skill"].(map[string]any); ok {
		request.SkillName = stringValue(skillValue["name"])
		request.Version = stringValue(skillValue["version"])
	}
	if c.artifacts != nil {
		if items, ok := payload["artifact_downloads"].([]any); ok {
			downloads := make([]map[string]any, 0, len(items))
			for _, item := range items {
				if value, ok := item.(map[string]any); ok {
					downloads = append(downloads, value)
				}
			}
			c.artifacts.Authorize(request.ExecutionID, downloads)
		}
	}
	started, err := c.runtime.Start(ctx, request)
	if err != nil {
		return err
	}
	return c.SendEvent("execution.accepted", map[string]any{"execution_id": started.ID, "status": started.Status})
}

func (c *RemoteClient) installSkill(ctx context.Context, payload map[string]any) error {
	name, version, url := stringValue(payload["name"]), stringValue(payload["version"]), stringValue(payload["package_url"])
	if name == "" || version == "" || url == "" {
		return errors.New("skill.install 参数不完整")
	}
	if c.artifacts == nil {
		return errors.New("Pilot HTTP Transfer 未装配")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.artifacts.absoluteURL(url), nil)
	if err != nil {
		return err
	}
	c.artifacts.authorizeRequest(request)
	response, err := c.artifacts.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 Skill 包失败: HTTP %d", response.StatusCode)
	}
	tempDir, err := os.MkdirTemp("", "pilot-skill-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	archive := filepath.Join(tempDir, "package.zip")
	file, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := file.ReadFrom(response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	definition, err := c.manager.Install(ctx, archive, name, version)
	if err != nil {
		c.reportSkill(SkillDefinition{Name: name, Version: version}, false, "failed", err.Error())
		return err
	}
	c.reportSkill(definition, false, "installed", "")
	return nil
}

func (c *RemoteClient) reportSkill(definition SkillDefinition, enabled bool, status, errorText string) {
	_ = c.SendEvent("skill.status", map[string]any{"name": definition.Name, "version": definition.Version, "enabled": enabled, "status": status, "error": errorText})
}

func (c *RemoteClient) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pilot := c.pilotSnapshot()
			pilot.Status = "online"
			pilot.LastSeenAt = time.Now().UTC()
			if items, err := c.executions.ListRecoverableSkillExecutions(); err == nil && len(items) > 0 {
				pilot.CurrentExecutionID = items[0].ID
				pilot.RobotStatus = "busy"
			} else {
				pilot.RobotStatus = "idle"
			}
			_ = c.write(map[string]any{"type": "heartbeat", "payload": pilot})
		}
	}
}

func (c *RemoteClient) pilotSnapshot() store.RobotPilot {
	pilot := c.config.Pilot
	if c.config.AbilityStatus != nil {
		status := c.config.AbilityStatus()
		pilot.AbilityFrameworkStatus = status.Status
		pilot.AbilityCatalogRevision = status.Revision
		pilot.Abilities = status.Abilities
	}
	return pilot
}

func (c *RemoteClient) SendEvent(eventType string, payload map[string]any) error {
	return c.write(map[string]any{"type": "event", "event_type": eventType, "payload": payload})
}

func (c *RemoteClient) write(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return errors.New("Pilot 尚未连接 Server")
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.conn.Write(writeCtx, websocket.MessageText, body)
}

// RemoteRuntimeEventSink 原样转发 Skill 的 Stage/expectation/observation/deviation/
// progress/next_step 字段，不从 Python 内部状态推断执行进度。
type RemoteRuntimeEventSink struct{ Client *RemoteClient }

func runtimeEventPayload(execution SkillExecution, fields map[string]any) map[string]any {
	payload := cloneMap(fields)
	payload["execution_id"] = execution.ID
	// status 属于 Stage 或 Action 本身。Skill Execution 的状态使用独立字段，
	// 否则 running 会覆盖 action.terminal 的 succeeded/stopped，导致 Server
	// 与 Studio 无法展示真实终态。
	payload["skill_status"] = execution.Status
	// Pilot 和 Server 可能部署在不同进程。由事件产生方记录时间，Server 仅在
	// 旧版本 Pilot 未提供时间时回退到接收时间，避免网络延迟扭曲 Stage 时间线。
	if _, exists := payload["occurred_at"]; !exists {
		payload["occurred_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return payload
}

func (s *RemoteRuntimeEventSink) Report(execution SkillExecution, event string, fields map[string]any) {
	payload := runtimeEventPayload(execution, fields)
	_ = s.Client.SendEvent(event, payload)
}
func (s *RemoteRuntimeEventSink) Log(execution SkillExecution, level, message string, fields map[string]any) {
	payload := cloneMap(fields)
	payload["execution_id"] = execution.ID
	payload["level"] = level
	payload["message"] = message
	_ = s.Client.SendEvent("skill.log", payload)
}
