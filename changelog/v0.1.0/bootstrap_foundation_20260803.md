# B1（M1.1 工程地基）：可启动的空壳 server 与 CI 门禁

日期：2026-08-03
里程碑：Phase 1 / B1（M1.1 工程地基）

## 为什么做这次变更

仓库此前只有骨架：三个空壳 main、空的 internal/pkg 目录、无依赖的 go.mod。
本里程碑把"工程地基"做实——配置、日志、版本、装配层、HTTP 健康检查与
CI 门禁全部落地，使空壳 semantic-server 可以真正启动并暴露
`/api/v1/system/healthz`，为后续 B2（存储/WS）与 B3（eino 接入）提供
统一的工程规范与质量门禁。

## 包含内容

- **go.mod 依赖收敛**：仅引入 `gopkg.in/yaml.v3`（配置解析）与
  `github.com/go-chi/chi/v5`（HTTP 路由）；`go mod tidy` 干净。
  websocket、sqlite、eino 按里程碑计划不引入。
- **pkg/log**：基于 log/slog 的结构化日志器（重写自参考实现，剔除全部
  ioslog 相关内容）。固定 JSON 输出、级别可配（含运行时 SetLevel）、
  WithField/WithFields/WithTraceID/WithError 不可变派生子日志器；
  traceid.go 提供 trace_id 生成（16 字节十六进制，对齐 OTel 规范）与
  context 注入/提取。
- **pkg/config**：`Load(path)` 三级覆盖——代码默认值 → yaml 文件 →
  `SEMANTIC_` 前缀环境变量（嵌套以 `_` 分隔）。仅建模 server/log/store
  三段（http_addr/read_timeout/write_timeout、level、driver/sqlite_path），
  默认值与 configs/semantic-server.yaml 一致；Duration 类型支持 "10s"
  风格字符串。
- **pkg/version**：`Version` 变量（ldflags 注入，默认 "0.1.0-dev"）+ `String()`。
- **internal/bootstrap**：`Wire(cfg, logger)` 装配 chi 路由
  （仅 `/api/v1/system/healthz`、`/api/v1/system/version`），
  `App.Run(ctx)` 支持 signal 通知 + http.Server.Shutdown 优雅退出
  （5s 超时）。
- **cmd/semantic-server**：`-c` 参数 → config.Load → bootstrap.Wire → Run。
- **cmd/semantic**：`doctor` 子命令（标准库 flag）：① 配置文件可加载
  ② :8080/:8081 端口占用检查 ③ SEMANTIC_LLM_API_KEY_DEEPSEEK_CHAT
  未设置时警告不报错；逐项 ✓/✗/! 输出，存在 ✗ 时退出码 1。
- **Makefile**：build 输出至 `.output/bin/`；test 带 -race 与
  `.output/coverage.out`；lint = gofmt + vet + golangci-lint；
  run/doctor/clean 目标。
- **.golangci.yml**：govet/gofmt/staticcheck/errcheck/gosimple/unused/revive
  （含导出注释规则），排除 docs/changelog。
- **.gitlab-ci.yml**：lint/test/build/gitleaks 四 job，image golang:1.23。
- **configs/semantic-server.yaml**：收敛至 server/log/store 三段，
  与 Config 模型一一对应。

## 影响面

无跨模块影响。本次为首批落地代码，仅填充空目录与修订工程文件
（Makefile/CI/configs 示例），不改动任何既有实现逻辑。
cmd/semantic-pilot 保持空壳占位。

## 测试内容与标准

- `pkg/log` 单测（14 个用例，覆盖率 94.1%，-race 通过）：
  级别过滤（Debug/Info/Error/Trace）、WithField/WithFields/WithTraceID/
  WithError/WithError(nil)/链式 With/结构化参数、SetLevel 动态调整、
  ParseLevel 表驱动、Level.String 表驱动、trace_id 长度/唯一性/字符集/
  context 往返/空 context。
- `pkg/config` 单测（5 个用例，覆盖率 76.9%，-race 通过）：
  默认值加载、yaml 覆盖（未出现字段保留默认）、env 覆盖（优先级最高）、
  配置文件缺失报错、非法时长环境变量报错。
- `tests/integration` 集成测试（2 个用例，-race 通过）：
  真实端口启动装配后的 App，断言 healthz 返回 200 + `{"status":"ok"}`、
  version 返回 200 + 当前版本号，并验证 ctx 取消后优雅退出。
- 手工实测：
  - `make build` 产物全部落在 `.output/bin/`，仓库根无 bin/；
  - `make run` 启动后 `curl localhost:8080/api/v1/system/healthz`
    返回 200 `{"status":"ok"}`，`/version` 返回 `{"version":"0.1.0"}`
    （ldflags 注入生效）；
  - SIGTERM 触发优雅退出，日志完整记录启动/信号/退出；
  - `semantic doctor` 在端口空闲时全 ✓（env 缺失误警告，退出码 0），
    8080 被占用时对应项 ✗ 且退出码 1。
- 门禁：`gofmt -l .` 无输出、`go vet ./...` 通过、
  `golangci-lint run` 零告警、`go test ./... -race` 全绿。

## 遗留 TODO

- healthz 当前仅表示进程存活，待 B2 存储接入后升级为真实依赖探活。
- store 段配置（driver/sqlite_path）已建模但尚无消费方代码，B2 落地。
- ws_addr（:8081）未进入 Config 模型，随 B2 WebSocket 接入一并建模；
  doctor 目前按文档约定硬编码检查该端口。
- doctor 暂只覆盖配置/端口/环境变量三类检查，后续随模块接入扩展
  （LLM 连通性、存储可写性等）。
- 集成测试采用"申请-释放"方式获取空闲端口，存在理论上的竞争窗口，
  后续可改为 App 支持 :0 随机端口并回读实际地址。
