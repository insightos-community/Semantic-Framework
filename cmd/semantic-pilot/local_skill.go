// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	stopport "insightos.cn/semantic-framework/internal/ports/stop"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"insightos.cn/semantic-framework/internal/pilot"
)

type localSkillEventWriter struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (w *localSkillEventWriter) Report(execution pilot.SkillExecution, event string, fields map[string]any) {
	w.write(map[string]any{
		"time": time.Now().UTC(), "kind": "event", "event": event,
		"execution_id": execution.ID, "skill": execution.SkillName,
		"status": execution.Status, "fields": fields,
	})
}

func (w *localSkillEventWriter) Log(execution pilot.SkillExecution, level, message string, fields map[string]any) {
	w.write(map[string]any{
		"time": time.Now().UTC(), "kind": "log", "level": level, "message": message,
		"execution_id": execution.ID, "skill": execution.SkillName, "fields": fields,
	})
}

func (w *localSkillEventWriter) write(value map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.encoder.Encode(value)
}

type repeatedStrings []string

func (values *repeatedStrings) String() string {
	return strings.Join(*values, string(os.PathListSeparator))
}

func (values *repeatedStrings) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("路径不能为空")
	}
	*values = append(*values, value)
	return nil
}

func runLocalSkillCommand(arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "run" {
		return errors.New("用法: semantic-pilot skill run --profile <robot-deployment.yaml> --skill <name@version> --input <input.json>")
	}
	flags := flag.NewFlagSet("skill run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	profilePath := flags.String("profile", envFirst("SEMANTIC_ROBOT_CONFIG", "SEMANTIC_ROBOT_DEPLOYMENT"), "RobotDeployment YAML")
	skillReference := flags.String("skill", "", "Robot Skill 精确名称和版本，例如 grasp-object@0.2.0")
	inputPath := flags.String("input", "", "Skill 输入 JSON；使用 - 从 stdin 读取")
	skillCatalogRoot := flags.String("skill-catalog", os.Getenv("SEMANTIC_ROBOT_SKILL_CATALOG_DIR"), "包含各 Skill 目录的本地 Catalog")
	python := flags.String("python", envDefault("SEMANTIC_ROBOT_SKILL_PYTHON", "python"), "运行 Robot Skill Worker 的 Python")
	abilityFramework := flags.String("ability-framework", "", "覆盖 RobotDeployment 中的 AbilityFramework endpoint")
	executionID := flags.String("execution-id", "", "可选的本地执行 ID")
	timeout := flags.Duration("timeout", 15*time.Minute, "执行最长时间；超时后请求安全停止")
	eventsPath := flags.String("events", "-", "JSONL 事件输出；- 表示 stderr，空字符串表示关闭")
	resultPath := flags.String("result", "-", "最终 JSON 输出；- 表示 stdout")
	var pythonPaths repeatedStrings
	flags.Var(&pythonPaths, "python-path", "附加 Worker Python import 路径；可重复")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*profilePath) == "" || strings.TrimSpace(*skillReference) == "" || strings.TrimSpace(*inputPath) == "" {
		return errors.New("--profile、--skill 和 --input 必填")
	}

	name, version, err := splitSkillReference(*skillReference)
	if err != nil {
		return err
	}
	input, err := readJSONObject(*inputPath, os.Stdin)
	if err != nil {
		return fmt.Errorf("读取 Skill 输入: %w", err)
	}
	deployment, err := pilot.LoadRobotDeployment(*profilePath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*abilityFramework) != "" {
		deployment.AbilityFramework.Endpoint = strings.TrimRight(*abilityFramework, "/")
	}
	if strings.TrimSpace(deployment.AbilityFramework.Endpoint) == "" {
		return errors.New("RobotDeployment 缺少 ability_framework.endpoint")
	}
	if strings.TrimSpace(*skillCatalogRoot) == "" {
		if deployment.Pilot.RobotSkillDirectory != "" {
			*skillCatalogRoot = filepath.Join(deployment.Pilot.RobotSkillDirectory, "active")
		} else {
			return errors.New("RobotDeployment 未配置 pilot.robot_skill_directory；开发态请传 --skill-catalog")
		}
	}

	skills, err := pilot.ScanSkillCatalog(*skillCatalogRoot)
	if err != nil {
		return fmt.Errorf("读取 Robot Skill Catalog: %w", err)
	}
	if _, err := skills.Resolve(name, version); err != nil {
		return err
	}
	abilityClient := pilot.NewAbilityFrameworkClient(deployment.AbilityFramework.Endpoint, nil)
	actionCatalog := pilot.NewCatalog()
	discovery := pilot.NewAbilityFrameworkDiscovery(deployment, abilityClient, actionCatalog, nil)
	discoveryContext, cancelDiscovery := context.WithTimeout(context.Background(), 10*time.Second)
	err = discovery.Refresh(discoveryContext)
	cancelDiscovery()
	if err != nil {
		return fmt.Errorf("刷新 AbilityFramework 目录: %w", err)
	}

	events, closeEvents, err := openLocalSkillEvents(*eventsPath)
	if err != nil {
		return err
	}
	defer closeEvents()
	// 本地物理调试的目标是观察 Skill/Ability/SDK，而不是用固定答案模拟 Robot
	// Agent。Skill 一旦确实需要上层决策就明确失败，调用者据此调整输入或回到
	// Semantic Framework；不能在这里伪造候选选择并掩盖真实恢复边界。
	agent := pilot.NewRemoteAgentGateway(func(request pilot.AgentRequest) error {
		events.Report(pilot.SkillExecution{
			ID: request.ExecutionID, SkillName: request.SkillName,
			Status: pilot.SkillWaitingAgent,
		}, "agent.requested", map[string]any{
			"decision_key": request.DecisionKey, "stage": request.Stage,
			"reason": request.Reason, "context": request.Context,
			"response_model":  request.ResponseModel,
			"response_schema": request.ResponseSchema,
		})
		return fmt.Errorf("本地 Skill 调试不装配 Agent；%s/%s 请求决策 %s: %s",
			request.SkillName, request.Stage, request.DecisionKey, request.Reason)
	})
	runtime := pilot.NewSkillRuntime(
		skills,
		actionCatalog,
		pilot.NewRunner(actionCatalog, abilityClient, pilot.NewMemoryActionJournal()),
		pilot.NewMemorySkillExecutionStore(),
		pilot.WorkerSupervisor{
			PythonExecutable: *python,
			PythonPaths:      append([]string(nil), pythonPaths...),
			OnStderrLine: func(line string) {
				events.Log(pilot.SkillExecution{SkillName: name}, "warn", "Robot Skill Worker stderr", map[string]any{"line": line})
			},
		},
		agent,
		nil,
		events,
	)

	runContext, stopSignal, stopErr := stopport.NotifyContext(context.Background())
	if stopErr != nil {
		return stopErr
	}
	defer stopSignal()
	if *timeout > 0 {
		var cancel context.CancelFunc
		runContext, cancel = context.WithTimeout(runContext, *timeout)
		defer cancel()
	}
	execution, err := runtime.Start(runContext, pilot.SkillStartRequest{
		ExecutionID: *executionID,
		RobotID:     deployment.Robot.ID,
		SkillName:   name,
		Version:     version,
		Input:       input,
	})
	if err != nil {
		return err
	}
	final, waitErr := runtime.Wait(runContext, execution.ID)
	if waitErr != nil {
		// Ctrl+C 和超时都必须沿用正式 Skill stop 链。直接退出进程会留下仍在
		// 运动的 Ability invocation，也无法判断 Robot 是否已经 hold。
		stopContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stopped, stopErr := runtime.Stop(stopContext, execution.ID, "local_debug", waitErr.Error(), "safe")
		cancel()
		final = stopped
		if stopErr != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("安全停止失败: %w", stopErr))
		}
	}
	if err := writeLocalSkillResult(*resultPath, final); err != nil {
		return err
	}
	if waitErr != nil {
		return waitErr
	}
	if final.Status != pilot.SkillCompleted {
		return fmt.Errorf("Skill 终态为 %s", final.Status)
	}
	return nil
}

