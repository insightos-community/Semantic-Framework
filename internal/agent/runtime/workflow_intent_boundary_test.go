package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"insightos.cn/semantic-framework/internal/agent/kernel"
	"insightos.cn/semantic-framework/internal/store"
)

// 历史 C0 现场在四箱完成后才由 Gate 发现 carrying_object:false。
// 这里直接复现同一个生产解析入口，要求非法计划在交给 Scheduler 前被拒绝。
func TestRobotPlanningRejectsRuntimeStateBeforeDispatch(t *testing.T) {
	task := store.Task{RequiredRole: "robot"}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{Name: "semantic-navigation", Version: "0.4.7"}}}
	for _, tc := range []struct{ name, intent, field string }{
		{"historical_empty_hands", `{"motion_phase":"approach_source","target_ref":"tote-large-l3-r2-c1","carrying_object":false}`, "carrying_object"},
		{"predicted_holding", `{"carrying_object":true}`, "carrying_object"},
		{"copied_holding", `{"held_object":{"object_ref":"box-1","grasp_candidate_id":"old"}}`, "held_object"},
		{"null_state", `{"carrying_object":null}`, "carrying_object"},
		{"nested_state", `{"target":{"held_object":{"object_ref":"box-1"}}}`, "held_object"},
		{"array_state", `{"hints":[{"carrying_object":false}]}`, "carrying_object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"summary":"来源导航","subtasks":[{"id":"nav-to-source","kind":"robot_skill","goal":"到来源箱","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":` + tc.intent + `}}]}`
			items, err := decodeAndValidateTaskPlan(body, task, catalog)
			if err == nil || len(items) != 0 {
				t.Fatalf("不得将执行期状态发布给 Scheduler: items=%+v err=%v", items, err)
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "nav-to-source") {
				t.Fatalf("纠正提示应指出 SubTask 和非法字段: %v", err)
			}
		})
	}
}

// Rebase coverage: upstream adds holding_object while our validator traverses
// nested objects and arrays. Exercise their combination at both consumers.
func TestReservedPlanningStateRebaseMatrix(t *testing.T) {
	task := store.Task{RequiredRole: "robot"}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{Name: "semantic-navigation", Version: "0.4.7"}}}
	for _, field := range []string{"carrying_object", "held_object", "holding_object"} {
		for _, value := range []struct {
			name string
			data any
		}{{"false", false}, {"null", nil}, {"object", map[string]any{"object_ref": "box-1"}}} {
			for _, shape := range []string{"top", "nested", "array"} {
				t.Run(field+"/"+value.name+"/"+shape, func(t *testing.T) {
					intent := map[string]any{field: value.data}
					path := "spec.intent." + field
					switch shape {
					case "nested":
						intent = map[string]any{"hint": intent}
						path = "spec.intent.hint." + field
					case "array":
						intent = map[string]any{"hints": []any{intent}}
						path = "spec.intent.hints[0]." + field
					}
					spec, err := json.Marshal(map[string]any{"skill_name": "semantic-navigation", "skill_version": "0.4.7", "intent": intent})
					if err != nil {
						t.Fatal(err)
					}
					step := store.SubTaskDraft{ID: "reserved-state", Kind: "robot_skill", Goal: "navigate", Spec: spec}
					plan, err := json.Marshal(map[string]any{"summary": "plan", "subtasks": []store.SubTaskDraft{step}})
					if err != nil {
						t.Fatal(err)
					}
					items, planErr := decodeAndValidateTaskPlan(string(plan), task, catalog)
					if planErr == nil || len(items) != 0 || !strings.Contains(planErr.Error(), path) {
						t.Fatalf("planning must reject exact path %s: %v", path, planErr)
					}
					recovery, err := json.Marshal(map[string]any{"decision": "revise_pending", "summary": "recover", "replacements": []store.SubTaskDraft{step}})
					if err != nil {
						t.Fatal(err)
					}
					decision, recoveryErr := decodeTaskRecoveryDecision(string(recovery), task, catalog)
					if recoveryErr == nil || len(decision.Replacements) != 0 || !strings.Contains(recoveryErr.Error(), path) {
						t.Fatalf("recovery must reject exact path %s: %v", path, recoveryErr)
					}
				})
			}
		}
	}
}

func TestRobotPlanningPreservesStableIntentAndCompletionCriteria(t *testing.T) {
	task := store.Task{RequiredRole: "robot"}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{Name: "semantic-navigation", Version: "0.4.7"}}}
	body := `{"summary":"携物导航","subtasks":[{"id":"carry","kind":"robot_skill","goal":"运箱","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":{"carried_object_ref":"box-1","target_ref":"slot-1","motion_phase":"carry_to_place","pose_hint":null}},"completion_criteria":{"carrying_object":true,"arrived":true}}]}`
	items, err := decodeAndValidateTaskPlan(body, task, catalog)
	if err != nil || len(items) != 1 {
		t.Fatalf("稳定对象引用与目标条件不应被当成实时状态拒绝: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(items[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec["intent"].(map[string]any)["carried_object_ref"] != "box-1" {
		t.Fatal("不得静默改写合法引用")
	}
}

