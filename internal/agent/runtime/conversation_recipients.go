package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ListConversationRecipients 复用 Team 与 Robot 目录，按当前 Project 过滤。
// 会话共享公开消息；收件人选择不修改任何 Task.ContextID 或任务分配。
func (s *Service) ListConversationRecipients(userID, sessionID string) ([]AgentInfo, error) {
	if err := s.requireOwnedSession(userID, sessionID); err != nil {
		return nil, err
	}
	sess, err := s.st.GetChatSession(sessionID)
	if err != nil {
		return nil, err
	}
	bindings, err := s.st.GetProjectBindings(sess.ProjectID)
	if err != nil {
		return nil, err
	}
	instances, err := s.st.ListRuntimeInstances(context.Background())
	if err != nil {
		return nil, err
	}
	owners := make(map[string]string)
	for _, instance := range instances {
		// Runtime 已按更新时间降序排列，只保留每台 Robot 的最新归属，
		// 与直接执行的 GetLatestRuntimeByRobot 门禁一致。Pilot 可跨场景
		// 复用或重新注册，不能让旧实例覆盖新归属，也不能凭新 Pilot 绕过。
		if _, seen := owners[instance.RobotID]; !seen {
			owners[instance.RobotID] = instance.ProjectID
		}
	}
	leaderID := s.leaderPlanningAgentID()
	infos := s.Roster()
	if !slices.ContainsFunc(infos, func(info AgentInfo) bool { return info.ID == leaderID }) {
		infos = append(infos, AgentInfo{ID: leaderID, Role: agentRoleLeader, Status: AgentStatusIdle})
	}
	result := make([]AgentInfo, 0, len(infos))
	for _, info := range infos {
		if info.RobotID != "" {
			if owner := owners[info.RobotID]; owner != "" && owner != sess.ProjectID {
				continue
			}
		} else if len(bindings.AgentIDs) > 0 && info.ID != leaderID && !slices.Contains(bindings.AgentIDs, info.ID) {
			continue
		}
		result = append(result, info)
	}
	return result, nil
}

func (s *Service) conversationRecipient(userID, sessionID, agentID string) (AgentInfo, error) {
	if strings.TrimSpace(agentID) == "" || agentID == agentRoleLeader {
		agentID = s.leaderPlanningAgentID()
	}
	infos, err := s.ListConversationRecipients(userID, sessionID)
	if err != nil {
		return AgentInfo{}, err
	}
	for _, info := range infos {
		if info.ID == agentID {
			return info, nil
		}
	}
	return AgentInfo{}, fmt.Errorf("Agent %q 不在当前项目的可选目录中，请重新选择", agentID)
}
