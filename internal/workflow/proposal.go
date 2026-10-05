package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"insightos.cn/semantic-framework/internal/store"
)

// SubmitPlanProposal 是显式 Plan Mode 的唯一提交入口。Leader 必须在一次
// plan.suggest 调用中提供完整的主要 Task；校验和持久化成功后才出现 ready
// Proposal。模型流取消、工具参数非法和结构化纠正都只结束当前 Run，不会先落
// drafting/failed 记录，也不会偷偷启动第二个 Planner。
func (s *Service) SubmitPlanProposal(_ context.Context, userID, projectID,
	conversationID string, requested store.WorkflowDraft, summary string, approvedScope json.RawMessage) (store.PlanProposal, error) {
	return s.SubmitPlanProposalForRun("", userID, projectID, conversationID, requested, summary, approvedScope)
}

func (s *Service) SubmitPlanProposalForRun(runID, userID, projectID,
	conversationID string, requested store.WorkflowDraft, summary string, approvedScope json.RawMessage) (store.PlanProposal, error) {
	_, _, err := s.requireWritableConversation(userID, projectID, conversationID)
	if err != nil {
		return store.PlanProposal{}, err
	}
	if len(requested.Tasks) == 0 {
		return store.PlanProposal{}, fmt.Errorf("Plan Proposal 至少需要一个主要 Task: %w",
			store.ErrInvalidState)
	}
	requested, err = prepareTaskIDs(requested)
	if err != nil {
		return store.PlanProposal{}, err
	}
	if err := s.validateMapScope(projectID, requested.MapScope); err != nil {
		return store.PlanProposal{}, err
	}
	ready, reused, err := s.st.SubmitPlanProposalForRun(runID, projectID, conversationID, requested, summary,
		approvedScope, renderPlanDocument(requested, summary, approvedScope), time.Now().UTC())
	if err != nil {
		return store.PlanProposal{}, fmt.Errorf("保存 Plan Proposal 失败（tasks=%d, dependencies=%d）: %w",
			len(requested.Tasks), len(requested.Dependencies), err)
	}
	if !reused {
		s.publishPlanProposal(ready, "plan_proposal.ready")
	}
	return ready, nil
}

// ApprovePlanProposal 把用户审阅的精确 revision 原子转换成运行中的 Workflow。
// Task Agent 与 Robot 的分配发生在批准之后，因而这里不要求 SubTask 或 robot_id。
func (s *Service) ApprovePlanProposal(ctx context.Context, userID, projectID,
	proposalID string, expectedRevision int64) (store.WorkflowView, error) {
	if s.executor == nil {
		return store.WorkflowView{}, ErrExecutorUnavailable
	}
	proposal, err := s.requirePlanProposal(userID, projectID, proposalID)
	if err != nil {
		return store.WorkflowView{}, err
	}
	if proposal.Revision != expectedRevision {
		return store.WorkflowView{}, store.ErrRevisionConflict
	}
	var draft store.WorkflowDraft
	if err := json.Unmarshal(proposal.StructuredPlan, &draft); err != nil {
		return store.WorkflowView{}, err
	}
	if err := s.validateMapScope(projectID, draft.MapScope); err != nil {
		return store.WorkflowView{}, err
	}
	view, err := s.st.ApprovePlanProposal(proposalID, expectedRevision, time.Now().UTC())
	if err != nil {
		return store.WorkflowView{}, err
	}
	s.publishPlanProposalAfterApproval(proposalID)
	s.publishView(view, "workflow.approved")
	activity := map[string]any{
		"message_kind": "workflow_started", "workflow_id": view.Workflow.ID,
		"proposal_id": proposal.ID, "proposal_revision": expectedRevision,
		"status": view.Workflow.Status, "message_type": "activity", "agent_role": "leader",
	}
	metadata, _ := json.Marshal(activity)
	// Proposal 已在上一个事务中原子批准；这里写的是 Leader 在 Conversation
	// 中发布的执行活动，不是下一轮模型的 system policy。若把产品活动保存为
	// system 消息，恢复历史时会污染系统提示，甚至让下一次 Leader Run 无法加载。
	// 消息写入失败也不能回滚或重复创建 Workflow，领域状态仍以 Store 为准。
	message := store.ChatMessage{
		ID: store.NewChatMessageID(), SessionID: proposal.ConversationID, AgentID: "leader",
		Message: schema.AssistantMessage(fmt.Sprintf("Plan revision %d 已批准，Workflow %s 已开始",
			expectedRevision, view.Workflow.ID), nil), Metadata: string(metadata), CreatedAt: time.Now().UTC(),
	}
	if err := s.st.AppendChatMessage(message); err == nil {
		s.publishConversationMessage(view.Workflow, message, activity)
	}
	s.schedule(ctx, userID, view.Workflow.ID)
	return view, nil
}

func (s *Service) DiscardPlanProposal(userID, projectID, proposalID string,
	expectedRevision int64) (store.PlanProposal, error) {
	proposal, err := s.requirePlanProposal(userID, projectID, proposalID)
	if err != nil {
		return store.PlanProposal{}, err
	}
	if proposal.Revision != expectedRevision {
		return store.PlanProposal{}, store.ErrRevisionConflict
	}
	discarded, err := s.st.DiscardPlanProposal(proposalID, expectedRevision, time.Now().UTC())
	if err == nil {
		s.publishPlanProposal(discarded, "plan_proposal.discarded")
	}
	return discarded, err
}

func (s *Service) requirePlanProposal(userID, projectID,
	proposalID string) (store.PlanProposal, error) {
	project, err := s.st.GetProject(projectID)
	if err != nil || project.OwnerID != userID {
		return store.PlanProposal{}, store.ErrNotFound
	}
	proposal, err := s.st.GetPlanProposal(proposalID)
	if err != nil || proposal.ProjectID != projectID {
		return store.PlanProposal{}, store.ErrNotFound
	}
	conversation, err := s.st.GetChatSession(proposal.ConversationID)
	if err != nil || conversation.ProjectID != projectID || conversation.UserID != userID {
		return store.PlanProposal{}, store.ErrNotFound
	}
	return proposal, nil
}

func renderPlanDocument(draft store.WorkflowDraft, summary string, approvedScope json.RawMessage) string {
	var document strings.Builder
	document.WriteString("# ")
	document.WriteString(strings.TrimSpace(draft.Goal))
	if strings.TrimSpace(summary) != "" {
		document.WriteString("\n\n")
		document.WriteString(strings.TrimSpace(summary))
	}
	if len(approvedScope) > 0 && string(approvedScope) != "{}" {
		document.WriteString("\n\n## 批准范围\n\n```json\n")
		document.Write(approvedScope)
		document.WriteString("\n```")
	}
	document.WriteString("\n\n## 主要流程\n\n")
	for index, task := range draft.Tasks {
		fmt.Fprintf(&document, "%d. %s（%s）\n", index+1, strings.TrimSpace(task.Goal),
			strings.TrimSpace(task.RequiredRole))
	}
	document.WriteString("\n## TODO\n\n")
	for _, task := range draft.Tasks {
		fmt.Fprintf(&document, "- [ ] %s\n", strings.TrimSpace(task.Goal))
	}
	return document.String()
}
