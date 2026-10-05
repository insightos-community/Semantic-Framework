package bootstrap

import (
	"context"
	"errors"
	"sync"

	"insightos.cn/semantic-framework/internal/interaction"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/workflow"
)

// planEntry 解决装配顺序中的闭环：Agent 工具注册早于 Workflow Service，
// 运行期则把协作模式的建议交给 Interaction，把用户显式 Plan Mode 的生成
// 交给唯一 Workflow Service。它不包含任何业务状态。
type planEntry struct {
	mu           sync.RWMutex
	st           *store.Store
	interactions *interaction.Service
	workflows    *workflow.Service
}

func newPlanEntry(interactions *interaction.Service, st *store.Store) *planEntry {
	return &planEntry{interactions: interactions, st: st}
}

func (p *planEntry) SetWorkflow(service *workflow.Service) {
	p.mu.Lock()
	p.workflows = service
	p.mu.Unlock()
}

func (p *planEntry) CreateAgentQuestion(ctx context.Context, request interaction.AgentQuestionRequest) (store.Interaction, error) {
	if request.TaskID == "" {
		return p.interactions.CreateAgentQuestion(ctx, request)
	}
	p.mu.RLock()
	service := p.workflows
	p.mu.RUnlock()
	if service == nil {
		return store.Interaction{}, workflow.ErrPlannerUnavailable
	}
	task, err := p.st.GetTask(request.TaskID)
	if err != nil {
		return store.Interaction{}, err
	}
	// Task Agent 询问用户前必须先把 Task 持久化为 paused。这样 Server 重启、
	// 浏览器刷新或模型 Run 已结束时，调度器都不会继续推进下一 SubTask；
	// Interaction 创建失败则用暂停后的 revision 精确回滚，避免遗留无入口的等待态。
	paused, err := service.PauseTaskForInteraction(request.UserID, request.ProjectID,
		request.WorkflowID, request.TaskID, task.Revision)
	if err != nil {
		return store.Interaction{}, err
	}
	created, err := p.interactions.CreateAgentQuestion(ctx, request)
	if err == nil {
		return created, nil
	}
	rollbackErr := service.ResumeTaskAfterInteractionFailure(request.UserID,
		request.ProjectID, request.WorkflowID, request.TaskID, paused.Revision)
	return store.Interaction{}, errors.Join(err, rollbackErr)
}

func (p *planEntry) SubmitPlanProposal(ctx context.Context,
	request interaction.PlanSuggestionRequest) (store.PlanProposal, error) {
	p.mu.RLock()
	service := p.workflows
	p.mu.RUnlock()
	if service == nil {
		return store.PlanProposal{}, workflow.ErrPlannerUnavailable
	}
	return service.SubmitPlanProposalForRun(request.RunID, request.UserID, request.ProjectID, request.SessionID,
		store.WorkflowDraft{Goal: request.Goal, Constraints: request.Constraints,
			CompletionCriteria: request.CompletionCriteria, MapScope: request.MapBinding,
			Tasks: request.Tasks, Dependencies: request.Dependencies}, request.Summary, request.ApprovedScope)
}
