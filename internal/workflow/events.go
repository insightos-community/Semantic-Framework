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

package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
	"insightos.cn/semantic-framework/internal/store"
)

func (s *Service) publishView(view store.WorkflowView, eventType string) {
	if s.bus == nil {
		return
	}
	// Workflow、Task 和 SubTask 是一次状态事务的同一张只读视图。旧实现先发布
	// 完整 WorkflowView，再为视图中的每个 Task/SubTask 各发布一次事件；四箱
	// Workflow 每次推进会放大成二十多条消息，浏览器随后又为每条消息重复读取
	// 整张视图，最终挤占 Studio WS 心跳。这里只发布一次原子快照，Inspector
	// 直接从其中恢复所有层级，Stage/Feedback 仍由 Robot Execution 事件承载。
	envelope := ws.NewEnvelope(view.Workflow.ConversationID, ws.ChannelDialogue,
		eventType, ws.ImportanceNormal,
		map[string]any{"workflow": view.Workflow, "workflow_view": view})
	envelope.ProjectID = view.Workflow.ProjectID
	envelope.ResourceType = "workflow"
	envelope.ResourceID = view.Workflow.ID
	envelope.Revision = view.Workflow.Revision
	s.bus.Publish(event.TopicAgentEvents, envelope)
}

func (s *Service) publishPlanProposal(proposal store.PlanProposal, eventType string) {
	if s.bus == nil {
		return
	}
	envelope := ws.NewEnvelope(proposal.ConversationID, ws.ChannelDialogue,
		eventType, ws.ImportanceNormal, map[string]any{"plan_proposal": proposal})
	envelope.ProjectID = proposal.ProjectID
	envelope.ResourceType = "plan_proposal"
	envelope.ResourceID = proposal.ID
	envelope.Revision = proposal.Revision
	s.bus.Publish(event.TopicAgentEvents, envelope)
}

func (s *Service) publishPlanProposalAfterApproval(id string) {
	if proposal, err := s.st.GetPlanProposal(id); err == nil {
		s.publishPlanProposal(proposal, "plan_proposal.approved")
	}
}