func splitSkillReference(value string) (string, string, error) {
	trimmed := strings.TrimSpace(value)
	index := strings.LastIndex(trimmed, "@")
	if index <= 0 || index == len(trimmed)-1 {
		return "", "", errors.New("--skill 必须使用 name@version，例如 grasp-object@0.2.0")
	}
	return trimmed[:index], trimmed[index+1:], nil
}

func readJSONObject(path string, stdin io.Reader) (map[string]any, error) {
	var content []byte
	var err error
	if path == "-" {
		content, err = io.ReadAll(io.LimitReader(stdin, 4<<20))
	} else {
		content, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(content, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("输入必须是 JSON object")
	}
	return value, nil
}

func openLocalSkillEvents(path string) (*localSkillEventWriter, func(), error) {
	if path == "" {
		return &localSkillEventWriter{encoder: json.NewEncoder(io.Discard)}, func() {}, nil
	}
	if path == "-" {
		return &localSkillEventWriter{encoder: json.NewEncoder(os.Stderr)}, func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, func() {}, fmt.Errorf("创建事件输出目录: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, func() {}, fmt.Errorf("打开事件输出: %w", err)
	}
	return &localSkillEventWriter{encoder: json.NewEncoder(file)}, func() { _ = file.Close() }, nil
}

func writeLocalSkillResult(path string, execution pilot.SkillExecution) error {
	value := map[string]any{
		"execution_id": execution.ID, "robot_id": execution.RobotID,
		"skill_name": execution.SkillName, "skill_version": execution.SkillVersion,
		"status": execution.Status, "result": execution.Result, "error": execution.Error,
		"stop_outcome": execution.StopOutcome, "created_at": execution.CreatedAt,
		"updated_at": execution.UpdatedAt,
	}
	var writer io.Writer = os.Stdout
	closeWriter := func() {}
	if path != "-" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("创建结果输出目录: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			return fmt.Errorf("打开结果输出: %w", err)
		}
		writer = file
		closeWriter = func() { _ = file.Close() }
	}
	defer closeWriter()
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
