package runtime

import (
	"encoding/json"
	"sort"
	"unicode/utf8"

	"insightos.cn/semantic-framework/internal/store"
)

// Prompt projection only. Never changes the stored outcome, receipt, criteria
// or evidence. Large data remains available by its original execution ID.
func contextText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + "…[truncated; original evidence retained]"
}

func contextValue(value any, depth int) any {
	if raw, ok := value.(json.RawMessage); ok {
		var decoded any
		if json.Unmarshal(raw, &decoded) != nil {
			return nil
		}
		return contextValue(decoded, depth)
	}
	if depth <= 0 {
		return "[nested evidence omitted; read original receipt]"
	}
	switch v := value.(type) {
	case string:
		return contextText(v, 1500)
	case map[string]any:
		out := map[string]any{}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i == 16 {
				out["_omitted_fields"] = len(keys) - i
				break
			}
			out[k] = contextValue(v[k], depth-1)
		}
		return out
	case []any:
		out := make([]any, 0, 9)
		for i, item := range v {
			if i == 8 {
				out = append(out, map[string]any{"_omitted_items": len(v) - i})
				break
			}
			out = append(out, contextValue(item, depth-1))
		}
		return out
	default:
		return value
	}
}

func workflowTerminalContext(view store.WorkflowView) map[string]any {
	w := view.Workflow
	tasks := make([]map[string]any, 0, len(view.Tasks))
	for _, t := range view.Tasks {
		tasks = append(tasks, map[string]any{
			"id": t.ID, "goal": contextText(t.Goal, 2000), "status": t.Status,
			"reason": contextText(t.WaitingReason, 1000), "revision": t.Revision,
			"result_summary":      contextText(t.ResultSummary, 6000),
			"completion_criteria": contextValue(t.CompletionCriteria, 4),
			"evidence":            contextValue(t.Evidence, 3),
		})
	}
	counts := map[string]int{}
	steps := make([]map[string]any, 0, len(view.SubTasks))
	for _, sub := range view.SubTasks {
		counts[sub.Status]++
		entry := completedSubTaskContext(sub)
		entry["task_id"] = sub.TaskID
		entry["reason"] = contextText(sub.WaitingReason, 1000)
		entry["goal"] = contextText(sub.Goal, 600)
		entry["result_summary"] = contextValue(entry["result_summary"], 2)
		entry["next_step"] = contextValue(entry["next_step"], 2)
		entry["evidence_refs"] = contextValue(sub.Evidence, 3)
		entry["completion_criteria"] = contextValue(sub.CompletionCriteria, 4)
		// Keep explicit error and scalar outcomes, never plan/packet/input graphs.
		var envelope map[string]any
		if json.Unmarshal(sub.Result, &envelope) == nil {
			entry["error"] = contextValue(envelope["error"], 3)
			if result, ok := envelope["result"].(map[string]any); ok {
				if payload, ok := result["payload"].(map[string]any); ok {
					facts := map[string]any{}
					for _, key := range []string{"reached", "final_pose", "pose", "fresh", "holding", "transport_posture", "completed", "operation_id", "search_status"} {
						if val, exists := payload[key]; exists {
							facts[key] = contextValue(val, 3)
						}
					}
					entry["observed"] = facts
				}
			}
		}
		steps = append(steps, entry)
	}
	return map[string]any{
		"workflow": map[string]any{"id": w.ID, "goal": contextText(w.Goal, 4000),
			"status": w.Status, "reason": w.Reason, "revision": w.Revision,
			"completion_criteria": contextValue(w.CompletionCriteria, 4)},
		"tasks": tasks, "subtask_counts": counts, "subtasks": steps,
		"evidence_policy": "Only explicit receipts prove effects. Failed/replaced history remains failed; planned is not executed. Use execution_id to read omitted details. Per-task acceptance does not change global tolerances.",
	}
}

func terminalInteractionContext(workflowID string, answers []store.Interaction) []map[string]any {
	result := []map[string]any{}
	for _, answer := range answers {
		if answer.WorkflowID != workflowID || answer.Status != store.InteractionStatusAnswered {
			continue
		}
		if len(result) == 16 {
			break
		}
		result = append(result, map[string]any{"interaction_id": answer.ID, "task_id": answer.TaskID,
			"question": contextValue(json.RawMessage(answer.Payload), 3),
			"reply":    contextText(answer.Reply, 3000), "scope": "this workflow only; not physical evidence"})
	}
	return result
}