// appendMilestone 把跨 Agent 协作中用户真正关心的状态写回主 Conversation。
// Stage、Tool Call 和模型思考仍留在 Robot Execution、Trace 与调试面板，避免
// 群聊被运行细节淹没。消息只是可读活动，Workflow Store 仍是状态事实来源。
func (s *Service) appendMilestone(workflowID, taskID, subTaskID, messageKind, status string) {
	wf, err := s.st.GetWorkflow(workflowID)
	if err != nil {
		return
	}
	agentID, content := "leader", fmt.Sprintf("Workflow %s 状态更新为 %s", wf.ID, status)
	metadata := map[string]any{"message_kind": messageKind, "message_type": "activity",
		"workflow_id": wf.ID, "status": status}
	if strings.TrimSpace(taskID) != "" {
		task, taskErr := s.st.GetTask(taskID)
		if taskErr != nil {
			return
		}
		agentID = task.AssignedAgentID
		if agentID == "" {
			agentID = task.RequiredRole
		}
		content = fmt.Sprintf("Task「%s」%s", task.Goal, milestoneStatusText(status, task.WaitingReason))
		metadata["task_id"] = task.ID
		metadata["assigned_agent_id"] = task.AssignedAgentID
		metadata["assigned_robot_id"] = task.AssignedRobotID
		metadata["agent_role"] = task.RequiredRole
		metadata["reason"] = task.WaitingReason
		metadata["result_summary"] = task.ResultSummary
		if len(task.Evidence) > 0 {
			metadata["evidence"] = json.RawMessage(task.Evidence)
		}
	}
	if strings.TrimSpace(subTaskID) != "" {
		metadata["subtask_id"] = subTaskID
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	message := store.ChatMessage{ID: store.NewChatMessageID(),
		SessionID: wf.ConversationID, AgentID: agentID, Message: schema.AssistantMessage(content, nil),
		Metadata: string(encoded), CreatedAt: now}
	if err := s.st.AppendChatMessage(message); err == nil {
		_ = s.st.TouchChatSession(wf.ConversationID, now)
		s.publishConversationMessage(wf, message, metadata)
	}
}

// appendTaskPlanMessage 只公开Task Agent提交的可读摘要与步骤名称。模型思考、
// Tool调用和原始JSON继续留在Run/Trace；这里是真实Agent对用户可见的业务结果，
// 与Framework自动产生的状态活动分开呈现。
func (s *Service) appendTaskPlanMessage(workflowID, taskID, summary string,
	items []store.SubTaskDraft) {
	wf, err := s.st.GetWorkflow(workflowID)
	if err != nil {
		return
	}
	task, err := s.st.GetTask(taskID)
	if err != nil || strings.TrimSpace(summary) == "" {
		return
	}
	// The runtime may already have streamed and persisted this exact Task's public
	// summary. Keep its reasoning/tool history in one bubble instead of repeating it.
	if existing, err := s.st.LatestTaskPlanningMessage(task.ID); err == nil && existing != nil &&
		existing.Message != nil && existing.Message.Content == strings.TrimSpace(summary) {
		return
	}
	agentID := task.AssignedAgentID
	if agentID == "" {
		agentID = task.RequiredRole
	}
	steps := make([]map[string]any, 0, len(items))
	for _, item := range items {
		steps = append(steps, map[string]any{"id": item.ID, "goal": item.Goal, "kind": item.Kind})
	}
	metadata := map[string]any{
		"message_kind": "task_plan_ready", "message_type": "agent_message",
		"workflow_id": wf.ID, "task_id": task.ID, "status": task.Status,
		"agent_role": task.RequiredRole, "assigned_agent_id": task.AssignedAgentID,
		"assigned_robot_id": task.AssignedRobotID, "subtask_count": len(items), "subtasks": steps,
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	message := store.ChatMessage{ID: store.NewChatMessageID(), SessionID: wf.ConversationID,
		AgentID: agentID, Message: schema.AssistantMessage(strings.TrimSpace(summary), nil),
		Metadata: string(encoded), CreatedAt: now}
	if err := s.st.AppendChatMessage(message); err == nil {
		_ = s.st.TouchChatSession(wf.ConversationID, now)
		s.publishConversationMessage(wf, message, metadata)
	}
}

// publishConversationMessage 在 ChatMessage 已持久化后复用现有 message.done
// 通路通知 Studio。主对话仍以 chat_messages 为唯一恢复来源；该事件只解决
// Workflow 里程碑已写库但浏览器必须等下一次 Leader 回复才看见的问题。
func (s *Service) publishConversationMessage(wf store.Workflow, message store.ChatMessage,
	metadata map[string]any) {
	if s.bus == nil || message.Message == nil {
		return
	}
	role, _ := metadata["agent_role"].(string)
	if role == "" {
		role = strings.SplitN(message.AgentID, ":", 2)[0]
	}
	envelope := ws.NewEnvelope(wf.ConversationID, ws.ChannelDialogue, "message.done",
		ws.ImportanceNormal, map[string]any{
			"run_id": message.RunID, "trace_id": message.TraceID,
			"text": message.Message.Content, "metadata": metadata,
		})
	envelope.ProjectID = wf.ProjectID
	envelope.ResourceType = "chat_message"
	envelope.ResourceID = message.ID
	envelope.Revision = wf.Revision
	envelope.Agent = ws.AgentRef{ID: message.AgentID, Role: role, Name: message.AgentID}
	s.bus.Publish(event.TopicAgentEvents, envelope)
}

func milestoneStatusText(status, reason string) string {
	switch status {
	case store.TaskStatusPending:
		return "已分配，等待执行"
	case store.TaskStatusRunning:
		return "已开始"
	case store.TaskStatusPaused:
		if reason != "" {
			return "已暂停：" + reason
		}
		return "已暂停"
	case store.TaskStatusCompleted:
		return "已完成"
	case store.TaskStatusFailed:
		if reason != "" {
			return "失败：" + reason
		}
		return "失败"
	case store.TaskStatusStopped:
		return "已安全停止"
	default:
		return "状态更新为 " + status
	}
}

func (s *Service) publishCurrent(workflowID, eventType string) {
	if view, err := s.st.GetWorkflowView(workflowID); err == nil {
		s.publishView(view, eventType)
	}
}
