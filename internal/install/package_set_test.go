package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectPackageEntries(t *testing.T) {
	entries := []PackageEntry{{ID: "skill", Requires: []string{"ability"}}, {ID: "ability", Requires: []string{"robot"}}, {ID: "robot"}, {ID: "scene"}}
	selected, err := SelectPackageEntries(entries, []string{"skill"})
	if err != nil || len(selected) != 3 || selected[0].ID != "robot" || selected[2].ID != "skill" {
		t.Fatalf("依赖未按顺序补齐: %+v %v", selected, err)
	}
	for _, selection := range [][]string{{}, {"missing"}} {
		if _, err := SelectPackageEntries(entries, selection); err == nil {
			t.Fatal("接受了无效选择")
		}
	}
	for _, invalid := range [][]PackageEntry{
		{{ID: "a", Requires: []string{"a"}}},
		{{ID: "a", Requires: []string{"b"}}},
		{{ID: "a"}, {ID: "a"}},
	} {
		if _, err := SelectPackageEntries(invalid, nil); err == nil {
			t.Fatal("接受了循环、缺失依赖或重复 ID")
		}
	}
}

func makeInstallPackage(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	child, id := componentArchive(t, root, `{}`)
	manifest := "schema_version: 1\nname: demo\nversion: 1.0.0\ncomponents:\n  - id: model\n    file: " + filepath.Base(child) + "\n  - id: optional\n    file: optional.zip\n"
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "optional.zip"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "semantic-package.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "package.zip")
	if err := BuildInstallPackage(root, archive); err != nil {
		t.Fatal(err)
	}
	return archive, id
}

func TestPackageBuildInspectAndIndependentReuse(t *testing.T) {
	archive, id := makeInstallPackage(t)
	pkg, err := InspectArchive(archive)
	if err != nil || pkg.Kind != "package" || len(pkg.Components) != 2 {
		t.Fatalf("%+v %v", pkg, err)
	}
	store := &ComponentStore{Root: t.TempDir()}
	called := 0
	installer := func(ctx context.Context, project string, record Record, file string, options Options, progress func(string)) ([]string, error) {
		called++
		if project != "project" || record.ID != id || options.ApplyNow || !options.ProjectDefault || options.RobotID != "robot" {
			t.Fatalf("安装身份或选项错误: %+v %+v", record, options)
		}
		item, err := store.Install(ctx, file, record.ID, progress)
		return []string{item.Name}, err
	}
	for i := 0; i < 2; i++ {
		_, err := BatchInstall(context.Background(), "project", archive, Options{Components: []string{"model"}, ProjectDefault: true, RobotID: "robot", ApplyNow: true}, installer, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || items[0].ID != id || called != 2 {
		t.Fatalf("未复用独立组件身份: %+v %d %v", items, called, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(archive), ".package-install-*"))
	if len(leftovers) != 0 {
		t.Fatal("安装后残留临时子包", leftovers)
	}
}

func TestPackageFailureAndCancellation(t *testing.T) {
	archive, _ := makeInstallPackage(t)
	var calls []string
	failure := errors.New("安装失败")
	installer := func(_ context.Context, _ string, record Record, _ string, _ Options, _ func(string)) ([]string, error) {
		calls = append(calls, record.Name)
		if len(calls) == 2 {
			return nil, failure
		}
		return []string{"completed"}, nil
	}
	resources, err := BatchInstall(context.Background(), "p", archive, Options{}, installer, func(string) {})
	if !errors.Is(err, failure) || !reflect.DeepEqual(resources, []string{"completed"}) || !strings.Contains(err.Error(), "optional") {
		t.Fatalf("部分结果或出错组件丢失: %v %v", resources, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = BatchInstall(ctx, "p", archive, Options{}, installer, func(string) {})
	if !errors.Is(err, context.Canceled) || len(calls) != 2 {
		t.Fatalf("取消后仍安装: %v %v", calls, err)
	}
}

func TestPackageRejectsMissingFileAndNestedRuntime(t *testing.T) {
	root := t.TempDir()
	for _, contents := range []map[string][]byte{
		{"semantic-package.yaml": []byte("schema_version: 1\nname: demo\nversion: 1\ncomponents: [{id: x, file: missing.zip}]\n")},
		{"semantic-package.yaml": []byte("schema_version: 1\nname: demo\nversion: 1\ncomponents: [{id: x, file: ../escape.zip}]\n")},
	} {
		data, _ := packFiles(contents)
		archive := filepath.Join(root, "bad.zip")
		_ = os.WriteFile(archive, data, 0600)
		if _, err := InspectArchive(archive); err == nil {
			t.Fatal("接受了缺失或越界文件")
		}
	}
	data, _ := packFiles(map[string][]byte{"runtime-pack.yaml": []byte("schema_version: 1\npack_id: test\npack_version: 1\n")})
	_ = os.WriteFile(filepath.Join(root, "runtime.zip"), data, 0600)
	_ = os.WriteFile(filepath.Join(root, "semantic-package.yaml"), []byte("schema_version: 1\nname: demo\nversion: 1\ncomponents: [{id: runtime, file: runtime.zip}]\n"), 0600)
	if err := BuildInstallPackage(root, filepath.Join(root, "out.zip")); err == nil {
		t.Fatal("Runtime 被隐式装入项目安装包")
	}
}
