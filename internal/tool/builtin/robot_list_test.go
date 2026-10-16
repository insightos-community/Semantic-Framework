package builtin

import (
	"context"
	"encoding/json"
	robotdomain "insightos.cn/semantic-framework/internal/robot"
	"insightos.cn/semantic-framework/internal/store"
	"insightos.cn/semantic-framework/internal/store/storetest"
	"insightos.cn/semantic-framework/internal/tool"
	"insightos.cn/semantic-framework/pkg/log"
	"testing"
)

func TestRobotListAvailableWithoutWorkerOrWorkflowBinding(t *testing.T) {
	st := storetest.OpenMigrated(t, log.New(log.Options{Level: log.LevelError}))
	project, err := st.CreateProject("owner", "catalog")
	if err != nil {
		t.Fatal(err)
	}
	service := robotdomain.NewService(st, nil)
	enrollment, err := service.CreatePilotEnrollment("owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.ClaimPilotEnrollment(enrollment.Code, "pilot"); err != nil {
		t.Fatal(err)
	}
	_, close, err := service.Connect(store.RobotPilot{PilotInstanceID: "pilot", RobotID: "robot", Backend: "real", RobotModel: "R1PRO"})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	if err = service.SetProjectRobot("owner", project.ID, "robot", true); err != nil {
		t.Fatal(err)
	}
	reader := &robotListTool{service: service}
	scope := tool.ExecutionScope{ProjectID: project.ID, OwnerID: "owner", SessionID: "chat", WorkspaceRoot: project.WorkspaceRoot, AgentID: "leader"}
	body, err := reader.Run(tool.WithExecutionScope(context.Background(), scope), "{}")
	if err != nil {
		t.Fatal(err)
	}
	var result robotdomain.ProjectRobotCatalog
	if err = json.Unmarshal([]byte(body), &result); err != nil || len(result.Robots) != 1 {
		t.Fatalf("leader cannot list project robots: %s %v", body, err)
	}
	localization, ok := result.Robots[0]["localization"].(map[string]any)
	if !ok || localization["available"] != false || localization["status"] != "unreported" || localization["pose"] != nil {
		t.Fatalf("unreported localization not explicit: %s", body)
	}
	if !tool.PlanningToolAllowed(reader.Def().Name) || !tool.ConversationPlanToolAllowed(reader.Def().Name) {
		t.Fatal("read-only catalog excluded from planning")
	}
	if tool.PlanningToolAllowed("robot.run") {
		t.Fatal("execution permission expanded")
	}
	scope.OwnerID = "foreign"
	if _, err = reader.Run(tool.WithExecutionScope(context.Background(), scope), "{}"); err == nil {
		t.Fatal("foreign owner can read catalog")
	}
	if _, err = reader.Run(context.Background(), "{}"); err == nil {
		t.Fatal("unscoped catalog exposed")
	}
}
