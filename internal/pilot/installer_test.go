package pilot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequirementsLockRejectsRangesAndInstallOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requirements.lock")
	for _, value := range []string{"pydantic>=2\n", "--extra-index-url https://example.invalid\n", "git+https://example.invalid/repo\n"} {
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateRequirementsLock(path); err == nil {
			t.Fatalf("unsafe lock accepted: %q", value)
		}
	}
	if err := os.WriteFile(path, []byte("pydantic==2.13.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateRequirementsLock(path); err != nil {
		t.Fatalf("exact lock rejected: %v", err)
	}
}

func TestInstallArgsOfflineWheelhouse(t *testing.T) {
	offline := VenvSkillInstaller{Wheelhouse: "/bundle/wheels"}.installArgs("/bundle/wheels/sdk.whl")
	want := []string{"install", "--disable-pip-version-check", "--no-index",
		"--find-links", "/bundle/wheels", "/bundle/wheels/sdk.whl"}
	if strings.Join(offline, " ") != strings.Join(want, " ") {
		t.Fatalf("offline install args: got=%v want=%v", offline, want)
	}
	online := VenvSkillInstaller{}.installArgs("/src/sdk")
	if strings.Join(online, " ") != "install --disable-pip-version-check /src/sdk" {
		t.Fatalf("无 Wheelhouse 时不应附加离线参数: %v", online)
	}
}
