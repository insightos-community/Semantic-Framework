package store

import "testing"

// TestSessionAgentModelSnapshot 验证快照只创建一次、用户覆盖可更新且会话删除
// 会清理关联配置。
func TestSessionAgentModelSnapshot(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateChatSession(newChatSession("cs-model", "usr-model", "模型快照")); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	first, err := st.EnsureSessionAgentModel(SessionAgentModel{
		SessionID: "cs-model", AgentID: "leader", EndpointID: "deepseek-v4-pro",
		ReasoningEffort: "medium", Source: ModelSourceAgentProfile,
	})
	if err != nil {
		t.Fatalf("创建快照失败: %v", err)
	}
	second, err := st.EnsureSessionAgentModel(SessionAgentModel{
		SessionID: "cs-model", AgentID: "leader", EndpointID: "minimax-m3",
		ReasoningEffort: "high", Source: ModelSourceAgentProfile,
	})
	if err != nil {
		t.Fatalf("重复确保快照失败: %v", err)
	}
	if second.EndpointID != first.EndpointID || second.ReasoningEffort != "medium" {
		t.Fatalf("重复确保不能改写已有会话: first=%+v second=%+v", first, second)
	}

	override, err := st.SetSessionAgentModel("cs-model", "leader", "minimax-m3", "auto")
	if err != nil {
		t.Fatalf("覆盖快照失败: %v", err)
	}
	if override.EndpointID != "minimax-m3" || override.Source != ModelSourceSessionOverride {
		t.Errorf("覆盖结果不符: %+v", override)
	}
	list, err := st.ListSessionAgentModels("cs-model")
	if err != nil || len(list) != 1 {
		t.Fatalf("快照列表不符: %v %+v", err, list)
	}
	if err := st.DeleteChatSession("cs-model"); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}
	if _, err := st.GetSessionAgentModel("cs-model", "leader"); err != ErrNotFound {
		t.Errorf("删除会话后快照应不存在，实际: %v", err)
	}
}
