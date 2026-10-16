package runtime

import (
	"context"
	"strings"
	"testing"

	"insightos.cn/semantic-framework/internal/skill"
	"insightos.cn/semantic-framework/internal/store"
)

// Exercise the shipped documents, profiles and Project intersection through the
// same reader used by task planning and execution, including scene isolation.
func TestKitchenSkillsProjectGuidance(t *testing.T) {
	fx := newTestFixture(t)
	skills, err := skill.NewStore("../../../configs/skills", fx.logger)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Deps{Profiles: newLoaderForRoot(t, "../../../configs/agents"), Store: fx.st, SkillStore: skills})
	project, err := fx.st.EnsureDefaultProject("kitchen-guidance")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, leader, robot string
	}{
		{"kitchen", "kitchen-workflow-planning", "kitchen-pick-place-task"},
		{"depalletizing", "depalletizing-workflow-planning", "depalletizing-robot-task"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if err := fx.st.ReplaceProjectBindings(context.Background(), project.ID,
				store.ProjectBindings{SkillNames: []string{scenario.leader, scenario.robot}}); err != nil {
				t.Fatal(err)
			}
			for role, expected := range map[string]string{"leader": scenario.leader, "robot": scenario.robot} {
				prof, err := svc.profiles.Load(role)
				if err != nil {
					t.Fatal(err)
				}
				reader, err := svc.projectSkillReader(project.ID, prof)
				if err != nil || reader == nil {
					t.Fatalf("%s skill reader unavailable: %v", role, err)
				}
				items := reader.List()
				if len(items) != 1 || items[0].Name != expected {
					t.Fatalf("%s received wrong scene/role guidance: %+v", role, items)
				}
				if !strings.Contains(reader.Summary(), expected) {
					t.Fatalf("%s is not discoverable during planning", expected)
				}
				guidance, err := svc.taskExecutionGuidance(project.ID, role)
				if err != nil || guidance != "# Agent Skill: "+expected+"\n"+items[0].Body {
					t.Fatalf("%s execution did not receive the complete installed guidance: %v", role, err)
				}
			}
		})
	}
}
