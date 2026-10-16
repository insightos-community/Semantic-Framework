package builtin

import (
	"context"
	"encoding/json"
	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/tool"
	"strings"
)

type robotListTool struct{ service *robotdomain.Service }

func (t *robotListTool) Def() tool.Definition {
	return tool.Definition{Name: "robot.list", Namespace: "robot", Description: "只读查询项目机器人、连接状态、已安装 Robot Skill 和实时定位 localization。问当前位置时直接调用本工具，不要用 map.query 中是否存在 robot 实体判断定位。localization.available=true 才可报告 pose.x/y（米）、yaw（弧度）；同时说明 frame、age_s 和查询时间。离线、超过 5 秒、未上报或地图变更时不给当前位置，不用历史或其他机器人的位姿代替。selected_map_source 表示该机器人是否是项目所选地图来源；它和 project_map_synchronized 都为 true 才可直接对应本项目地图；否则只报告机器人自身 map 坐标并说明未对齐。available_robots 尚未加入项目，不返回定位。普通对话和规划可用，无需批准 Workflow、执行 Skill 或绑定 Robot Worker；不执行动作，定位不作为运动到位或安全控制证据。", ParametersJSON: `{"type":"object","properties":{},"additionalProperties":false}`, Annotations: tool.Annotations{Risk: tool.RiskLow, Idempotent: true}}
}
func (t *robotListTool) Run(ctx context.Context, raw string) (string, error) {
	if strings.TrimSpace(raw) != "" {
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &args); err != nil || len(args) > 0 {
			return "", &tool.Error{Code: "BAD_ARGUMENTS", Message: "robot.list 不接受参数；项目来自当前对话"}
		}
	}
	scope, ok := tool.ExecutionScopeFromContext(ctx)
	if !ok {
		return "", &tool.Error{Code: "PROJECT_REQUIRED", Message: "请在项目对话中查询机器人"}
	}
	result, err := t.service.ProjectRobots(scope.OwnerID, scope.ProjectID)
	if err != nil {
		return "", robotToolError(err)
	}
	body, err := json.Marshal(result)
	return string(body), err
}
