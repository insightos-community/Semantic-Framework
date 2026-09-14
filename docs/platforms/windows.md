# Native Windows foundation

Target: Windows x64, with Go 1.25.8. The native Windows workflow builds
`semantic.exe`, `semantic-server.exe` and `semantic-pilot.exe` and packages them as
an Actions development artifact. This is not a complete product installer.

```powershell
# Install LLVM-MinGW 20260908 UCRT x86_64, verify the archive SHA-256:
# 1bcf74d06b724aeecaa6412ca85f5b26fb1da770e7cdcefa9263c9c5c3ad34b6
# Add its bin directory to PATH. The MSVCRT variant is incompatible with
# the bundled go-fitz MuPDF archives; do not substitute it.
$env:CC = 'x86_64-w64-mingw32-clang'
$env:CXX = 'x86_64-w64-mingw32-clang++'
$env:CGO_ENABLED = '1'
$env:GOFLAGS = '-ldflags=-extldflags=-static'
go build -o .output/bin/semantic.exe ./cmd/semantic
go build -o .output/bin/semantic-server.exe ./cmd/semantic-server
go build -o .output/bin/semantic-pilot.exe ./cmd/semantic-pilot
$env:SEMANTIC_NATIVE_BIN = (Resolve-Path .output/bin).Path
go test ./internal/ports/... -count=1 -timeout=4m
go test ./internal/tool/builtin -run 'TestExecuteHost|TestWindowsHost' -count=1 -timeout=3m
```

The lifecycle test initializes an instance under a Unicode/space-containing path,
starts the actual Server, checks HTTP health, requests graceful shutdown through
the Windows named-event port, verifies the SQLite file is unlocked and repeats.
Process contracts test owned descendant retirement and preservation when a
management handle closes. Production Robot stop still requires Pilot hold and
Ability stop evidence; no Windows API call replaces that protocol.

Windows uses `.exe`, `Scripts/python.exe`, native PowerShell host commands and the
`glfw` MuJoCo backend. Host execution retains both server/session permission gates,
uses an encoded PowerShell command with a literal working directory and cancels
its owned Job Object. Linux/macOS retain their existing shells and signal behavior.

Framework and semantic-deployment carry compatible, independently testable
`internal/ports/process` and `internal/ports/stop` adapters; neither imports the
other repository's internal packages. Keep their stop-event protocol compatible.

Full Windows Robot/AbilityFramework, Python dependency closure and graphical
MuJoCo validation remain separate work. Interrupted Windows orphan recovery does
not claim ownership from executable names: without an owning job handle it
reports the need for reconciliation. This preserves the existing safety boundary.
