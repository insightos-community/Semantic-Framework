package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func bindingReceipt(t *testing.T, s *ComponentStore, item InstalledComponent) string {
	t.Helper()
	item.ID = Digest([]byte(item.Name))
	item.Root = filepath.Join(s.Root, "components", item.ID)
	item.PythonExecutable = filepath.Join(s.Root, "environments", item.ID, "bin/python")
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(s.Root, "receipts", item.ID+".json"), body); err != nil {
		t.Fatal(err)
	}
	return item.ID
}

func TestSelectBindingSwitchesModelAtomicallyWithoutChangingInstalledFiles(t *testing.T) {
	s := &ComponentStore{Root: t.TempDir()}
	ability := InstalledComponent{Component: Component{Kind: "robot_ability", Name: "arm-ability", RobotModels: []string{"arm"}, Abilities: []AbilityComponent{{Role: "policy", AbilityName: "ArmPolicy.V2", Template: "policy", Package: "ability.zip", ModelBackends: []string{"backend-a", "backend-b"}}}}}
	a := bindingReceipt(t, s, ability)
	model := InstalledComponent{Component: Component{Kind: "model", Name: "weights-a", RobotModels: []string{"arm"}, ModelConfig: "model.json", ModelCompatibility: &ModelCompatibility{Role: "policy", AbilityName: "ArmPolicy.V2", Backend: "backend-a", RuntimeProfiles: []string{"sim-a"}}}}
	m1 := bindingReceipt(t, s, model)
	model.Name = "weights-b"
	model.ModelCompatibility.Backend = "backend-b"
	m2 := bindingReceipt(t, s, model)
	first, err := s.SelectBinding([]string{a, m1}, "robot", "arm", "sim-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SelectBinding([]string{m2, a}, "robot", "arm", "sim-a")
	if err != nil || second.Model.ComponentID != m2 || second.Abilities["policy"].ComponentID != a {
		t.Fatalf("%+v %v", second, err)
	}
	if second.Revision == first.Revision {
		t.Fatal("更换模型未产生新修订")
	}
	before, _ := os.ReadFile(BindingPath(s.Root, "robot"))
	for _, test := range []struct {
		ids            []string
		model, profile string
	}{
		{[]string{a, m1}, "other", "sim-a"}, {[]string{a, m1}, "arm", "sim-b"},
		{[]string{a, m1, m2}, "arm", "sim-a"}, {[]string{m1}, "arm", "sim-a"},
		{[]string{a}, "arm", "sim-a"}, {[]string{a, a, m1}, "arm", "sim-a"},
	} {
		if _, err := s.SelectBinding(test.ids, "robot", test.model, test.profile); err == nil {
			t.Fatalf("错误组合被接受: %+v", test)
		}
		after, _ := os.ReadFile(BindingPath(s.Root, "robot"))
		if string(before) != string(after) {
			t.Fatal("失败的选择改变了待生效绑定")
		}
	}
	model.Name = "unknown-backend"
	model.ModelCompatibility.Backend = "unsupported"
	bad := bindingReceipt(t, s, model)
	if _, err := s.SelectBinding([]string{a, bad}, "robot", "arm", "sim-a"); err == nil {
		t.Fatal("未知推理后端被接受")
	}
	model.Name = "unknown-interface"
	model.ModelCompatibility.Backend = "backend-a"
	model.ModelCompatibility.AbilityName = "Other.V2"
	bad = bindingReceipt(t, s, model)
	if _, err := s.SelectBinding([]string{a, bad}, "robot", "arm", "sim-a"); err == nil {
		t.Fatal("错误模型接口被接受")
	}
	model.Name = "legacy"
	model.ModelCompatibility = nil
	legacy := bindingReceipt(t, s, model)
	if _, err := s.SelectBinding([]string{a, legacy}, "robot", "arm", "sim-a"); err == nil {
		t.Fatal("缺少兼容性声明的模型被当作兼容")
	}
	if _, err := s.Rollback("robot", first.Revision); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, m1, m2} {
		if _, err := s.Get(id); err != nil {
			t.Fatal("切换修改了安装收据", err)
		}
	}
}

func TestProjectSelectionOnlySeedsNewRobots(t *testing.T) {
	s := &ComponentStore{Root: t.TempDir()}
	item := InstalledComponent{Component: Component{Kind: "robot_ability", Name: "a", RobotModels: []string{"arm"}, Abilities: []AbilityComponent{{Role: "motion", AbilityName: "Motion", Template: "motion", Package: "a.zip"}}}}
	a := bindingReceipt(t, s, item)
	item.Name = "b"
	b := bindingReceipt(t, s, item)
	if _, err := s.SelectProjectDefault([]string{a}, "project", "arm", "sim"); err != nil {
		t.Fatal(err)
	}
	path, err := s.EnsureRobotBinding("project", "existing", "arm")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := s.SelectProjectDefault([]string{b}, "project", "arm", "sim"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureRobotBinding("project", "existing", "arm"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("更改项目默认覆盖了现有 Robot")
	}
	path, err = s.EnsureRobotBinding("project", "new", "arm")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	var binding RobotBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Abilities["motion"].ComponentID != b {
		t.Fatal("新 Robot 未继承项目默认")
	}
	if value, err := s.ProjectDefault("other-project", "arm"); err != nil || value != nil {
		t.Fatal("项目默认串用")
	}
}

func TestParseModelCompatibilityRequiresExplicitContract(t *testing.T) {
	valid := modelManifest + "model_compatibility:\n  role: policy\n  ability_name: Arm.V2\n  backend: backend-a\n  runtime_profiles: [sim]\n"
	if _, err := ParseComponent([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseComponent([]byte(modelManifest + "model_compatibility: {role: policy}\n")); err == nil {
		t.Fatal("不完整兼容声明通过")
	}
}
