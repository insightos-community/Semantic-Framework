package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"insightos.cn/semantic-framework/internal/agent/kernel"
)

// A correction is a new model exchange after machine feedback, NOT a tool
// failure. Share the budget between tool feedback and final-output validation
// within one purpose Run, so switching error text or validation paths cannot
// create an unbounded retry loop. Normal successful exchanges cost no budget.
const maxContractCorrections = 2

// A successful plan.suggest is a terminal side effect for this Leader Run.
// Stop before another model round; failed submissions still receive bounded
// feedback and may be corrected by the model.
var errPlanSubmissionComplete = errors.New("plan proposal already submitted in this run")

type contractCorrectionBudget struct {
	mu          sync.Mutex
	limit, used int
}

func newContractCorrectionBudget(maxTurns int) *contractCorrectionBudget {
	limit := maxContractCorrections
	if maxTurns > 0 && maxTurns-1 < limit {
		limit = maxTurns - 1
	}
	return &contractCorrectionBudget{limit: limit}
}

func (b *contractCorrectionBudget) request(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.limit {
		return fmt.Errorf("Agent 契约纠正预算耗尽（已安排 %d 次纠正）: %w", b.used, cause)
	}
	b.used++
	return nil
}

// All tools in one model response must return their feedback before deciding
// whether another model exchange is allowed. A successful sibling tool must
// not erase an error, and two failing siblings are not two failed corrections.
// No endpoint is automatically replayed; existing execution/idempotency gates
// remain responsible for preventing duplicate physical side effects.
type contractToolPolicy struct {
	next          kernel.ToolCallGuard
	budget        *contractCorrectionBudget
	mu            sync.Mutex
	pending       map[string]struct{}
	planSubmitted bool
}

func (p *contractToolPolicy) WrapToolCall(ctx context.Context, meta kernel.ToolCallMeta,
	argsJSON string, next kernel.ToolCallEndpoint) (string, error) {
	result, err := p.next.WrapToolCall(ctx, meta, argsJSON, next)
	if err != nil {
		return "", err
	} // Includes interrupts: never convert to retries.
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			ProposalID string `json:"plan_proposal_id"`
		} `json:"data"`
		Error *struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(result), &envelope) == nil && !envelope.OK &&
		envelope.Error != nil && !envelope.Error.Retryable {
		problem := fmt.Sprintf("%s: %s: %s", meta.Name, envelope.Error.Code, envelope.Error.Message)
		p.mu.Lock()
		if p.pending == nil {
			p.pending = make(map[string]struct{})
		}
		p.pending[problem] = struct{}{}
		p.mu.Unlock()
	}
	if (meta.Name == "plan.suggest" || meta.Name == "plan_suggest") && envelope.OK && envelope.Data.ProposalID != "" {
		p.mu.Lock()
		p.planSubmitted = true
		p.mu.Unlock()
	}
	return result, nil
}

func (p *contractToolPolicy) BeforeModelRound(ctx context.Context) error {
	p.mu.Lock()
	if p.planSubmitted {
		p.mu.Unlock()
		return errPlanSubmissionComplete
	}
	problems := make([]string, 0, len(p.pending))
	for problem := range p.pending {
		problems = append(problems, problem)
	}
	p.pending = nil
	p.mu.Unlock()
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems) // Parallel tool completion order is nondeterministic.
	return p.budget.request(ctx, fmt.Errorf("工具反馈: %s", strings.Join(problems, "; ")))
}

var _ kernel.ModelRoundGuard = (*contractToolPolicy)(nil)