func TestRecoveryReplacementUsesPlanningBoundary(t *testing.T) {
	task := store.Task{RequiredRole: "robot"}
	catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{Name: "semantic-navigation", Version: "0.4.7"}}}
	for _, tc := range []struct {
		name, spec string
		valid      bool
	}{
		{"stable_reference", `{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":{"carried_object_ref":"box-1"}}`, true},
		{"runtime_state", `{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":{"carrying_object":false}}`, false},
		{"copied_result", `{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":{"held_object":{"object_ref":"box-1"}}}`, false},
		{"unknown_skill", `{"skill_name":"invented","skill_version":"0.4.7","intent":{}}`, false},
		{"executable_input", `{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":{},"input":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"decision":"revise_pending","summary":"更新未执行步骤","replacements":[{"id":"next","kind":"robot_skill","goal":"导航","spec":` + tc.spec + `}]}`
			_, err := decodeTaskRecoveryDecision(body, task, catalog)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	if _, err := decodeTaskRecoveryDecision(`{"decision":"fail_task","summary":"无法安全恢复","replacements":[]}`, task, catalog); err != nil {
		t.Fatalf("安全终止应保持可用: %v", err)
	}
}

// 复用生产 runTaskAgent 的审核/纠正循环：只替换模型输出，不启动物理动作。
func TestPlanningHoldingBoundaryCorrectionUsesSameRunAndStopsRepeatedError(t *testing.T) {
	invalid := `{"summary":"导航","subtasks":[{"id":"nav","kind":"robot_skill","goal":"导航","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":{"carrying_object":false}}}]}`
	valid := `{"summary":"导航","subtasks":[{"id":"nav","kind":"robot_skill","goal":"导航","spec":{"skill_name":"semantic-navigation","skill_version":"0.4.7","intent":{"target_ref":"box-1"}}}]}`
	for _, repeated := range []bool{false, true} {
		name := "corrected"
		second := valid
		if repeated {
			name, second = "repeated_error", invalid
		}
		t.Run(name, func(t *testing.T) {
			fx := newTestFixture(t)
			model := kernel.NewMockChatModel()
			model.SetScript(kernel.MockReply{Content: invalid}, kernel.MockReply{Content: second}, kernel.MockReply{Content: second})
			svc := newWorkflowRuntimeForTest(t, fx, model)
			project, err := fx.st.EnsureDefaultProject("usr-holding-correction")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			conversation := store.ChatSession{ID: "cs-holding-correction", UserID: project.OwnerID,
				ProjectID: project.ID, Title: name, CreatedAt: now, UpdatedAt: now}
			if err := fx.st.CreateChatSession(conversation); err != nil {
				t.Fatal(err)
			}
			view := approveRuntimeWorkflow(t, fx.st, project, conversation, store.WorkflowDraft{
				Goal: "测试规划纠正循环", Tasks: []store.TaskDraft{{ID: "task-holding-correction", RequiredRole: "developer", Goal: "只验证模型输出"}},
			}, now)
			task := view.Tasks[0]
			catalog := robotTaskPlanningView{Skills: []robotTaskSkillView{{Name: "semantic-navigation", Version: "0.4.7"}}}
			_, err = svc.runTaskAgent(context.Background(), project.OwnerID, project, conversation,
				view.Workflow, "", &task, "developer-1", buildTaskPlanningPrompt([]byte(`{}`), ""),
				nil, nil, store.RunKindTaskPlanning, "", func(body string) error {
					_, problem := decodeAndValidateTaskPlan(body, store.Task{RequiredRole: "robot"}, catalog)
					return problem
				})
			if repeated && err == nil {
				t.Fatal("重复非法状态不得通过或无限重试")
			}
			if !repeated && err != nil {
				t.Fatalf("合法纠正应通过: %v", err)
			}
			calls := model.CallInputs()
			wantCalls := 2
			if repeated {
				wantCalls = 3
			}
			if len(calls) != wantCalls {
				t.Fatalf("模型交换次数错误: got=%d want=%d", len(calls), wantCalls)
			}
			var correction strings.Builder
			for _, message := range calls[1] {
				if message != nil {
					correction.WriteString(message.Content)
				}
			}
			if !strings.Contains(correction.String(), "spec.intent.carrying_object") {
				t.Fatal("纠正调用必须取得真实字段错误")
			}
			runs, _, err := fx.st.ListRunSessions(store.RunFilter{ChatSessionID: conversation.ID}, 0, 0)
			if err != nil || len(runs) != 1 || runs[0].Kind != store.RunKindTaskPlanning {
				t.Fatalf("应在一个 Planning Run 内纠正，不创建执行 Run: %+v %v", runs, err)
			}
		})
	}
}
