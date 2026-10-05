# Copyright 2026 InsightOS
# SPDX-License-Identifier: Apache-2.0
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

VERSION ?= 0.5.0-dev
LDFLAGS := -X insightos.cn/semantic-framework/pkg/version.Version=$(VERSION)

# 输出纪律：构建/测试产物一律写入 .output/（已在 .gitignore），仓库根不生成 bin/。
OUTPUT_DIR := .output
BIN_DIR := $(OUTPUT_DIR)/bin
LIBERO_REVISION := 8f1084e3132a39270c3a13ebe37270a43ece2a01

.PHONY: build init test test-v040-contracts test-simulation-plugin test-simulation-profile test-simulation-libero lint run logs doctor clean perf-baseline test-v050-real-gate test-v050-mujoco-product test-v050-mujoco-deepseek-single refresh-v050-mujoco-bundle publish-v050-mujoco-skills

build: ## 构建 server / pilot / cli 到 .output/bin/
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/semantic-server ./cmd/semantic-server
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/semantic-pilot  ./cmd/semantic-pilot
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/semantic        ./cmd/semantic

perf-baseline: ## 构建性能基线工具到 .output/bin/semantic-perf
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/semantic-perf ./cmd/semantic-perf

test-v040-contracts: ## 严格验证 Plugin/Framework/Studio/SDK 共用的 v1 仿真样例
	go test ./internal/simulation -count=1 \
		-run 'TestV040PublicInterfaceFixtures|TestV040FixtureRejectsUnknownFields|TestSceneEvaluationFixture'

test-simulation-plugin: ## 连接已启动的真实 Plugin MuJoCo，验证 Framework 客户端
	test -n "$$PLUGIN_MUJOCO_URL"
	go test ./internal/simulation -run TestHTTPRuntimeClientWithRealPlugin -count=1

test-simulation-profile: ## 连接真实 robosuite/LIBERO Profile Runtime，验证评测与 generation
	test -n "$$PLUGIN_MUJOCO_PROFILE_URL"
	go test ./internal/simulation -run TestHTTPRuntimeClientWithRealProfileRuntime -count=1 -timeout=6m

# 固定连接 libero_spatial:0/init-0；Runtime 必须由 semantic-mujoco-libero Runner 提供。
test-simulation-libero:
	test -n "$$PLUGIN_MUJOCO_PROFILE_URL"
	test -n "$$SEMANTIC_LIBERO_ROOT"
	test -n "$$SEMANTIC_LIBERO_REVISION"
	test "$$SEMANTIC_LIBERO_REVISION" = "$(LIBERO_REVISION)"
	test "$$(git -C "$$SEMANTIC_LIBERO_ROOT" rev-parse HEAD)" = "$$SEMANTIC_LIBERO_REVISION"
	test -n "$$SIMULATION_EVIDENCE_DIR"
	mkdir -p "$$SIMULATION_EVIDENCE_DIR/framework"
	PLUGIN_MUJOCO_PROFILE_SCENE_KEY=libero_spatial:0 go test ./internal/simulation \
		-run TestHTTPRuntimeClientWithRealProfileRuntime -count=1 -timeout=6m -json \
		> "$$SIMULATION_EVIDENCE_DIR/framework/libero-profile.json"

test: ## 单元与集成测试（-race -count=1，覆盖率写入 .output/coverage.out）
	@mkdir -p $(OUTPUT_DIR)
	go test ./... -race -count=1 -coverprofile=$(OUTPUT_DIR)/coverage.out

test-v050-real-gate: ## 真实 AbilityFramework + Server + Pilot + Fake Robot 纵向验收
	python3 tests/gate/v050_real_gate.py --output-dir $(OUTPUT_DIR)/v050-real-gate

test-v050-mujoco-product: ## 原生 MuJoCo 单箱产品链：Plan → Workflow → 三个 Robot Skill → 真实物理
	python3 tests/gate/v050_mujoco_product.py --output-dir $(OUTPUT_DIR)/v050-mujoco-product/gate

test-v050-mujoco-deepseek-single: ## 真实 DeepSeek + 原生 MuJoCo 单箱产品链（会产生外部模型费用）
	python3 tests/gate/v050_mujoco_deepseek.py \
		--case single \
		--output-dir $(OUTPUT_DIR)/v050-mujoco-deepseek/single

.PHONY: test-refresh-v050-mujoco
test-refresh-v050-mujoco: ## 刷新脚本的制品版本与独立构建目录回归测试
	python3 -m unittest discover -s scripts -p 'test_refresh_v050_mujoco.py' -v

refresh-v050-mujoco-bundle: ## 重建并激活一致的 MuJoCo Bundle；安装器会先停占用进程，手工跑请先停场景/Server 或加 --stop-users
	python3 scripts/refresh_v050_mujoco.py build --activate

publish-v050-mujoco-skills: ## 发布刷新产物；需要 SEMANTIC_ACCESS_TOKEN，可选 SERVER_HTTP
	test -n "$$SEMANTIC_ACCESS_TOKEN"
	python3 scripts/refresh_v050_mujoco.py publish \
		--server-http "$${SERVER_HTTP:-http://127.0.0.1:8080}" \
		--access-token "$$SEMANTIC_ACCESS_TOKEN"
lint: ## 静态检查（gofmt + go vet + golangci-lint + go mod tidy）
	test -z "$$(gofmt -l $$(git ls-files '*.go'))"
	go vet ./...
	golangci-lint run
	go mod tidy
	git diff --exit-code -- go.mod go.sum

init: build ## 在 .output/ 安装开发运行配置（已有文件保持不变）
	$(BIN_DIR)/semantic init -c $(OUTPUT_DIR)/configs/semantic-server.yaml

run: init ## 使用 .output 安装副本启动 semantic-server
	@mkdir -p $(OUTPUT_DIR)/tmp
	TMPDIR=$(abspath $(OUTPUT_DIR)/tmp) $(BIN_DIR)/semantic-server -c $(OUTPUT_DIR)/configs/semantic-server.yaml

logs: ## 查看开发实例的 Server 结构化日志（Ctrl+C 退出）
	@test -f $(OUTPUT_DIR)/logs/semantic-server.jsonl || \
		(echo "日志尚未生成；请先运行 make run。预期路径：$(OUTPUT_DIR)/logs/semantic-server.jsonl"; exit 1)
	tail -n 200 -F $(OUTPUT_DIR)/logs/semantic-server.jsonl

doctor: init ## 对 .output 安装副本运行 CLI doctor 自检
	$(BIN_DIR)/semantic doctor -c $(OUTPUT_DIR)/configs/semantic-server.yaml

clean: ## 清理 .output/ 下全部产物
	rm -rf $(OUTPUT_DIR)
