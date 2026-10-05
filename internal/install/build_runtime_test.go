package install

import (
	"os"
	"path/filepath"
	"testing"

	"insightos.cn/semantic-framework/internal/simulation"
)

func TestRuntimeBuildFillsIntegrityAndSeparatesDependencyWheels(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"runtime-pack.yaml": `schema_version: 1
pack_id: behavior-omnigibson
pack_version: 0.1.0
runner: behavior-omnigibson
python_version: '3.11'
endpoint: http://127.0.0.1:18100
profile: {runtime_profile_id: behavior-omnigibson, engine: isaac, loader: omnigibson}
requirements_lock: {path: requirements.lock}
wheels: [{path: wheels/runtime.whl}]
licenses: [{path: licenses/NOTICE}]
verification_files: [{path: verification/version.json}]
`,
		"requirements.lock":  "dependency==1.0\n",
		"wheels/runtime.whl": "runtime", "wheels/dependency.whl": "dependency",
		"licenses/NOTICE": "test", "verification/version.json": "{}",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := finalizeRuntimeBuild(root, Component{Name: "behavior-omnigibson", Version: "0.2.0"}); err == nil {
		t.Fatal("源码与发布模板版本不一致应拒绝")
	}
	if err := finalizeRuntimeBuild(root, Component{Name: "behavior-omnigibson", Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	manifest, err := simulation.LoadRuntimePack(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Wheelhouse) != 1 || manifest.Wheelhouse[0].Path != "wheelhouse/dependency.whl" {
		t.Fatalf("依赖目录不符合既有安装器: %+v", manifest.Wheelhouse)
	}
	archive := filepath.Join(t.TempDir(), "runtime.zip")
	if err := ZipDirectory(root, archive); err != nil {
		t.Fatal(err)
	}
	pkg, err := InspectArchive(archive)
	if err != nil || pkg.Kind != "runtime" {
		t.Fatalf("不能作为 Runtime 导入: %+v %v", pkg, err)
	}
}
