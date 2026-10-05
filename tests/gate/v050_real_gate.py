#!/usr/bin/env python3
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

"""v0.5 真实 Semantic Server、双 Pilot、双 AbilityFramework 纵向 Gate。"""

from __future__ import annotations

import argparse
import base64
import json
import logging
import os
import shutil
import socket
import sqlite3
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path
from typing import Any

from workspace import GateLayout, WorkspaceError, resolve_gate_layout, workspace_roots


ABILITY_PROJECTS = (
    "r1pro-navigation",
    "r1pro-manipulator-motion",
    "r1pro-end-effector",
    "r1pro-robot-state",
    "r1pro-sensor-capture",
    "r1pro-object-perception",
    "r1pro-grasp-planning",
)
SKILL_PROJECTS = {
    "grasp-object": "grasp_object",
    "semantic-navigation": "semantic_navigation",
    "place-object": "place_object",
}
ROBOT_A = "r1pro-fake-01"
ROBOT_B = "r1pro-fake-02"
PILOT_A = "pilot-r1pro-fake-01"
PILOT_B = "pilot-r1pro-fake-02"
TERMINAL_EXECUTIONS = {"completed", "failed", "stopped", "interrupted"}
TERMINAL_WORKFLOWS = {"completed", "failed", "stopped"}


class GateError(RuntimeError):
    """真实 Gate 的某个可验收条件没有满足。"""


class V050RealGate:
    """只编排发布制品和真实进程，不替代任何产品模块。"""

    def __init__(self, output_dir: Path) -> None:
        self.framework_repo = Path(__file__).resolve().parents[2]
        self.root = workspace_roots(self.framework_repo)[0]
        self.sdk_repo = Path()
        self.ability_repo = Path()
        self.skills_repo = Path()
        self.deployment_repo = Path()
        self.web_repo = Path()
        self.vendor_root = Path()
        self.ability_framework = Path()
        self.ability_py_wheel = Path()
        self.ability_scaffold = Path()
        self.output_dir = output_dir.resolve()
        self.runtime_dir = self.output_dir / "runtime"
        self.artifacts_dir = self.runtime_dir / "artifacts"
        self.ability_packages = self.artifacts_dir / "abilities"
        self.skill_packages = self.artifacts_dir / "skills"
        self.wheels_dir = self.artifacts_dir / "wheels"
        self.bin_dir = self.runtime_dir / "bin"
        self.server_dir = self.runtime_dir / "server"
        self.bundle_dir = self.runtime_dir / "bundles" / "r1pro-fake-0.5.0-dev"
        self.robots_dir = self.runtime_dir / "robots"
        self.server_binary = self.bin_dir / "semantic-server"
        self.pilot_binary = self.bin_dir / "semantic-pilot"
        self.http_port, self.ws_port, af_a, af_b = self._free_ports(4)
        self.af_ports = {ROBOT_A: af_a, ROBOT_B: af_b}
        self.http_base = f"http://127.0.0.1:{self.http_port}"
        self.ws_base = f"ws://127.0.0.1:{self.ws_port}"
        self.token = ""
        self.project_id = ""
        self.conversation_id = ""
        self.processes: list[tuple[str, subprocess.Popen[bytes], Any]] = []
        self.instance_processes: dict[str, subprocess.Popen[bytes]] = {}
        self.execution_evidence: list[dict[str, Any]] = []
        self.web_execution_id = ""
        self.current_step = "initialize"
        self.summary: dict[str, Any] = {
            "status": "failed",
            "deployment_commit": "",
            "bundle": {},
            "devices": {},
            "skills": [],
            "executions": [],
            "evidence": {},
            "web": {},
            "safe_stop": {},
            "shutdown": {},
            "failed_step": None,
            "error": None,
        }

    def run(self) -> None:
        try:
            self._prepare_output()
            self._check_inputs()
            self._build_artifacts()
            self._build_shared_bundle()
            self._start_server()
            self._publish_skills()
            self._start_robot_instances()
            self._wait_devices_ready()
            self._wait_skills(installed=True, enabled=True)
            self._create_project()
            self._run_normal_robot_chain()
            self._run_safe_stop_and_isolation()
            self._collect_final_evidence()
            self._run_web_gate()
            self.summary["status"] = "passed"
            logging.info("v0.5 真实双 Robot Gate 已通过")
        except BaseException as exc:
            self.summary["failed_step"] = self.current_step
            self.summary["error"] = str(exc)
            raise
        finally:
            passed_before_shutdown = self.summary["status"] == "passed"
            shutdown_errors = self._stop_processes()
            if shutdown_errors:
                self.summary["status"] = "failed"
                if passed_before_shutdown:
                    self.summary["failed_step"] = "shutdown_robot_instances"
                    self.summary["error"] = "; ".join(shutdown_errors)
            self._write_json(self.output_dir / "gate-summary.json", self.summary)
            # 功能链完成不代表实例可以安全释放；关闭失败必须使 Gate 失败。
            # 原流程已经失败时保留其根因，不用清理错误覆盖首个故障。
            if passed_before_shutdown and shutdown_errors:
                raise GateError("Robot 实例安全关闭失败：" + "; ".join(shutdown_errors))

    def _bind_workspace(self) -> GateLayout:
        """解析旁路仓库和 Ability 制品；缺路径时失败，不回退到个人目录。"""
        try:
            layout = resolve_gate_layout(self.framework_repo)
        except WorkspaceError as exc:
            raise GateError(str(exc)) from exc
        self.root = layout.root
        self.sdk_repo = layout.sdk
        self.ability_repo = layout.ability
        self.skills_repo = layout.skills
        self.deployment_repo = layout.deployment
        self.web_repo = layout.web
        self.vendor_root = layout.vendor
        self.ability_framework = layout.ability_framework
        self.ability_py_wheel = layout.ability_py_wheel
        self.ability_scaffold = layout.ability_scaffold
        return layout

    def _prepare_output(self) -> None:
        self.current_step = "prepare_output"
        if self.output_dir.exists():
            # bundle 构建完成后有意设为只读。这里只恢复本 Gate 输出目录的
            # 目录写权限；不跟随 venv 中指向系统解释器的符号链接。
            for root, directories, _files in os.walk(self.output_dir):
                os.chmod(root, 0o700)
                for name in directories:
                    path = Path(root) / name
                    if not path.is_symlink():
                        os.chmod(path, 0o700)
            shutil.rmtree(self.output_dir)
        for directory in (
            self.ability_packages,
            self.skill_packages,
            self.wheels_dir,
            self.bin_dir,
            self.server_dir,
            self.robots_dir,
            self.output_dir / "executions",
        ):
            directory.mkdir(parents=True, exist_ok=True)

    def _check_inputs(self) -> None:
        self.current_step = "check_inputs"
        self._bind_workspace()
        commit = self._capture(("git", "rev-parse", "--short", "HEAD"), self.deployment_repo).strip()
        self.summary["deployment_commit"] = commit
        required = (
            self.ability_framework,
            self.ability_py_wheel,
            self.ability_scaffold,
            self.deployment_repo / "type-packages" / "r1pro-fake" / "bundle.yaml",
            self.web_repo / "tests" / "integration" / "devices-framework-v050.spec.js",
        )
        missing = [str(path) for path in required if not path.exists()]
        if missing:
            raise GateError("缺少 Gate 输入：" + ", ".join(missing))

    def _build_artifacts(self) -> None:
        self.current_step = "build_artifacts"
        log = self.output_dir / "build.log"
        ability_env = os.environ.copy()
        ability_env["ROBOT_SDK_PATH"] = str(self.sdk_repo)
        self._run(("make", "build"), self.sdk_repo, log)
        self._run(("make", "build"), self.ability_repo, log, append=True, env=ability_env)
        self._run(("make", "build"), self.deployment_repo, log, append=True)
        self._run(("go", "build", "-o", str(self.server_binary), "./cmd/semantic-server"),
                  self.framework_repo, log, append=True)
        self._run(("go", "build", "-o", str(self.pilot_binary), "./cmd/semantic-pilot"),
                  self.framework_repo, log, append=True)

        # SDK Wheel 只含 Runtime SDK；三个具体 Skill 始终另打 Zip 并经 Registry 安装。
        self._run((sys.executable, "-m", "pip", "wheel", "--no-deps", "--no-build-isolation",
                   "--wheel-dir", str(self.wheels_dir), str(self.skills_repo)),
                  self.framework_repo, log, append=True)

        scaffold = self.ability_scaffold
        for project in ABILITY_PROJECTS:
            source = self.ability_repo / "abilities" / project
            target = self.ability_packages / f"{project}.zip"
            self._run((str(scaffold), "pack", str(source), "-o", str(target)),
                      self.framework_repo, log, append=True)
        for name, directory in SKILL_PROJECTS.items():
            self._zip_tree(
                self.skills_repo / "semantic_robot_skills" / "skills" / directory,
                self.skill_packages / f"{name}-0.1.0.zip",
            )

    def _build_shared_bundle(self) -> None:
        self.current_step = "build_shared_bundle"
        bundle = self.deployment_repo / "bin" / "semantic-robot-bundle"
        skill_sdk = self._single_wheel(self.wheels_dir, "semantic_robot_skill_sdk-*.whl")
        mappings = [
            f"bin/semantic-robot-instance={self.deployment_repo / 'bin' / 'semantic-robot-instance'}",
            f"bin/AbilityFramework={self.ability_framework}",
            f"bin/semantic-pilot={self.pilot_binary}",
            f"wheels/ability_py-0.4.0-py3-none-any.whl={self.ability_py_wheel}",
            f"wheels/semantic_robot_sdk_core-0.5.0.dev0-py3-none-any.whl={self._single_wheel(self.sdk_repo / 'dist', 'semantic_robot_sdk_core-*.whl')}",
            f"wheels/semantic_robot_sdk_r1pro-0.5.0.dev0-py3-none-any.whl={self._single_wheel(self.sdk_repo / 'dist', 'semantic_robot_sdk_r1pro-*.whl')}",
            f"wheels/semantic_r1pro_abilities-0.1.0.dev0-py3-none-any.whl={self._single_wheel(self.ability_repo / 'dist', 'semantic_r1pro_abilities-*.whl')}",
            f"wheels/semantic_robot_skill_sdk-0.1.0.dev0-py3-none-any.whl={skill_sdk}",
        ]
        mappings.extend(
            f"abilities/{project}.zip={self.ability_packages / f'{project}.zip'}"
            for project in ABILITY_PROJECTS
        )
        command = [str(bundle), "build", "--source",
                   str(self.deployment_repo / "type-packages" / "r1pro-fake"),
                   "--output", str(self.bundle_dir), "--python", sys.executable]
        for mapping in mappings:
            command.extend(("--file", mapping))
        self._run(tuple(command), self.framework_repo, self.output_dir / "bundle.log")
        inspected = self._capture_json((str(bundle), "inspect", "--bundle", str(self.bundle_dir)),
                                       self.framework_repo)
        self.summary["bundle"] = inspected
        skill_names = {path.name for path in (self.bundle_dir / "wheels").glob("*.whl")}
        if not any(name.startswith("semantic_robot_skill_sdk-") for name in skill_names):
            raise GateError("共享 bundle 缺少 SDK-only Robot Skill Wheel")
        for concrete in SKILL_PROJECTS:
            if any(concrete in str(path) for path in self.bundle_dir.rglob("*")):
                raise GateError(f"具体 Skill {concrete} 不得预装进共享 bundle")

    def _start_server(self) -> None:
        self.current_step = "start_semantic_server"
        script = self._mock_script()
        config = self.server_dir / "semantic-server.yaml"
        config.write_text(
            f'''server:\n  http_addr: "127.0.0.1:{self.http_port}"\n  ws_addr: "127.0.0.1:{self.ws_port}"\n  read_timeout: 30s\n  write_timeout: 30s\nlog:\n  level: info\nstore:\n  driver: sqlite\n  sqlite_path: {self.server_dir / "semantic.db"}\nllm:\n  default: mock\n  providers:\n    mock:\n      component: mock\n      model: "mock"\n      capabilities: [text, tool_call]\n      price: {{prompt: 0, completion: 0}}\nagents:\n  profiles_dir: {self.framework_repo / "configs" / "agents"}\n  teams_dir: {self.framework_repo / "configs" / "agents" / "teams"}\nskills:\n  dir: {self.framework_repo / "configs" / "skills"}\nexecution:\n  allow_host: false\nmcp_servers: []\n''',
            encoding="utf-8",
        )
        env = os.environ.copy()
        env.update({
            "SEMANTIC_ADMIN_PASSWORD": "v050-real-gate-admin",
            "SEMANTIC_MOCK_SCRIPT": json.dumps(script, ensure_ascii=False),
        })
        process, stream = self._start_process(
            "semantic-server", (str(self.server_binary), "-c", str(config)),
            self.framework_repo, self.output_dir / "server.log", env,
        )
        self.processes.append(("semantic-server", process, stream))
        self._wait_http(f"{self.http_base}/api/v1/system/healthz", process, 20)
        response = self._request_json("POST", "/api/v1/auth/login", {
            "username": "admin", "password": "v050-real-gate-admin",
        }, authenticated=False)
        self.token = str(response["token"])

    def _start_robot_instances(self) -> None:
        self.current_step = "start_robot_instances"
        launcher = self.bundle_dir / "bin" / "semantic-robot-instance"
        for robot_id, pilot_id in ((ROBOT_A, PILOT_A), (ROBOT_B, PILOT_B)):
            enrollment = self._request_json(
                "POST", "/api/v1/pilot-enrollments", {}, expected=(201,)
            )["enrollment"]
            join_code = str(enrollment["code"])
            instance_dir = self.robots_dir / robot_id
            deployment = self.runtime_dir / f"{robot_id}-deployment.yaml"
            deployment.write_text(
                self._robot_deployment_yaml(
                    robot_id, pilot_id, auto_complete=robot_id != ROBOT_A
                ),
                encoding="utf-8",
            )
            command = (
                str(launcher), "start", "--config", str(deployment),
                "--data-dir", str(instance_dir), "--bundle", str(self.bundle_dir),
                "--join-code", join_code, "--server-http", self.http_base,
                "--server-ws", f"{self.ws_base}/ws/pilot",
            )
            process, stream = self._start_process(
                f"instance-{robot_id}", command, self.framework_repo,
                self.output_dir / f"{robot_id}-instance.log", os.environ.copy(),
            )
            self.processes.append((f"instance-{robot_id}", process, stream))
            self.instance_processes[robot_id] = process
    def _wait_devices_ready(self) -> None:
        self.current_step = "wait_two_devices"
        deadline = time.monotonic() + 75
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            self._ensure_processes_alive()
            body = self._request_json("GET", "/api/v1/devices/snapshot")
            last = body.get("snapshot", {})
            devices = {item.get("robot_id"): item for item in last.get("robots", [])}
            if set((ROBOT_A, ROBOT_B)).issubset(devices):
                ready = True
                identities: set[str] = set()
                for robot_id in (ROBOT_A, ROBOT_B):
                    device = devices[robot_id]
                    abilities = device.get("abilities") or []
                    ready = ready and device.get("pilot", {}).get("status") == "online"
                    ready = ready and device.get("ability_framework", {}).get("status") == "ready"
                    ready = ready and len(abilities) == 7
                    for item in abilities:
                        identities.add(str(item.get("instance_id") or item.get("instance_uuid")))
                if ready and len(identities) == 14:
                    self.summary["devices"]["ready_snapshot"] = last
                    return
            time.sleep(0.5)
        raise GateError(f"两个 Pilot/十四 Ability 未全部 ready：{last}")

    def _publish_skills(self) -> None:
        self.current_step = "publish_robot_skills"
        for name in SKILL_PROJECTS:
            archive = self.skill_packages / f"{name}-0.1.0.zip"
            self._request_bytes(
                "POST", "/api/v1/robot-skills", archive.read_bytes(),
                content_type="application/zip", expected=(201,),
            )
    def _wait_skills(self, *, installed: bool, enabled: bool) -> None:
        deadline = time.monotonic() + 90
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            last = self._request_json("GET", "/api/v1/devices/snapshot")["snapshot"]
            devices = {item.get("robot_id"): item for item in (last.get("robots") or [])}
            ok = True
            for robot_id in (ROBOT_A, ROBOT_B):
                skills = {(item.get("name"), item.get("version")): item
                          for item in (devices.get(robot_id, {}).get("installed_skills") or [])}
                for name in SKILL_PROJECTS:
                    item = skills.get((name, "0.1.0"), {})
                    if item.get("status") == "failed":
                        raise GateError(
                            f"{robot_id} 安装 {name} 失败：{item.get('error') or 'Pilot 未提供错误'}"
                        )
                    ok = ok and (not installed or item.get("status") == "installed")
                    ok = ok and (item.get("enabled") is enabled)
            if ok:
                self.summary["skills"] = last.get("skill_packages") or []
                return
            time.sleep(0.5)
        raise GateError(f"Skill 安装/启用未完成：{last}")

    def _create_project(self) -> None:
        self.current_step = "create_project"
        project = self._request_json(
            "POST", "/api/v1/projects", {"name": "v0.5 双 Robot Gate"}, expected=(201,)
        )["project"]
        self.project_id = str(project["id"])
        conversation = self._request_json(
            "POST", f"/api/v1/projects/{self.project_id}/conversations",
            {"title": "真实 Robot Agent Gate"}, expected=(201,),
        )["conversation"]
        self.conversation_id = str(conversation["id"])

    def _run_normal_robot_chain(self) -> None:
        self.current_step = "normal_robot_chain"
        for case in ("b-grasp", "b-navigation", "b-place"):
            execution = self._run_robot_workflow(case)
            if execution.get("status") != "completed":
                raise GateError(f"{case} Robot Execution 未完成：{execution}")
            self.execution_evidence.append(execution)

    def _run_safe_stop_and_isolation(self) -> None:
        self.current_step = "safe_stop_and_isolation"
        execution = self._start_robot_workflow("a-stop-navigation")
        execution_id = self._wait_execution_created("gate-a-stop-navigation")
        self._wait_action_started(execution_id, "navigation.follow_route")
        self._request_json("POST", f"/api/v1/robot-executions/{execution_id}/stop",
                           {"reason": "v0.5 Gate 安全停止"})
        stopped = self._wait_execution_terminal(execution_id, timeout=45)
        if stopped.get("status") != "stopped":
            raise GateError(f"Robot A 未确认 stopped：{stopped}")
        detail = self._execution_detail(execution_id)
        finalized = [event for event in detail.get("events", [])
                     if event.get("type") == "skill.stop.finalized"]
        if not finalized:
            raise GateError("Robot A 缺少 skill.stop.finalized 安全证据")
        fake_state = self._fake_robot_state(ROBOT_A)
        if not fake_state.get("state", {}).get("in_hold"):
            raise GateError(f"Robot A SDK 未进入 hold：{fake_state}")
        self.summary["safe_stop"] = {
            "execution_id": execution_id,
            "status": "stopped",
            "robot_in_hold": True,
            "event": finalized[-1],
        }
        self.execution_evidence.append(stopped)
        self._wait_workflow_terminal(execution)

        # A 的停止不能影响 B 的 AF、Pilot、锁或后续执行。
        post_stop = self._run_robot_workflow("b-after-a-stop")
        if post_stop.get("status") != "completed":
            raise GateError(f"Robot B 在 A 停止后不可执行：{post_stop}")
        self.execution_evidence.append(post_stop)
        snapshot = self._request_json("GET", "/api/v1/devices/snapshot")["snapshot"]
        devices = {item["robot_id"]: item for item in snapshot["robots"]}
        b = devices[ROBOT_B]
        if b.get("pilot", {}).get("status") != "online" or b.get("ability_framework", {}).get("status") != "ready":
            raise GateError(f"Robot B 被 A 停止影响：{b}")

    def _run_robot_workflow(self, case: str) -> dict[str, Any]:
        workflow_id = self._start_robot_workflow(case)
        request_key = f"gate-{case}"
        execution_id = self._wait_execution_created(request_key)
        execution = self._wait_execution_terminal(execution_id, timeout=90)
        self._wait_workflow_terminal(workflow_id)
        return execution

    def _start_robot_workflow(self, case: str) -> str:
        # Gate 必须走与 Studio 相同的 Conversation Plan Mode：Proposal 是批准前
        # 唯一可审阅对象，不能再调用已经下线的“直接创建 Workflow”兼容接口。
        self._send_plan_message(f"PLAN-{case}")
        deadline = time.monotonic() + 20
        proposal: dict[str, Any] = {}
        while time.monotonic() < deadline:
            try:
                body = self._request_json(
                    "GET", f"/api/v1/projects/{self.project_id}/plan-proposals/active"
                )
                proposal = body.get("plan_proposal") or {}
                if proposal.get("status") == "ready":
                    break
            except GateError as exc:
                if "返回 404" not in str(exc):
                    raise
            time.sleep(0.2)
        if proposal.get("status") != "ready":
            raise GateError(f"Plan Mode 未生成 ready Proposal：{proposal}")
        response = self._request_json(
            "POST",
            f"/api/v1/projects/{self.project_id}/plan-proposals/{proposal['id']}/approve",
            {"revision": int(proposal["revision"])},
        )
        workflow = (response.get("workflow_view") or {}).get("workflow") or {}
        workflow_id = str(workflow.get("id") or "")
        if not workflow_id:
            raise GateError(f"批准 Proposal 未原子创建 Workflow：{response}")
        return workflow_id

    def _wait_execution_created(self, request_key: str) -> str:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            self._approve_pending_interactions()
            body = self._request_json("GET", f"/api/v1/projects/{self.project_id}/robot-executions")
            for item in (body.get("executions") or []):
                if item.get("request_key") == request_key:
                    return str(item["id"])
            time.sleep(0.2)
        raise GateError(f"robot.run 未建立 Execution：{request_key}")

    def _wait_execution_terminal(self, execution_id: str, *, timeout: float) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self._approve_pending_interactions()
            detail = self._execution_detail(execution_id)
            execution = detail["execution"]
            if execution.get("status") in TERMINAL_EXECUTIONS:
                self._write_json(self.output_dir / "executions" / f"{execution_id}.json", detail)
                return execution
            time.sleep(0.25)
        raise GateError(f"Robot Execution 未终止：{self._execution_detail(execution_id)}")

    def _wait_action_started(self, execution_id: str, action_type: str) -> None:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            detail = self._execution_detail(execution_id)
            for event in detail.get("events", []):
                if event.get("type") == "action.started" and event.get("payload", {}).get("action_type") == action_type:
                    return
            time.sleep(0.2)
        raise GateError(f"Execution {execution_id} 未启动 {action_type}")

    def _wait_workflow_terminal(self, workflow_id: str) -> None:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            self._approve_pending_interactions()
            workflow = self._workflow_view(workflow_id)["workflow"]
            if workflow.get("status") in TERMINAL_WORKFLOWS:
                return
            time.sleep(0.2)
        raise GateError(f"Workflow 未终止：{self._workflow_view(workflow_id)}")

    def _workflow_view(self, workflow_id: str) -> dict[str, Any]:
        return self._request_json(
            "GET", f"/api/v1/projects/{self.project_id}/workflows/{workflow_id}/view"
        )["workflow_view"]

    def _approve_pending_interactions(self) -> None:
        body = self._request_json(
            "GET", f"/api/v1/interactions?status=pending&project_id={urllib.parse.quote(self.project_id)}"
        )
        for item in body.get("interactions", []):
            interaction_id = str(item.get("id") or item.get("interaction_id") or "")
            if interaction_id:
                self._studio_reply(
                    interaction_id,
                    int(item.get("source_revision") or 0),
                )

    def _send_plan_message(self, text: str) -> None:
        query = urllib.parse.urlencode({"token": self.token, "session_id": self.conversation_id})
        self._send_ws_json(f"/ws/chat?{query}", {
            "type": "chat.message",
            "session_id": self.conversation_id,
            "text": text,
            "send_scope": {"type": "conversation", "intent": "plan"},
        }, wait_for_server=True)

    def _studio_reply(self, interaction_id: str, source_revision: int = 0) -> None:
        query = urllib.parse.urlencode({"token": self.token, "project_id": self.project_id})
        self._send_ws_json(f"/ws/studio?{query}", {
            "type": "interaction.reply",
            "interaction_id": interaction_id,
            # Interaction 的来源对象可能在提问后继续更新。Gate 与真实 Web
            # 一样回传创建问题时的 revision，让 Server 能拒绝对过期问题的
            # 应答；不能为了测试方便绕开生产 revision 边界。
            "expected_state_revision": source_revision,
            "approved": True,
        }, wait_for_server=True)

        # Studio 协议通过 interaction.resolved 事件确认成功，而不是为命令
        # 单独返回 ACK。短连接 Gate 必须等服务端实际消费消息，并再由 HTTP
        # 读取持久状态；只执行 send 后立即 close 会制造“界面已回复、数据库仍
        # pending”的假失败，真实 Web 使用的长连接不会有这个窗口。
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            body = self._request_json(
                "GET",
                f"/api/v1/interactions?status=pending&project_id={urllib.parse.quote(self.project_id)}",
            )
            pending_ids = {
                str(item.get("id") or item.get("interaction_id") or "")
                for item in body.get("interactions", [])
            }
            if interaction_id not in pending_ids:
                return
            time.sleep(0.05)
        raise GateError(f"Interaction {interaction_id} 回复未持久化")

    def _send_ws_json(self, path: str, value: dict[str, Any], *, wait_for_server: bool = False) -> None:
        """发送一条短控制消息；二进制 Artifact 不经过 WebSocket。"""
        sock = socket.create_connection(("127.0.0.1", self.ws_port), timeout=3)
        try:
            key = base64.b64encode(os.urandom(16)).decode("ascii")
            request = (
                f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1:{self.ws_port}\r\n"
                f"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
                "Sec-WebSocket-Version: 13\r\n\r\n"
            )
            sock.sendall(request.encode("ascii"))
            response = b""
            while b"\r\n\r\n" not in response:
                response += sock.recv(4096)
            if b" 101 " not in response.split(b"\r\n", 1)[0]:
                raise GateError(f"WebSocket 升级失败：{response[:200]!r}")
            payload = json.dumps(value, ensure_ascii=False).encode("utf-8")
            mask = os.urandom(4)
            header = bytearray([0x81])
            if len(payload) < 126:
                header.append(0x80 | len(payload))
            else:
                header.append(0x80 | 126)
                header.extend(struct.pack("!H", len(payload)))
            masked = bytes(item ^ mask[index % 4] for index, item in enumerate(payload))
            sock.sendall(bytes(header) + mask + masked)
            if wait_for_server:
                reply = self._receive_ws_json(sock)
                if reply.get("type") == "error":
                    raise GateError(f"WebSocket 消息处理失败：{reply}")
        finally:
            sock.close()

    @staticmethod
    def _receive_ws_json(sock: socket.socket) -> dict[str, Any]:
        """等待服务端首个数据帧，确认上行消息已进入正式处理链。"""

        def receive_exact(length: int) -> bytes:
            chunks = bytearray()
            while len(chunks) < length:
                chunk = sock.recv(length - len(chunks))
                if not chunk:
                    raise GateError("WebSocket 在确认上行消息前断开")
                chunks.extend(chunk)
            return bytes(chunks)

        sock.settimeout(5)
        first, second = receive_exact(2)
        opcode = first & 0x0F
        length = second & 0x7F
        if length == 126:
            length = struct.unpack("!H", receive_exact(2))[0]
        elif length == 127:
            length = struct.unpack("!Q", receive_exact(8))[0]
        if second & 0x80:
            mask = receive_exact(4)
            payload = bytes(
                item ^ mask[index % 4]
                for index, item in enumerate(receive_exact(length))
            )
        else:
            payload = receive_exact(length)
        if opcode == 0x8:
            raise GateError("WebSocket 在确认上行消息前被服务端关闭")
        if opcode != 0x1:
            raise GateError(f"WebSocket 返回非文本确认帧：opcode={opcode}")
        decoded = json.loads(payload.decode("utf-8"))
        return decoded if isinstance(decoded, dict) else {"value": decoded}

    def _execution_detail(self, execution_id: str) -> dict[str, Any]:
        after = 0
        combined: list[dict[str, Any]] = []
        execution: dict[str, Any] = {}
        while True:
            response = self._request_json(
                "GET",
                f"/api/v1/robot-executions/{execution_id}?after_sequence={after}",
            )
            execution = response.get("execution") or execution
            events = response.get("events") or []
            combined.extend(events)
            if not response.get("has_more"):
                return {"execution": execution, "events": combined}
            next_sequence = int(response.get("next_sequence") or 0)
            if next_sequence <= after:
                raise GateError(
                    f"Robot Execution事件分页没有前进: {execution_id} after={after}"
                )
            after = next_sequence

    def _collect_final_evidence(self) -> None:
        self.current_step = "collect_evidence"
        snapshot = self._request_json("GET", "/api/v1/devices/snapshot")["snapshot"]
        self.summary["devices"]["final_snapshot"] = snapshot
        self.summary["executions"] = self.execution_evidence
        self._write_json(self.output_dir / "devices-snapshot.json", snapshot)
        identities: dict[str, set[str]] = {}
        for device in snapshot.get("robots", []):
            identities[device["robot_id"]] = {
                str(item.get("instance_id") or item.get("instance_uuid"))
                for item in device.get("abilities", [])
            }
        if len(identities.get(ROBOT_A, set())) != 7 or len(identities.get(ROBOT_B, set())) != 7:
            raise GateError(f"最终设备快照缺少十四 Ability：{identities}")
        if identities[ROBOT_A] & identities[ROBOT_B]:
            raise GateError("两个 Robot 串用了 Ability instance ID")
        self._collect_execution_evidence(identities)
        for robot_id in (ROBOT_A, ROBOT_B):
            state_path = self.robots_dir / robot_id / "run" / "state.json"
            if state_path.exists():
                self._write_json(self.output_dir / f"{robot_id}-instance-state.json",
                                 json.loads(state_path.read_text(encoding="utf-8")))

    def _collect_execution_evidence(self, identities: dict[str, set[str]]) -> None:
        """逐条核对物理执行证据，避免只凭 Skill 最终状态判定 Gate 成功。"""
        counts = {"actions": 0, "feedback": 0, "observations": 0, "artifacts": 0}
        allowed_terminal = {"succeeded", "failed", "stopped", "interrupted"}

        for execution in self.execution_evidence:
            execution_id = str(execution["id"])
            detail = self._execution_detail(execution_id)
            events = detail.get("events") or []
            started = [item for item in events if item.get("type") == "action.started"]
            terminal = [item for item in events if item.get("type") == "action.terminal"]
            started_ids = {str(item.get("payload", {}).get("action_id") or "") for item in started}
            terminal_ids = {str(item.get("payload", {}).get("action_id") or "") for item in terminal}
            if not started or started_ids != terminal_ids:
                raise GateError(
                    f"{execution_id} Action 起止证据不闭合：started={started_ids}, terminal={terminal_ids}"
                )

            robot_id = str(detail.get("execution", {}).get("robot_id") or execution.get("robot_id"))
            ability_ids = identities.get(robot_id, set())
            for item in started:
                instance_id = str(item.get("payload", {}).get("ability_instance_id") or "")
                if not instance_id or instance_id not in ability_ids:
                    raise GateError(f"{execution_id} Action 未绑定本 Robot 的精确 Ability：{item}")
            for item in terminal:
                status = str(item.get("payload", {}).get("status") or "")
                if status not in allowed_terminal:
                    raise GateError(f"{execution_id} Action 终态无效：{item}")

            feedback = [item for item in events if item.get("type") == "feedback.emitted"]
            observations = [item for item in events if item.get("type") == "observation.recorded"]
            counts["actions"] += len(started)
            counts["feedback"] += len(feedback)
            counts["observations"] += len(observations)

            if detail.get("execution", {}).get("status") == "completed":
                # Skill 完成后 Pilot 才发布摘要 Artifact，上传与终态事件允许短暂并行。
                # Gate 在这里等待 Server 映射完成，确保 Web 读取的是长期 ArtifactRef。
                deadline = time.monotonic() + 15
                synced: list[dict[str, Any]] = []
                while time.monotonic() < deadline:
                    detail = self._execution_detail(execution_id)
                    synced = [
                        item for item in (detail.get("execution", {}).get("artifact_sync") or [])
                        if item.get("status") == "synced" and item.get("server_artifact_id")
                    ]
                    if synced:
                        break
                    time.sleep(0.2)
                if not synced:
                    raise GateError(f"{execution_id} 完成后没有已同步的 Server Artifact")
                counts["artifacts"] += len(synced)
                if robot_id == ROBOT_B and feedback and observations:
                    # 选择同时具有三类运行证据的 B Execution，保证设备页和
                    # Project Studio 验证的是完整时间线，而非只读一个终态。
                    self.web_execution_id = execution_id

            self._write_json(self.output_dir / "executions" / f"{execution_id}.json", detail)

        if counts["feedback"] == 0 or counts["observations"] == 0:
            raise GateError(f"完整 Robot Skill 链缺少 Feedback 或 Observation：{counts}")
        if not self.web_execution_id:
            raise GateError("没有可供真实 Web 验收的 Robot B Execution")
        self.summary["evidence"] = counts

    def _run_web_gate(self) -> None:
        """构建正式 Web 制品，并让 Playwright 通过同源代理读取当前真实 Server。"""
        self.current_step = "real_web_product_gate"
        env = os.environ.copy()
        env.update({
            "V050_FRAMEWORK_TOKEN": self.token,
            "V050_FRAMEWORK_HTTP": self.http_base,
            "V050_FRAMEWORK_WS": self.ws_base,
            "V050_EXPECT_ROBOT_ID": ROBOT_B,
            "V050_EXPECT_EXECUTION_ID": self.web_execution_id,
            "V050_EXPECT_PROJECT_ID": self.project_id,
            "V050_WEB_PORT": str(self._free_port()),
            "V050_PLAYWRIGHT_OUTPUT_DIR": str(self.output_dir / "web-playwright"),
        })
        log = self.output_dir / "web-playwright.log"
        self._run(("npm", "run", "build"), self.web_repo, log, env=env)
        self._run(("npm", "run", "test:framework:v050"), self.web_repo, log,
                  env=env, append=True)
        self.summary["web"] = {
            "status": "passed",
            "robot_id": ROBOT_B,
            "execution_id": self.web_execution_id,
            "project_id": self.project_id,
            "log": str(log),
        }

    def _fake_robot_state(self, robot_id: str) -> dict[str, Any]:
        database = self.robots_dir / robot_id / "pilot" / "robot-state.sqlite"
        with sqlite3.connect(database) as connection:
            row = connection.execute(
                "SELECT payload FROM fake_robot_state WHERE singleton=1"
            ).fetchone()
        if row is None:
            raise GateError(f"{robot_id} 没有 Fake Robot 状态")
        value = json.loads(row[0])
        self._write_json(self.output_dir / f"{robot_id}-fake-state.json", value)
        return value

    def _stop_processes(self) -> list[str]:
        errors: list[str] = []
        launcher = self.bundle_dir / "bin" / "semantic-robot-instance"
        for robot_id in (ROBOT_A, ROBOT_B):
            instance = self.robots_dir / robot_id
            if launcher.exists() and instance.exists():
                result = self._run(
                    (str(launcher), "stop", "--instance", str(instance), "--timeout", "30s"),
                    self.framework_repo, self.output_dir / f"{robot_id}-stop.log", check=False,
                )
                state_path = instance / "run" / "state.json"
                state = (json.loads(state_path.read_text(encoding="utf-8"))
                         if state_path.exists() else {})
                evidence = state.get("stop_evidence") or {}
                self.summary["shutdown"][robot_id] = {
                    "command_exit_code": result.returncode,
                    "instance_status": state.get("status"),
                    "stop_evidence": evidence,
                }
                if (result.returncode != 0 or state.get("status") != "stopped"
                        or evidence.get("pilot_exited_cleanly") is not True
                        or evidence.get("ability_stop_requested") != 7
                        or evidence.get("ability_stop_confirmed") != 7):
                    errors.append(
                        f"{robot_id} 未形成完整停止证据，详见 {robot_id}-stop.log"
                    )
        for _, process, stream in reversed(self.processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=3)
            stream.close()
        return errors

    def _mock_script(self) -> dict[str, Any]:
        cases = {
            "b-grasp": (ROBOT_B, "grasp-object", {
                "object_ref": "object://pallet-a/box-17",
                "gripper_ref": "component://gripper/main",
                "preferred_strategy": "top",
            }),
            # Robot Agent 把抓取形成的语义持物事实交给导航；Navigation Ability 会
            # 再读取共享 Robot SDK 状态并刷新证据，不能只相信这份输入声明。
            "b-navigation": (ROBOT_B, "semantic-navigation",
                             self._navigation_input("gate-nav-b", carrying=True)),
            "b-place": (ROBOT_B, "place-object", self._place_input()),
            "a-stop-navigation": (ROBOT_A, "semantic-navigation", self._navigation_input("gate-nav-a")),
            "b-after-a-stop": (ROBOT_B, "semantic-navigation", self._navigation_input("gate-nav-b-after")),
        }
        routes: list[dict[str, Any]] = []
        for case, (robot_id, skill, skill_input) in cases.items():
            execution_marker = f"EXEC-{case}"
            draft = {
                "goal": execution_marker,
                "summary": f"使用 {robot_id} 执行 {skill}",
                "approved_scope": {
                    "robot_ids": [robot_id],
                    "robot_models": ["r1pro"],
                    "allowed_skills": [skill],
                },
                "constraints": {},
                "completion_criteria": {"robot_execution_status": "completed"},
                "tasks": [{
                    "id": f"task-{case}", "required_role": "robot",
                    "required_capabilities": [skill],
                    "resource_requirements": {"robot_ids": [robot_id]},
                    "goal": execution_marker,
                    "input": {},
                    "completion_criteria": {"robot_execution_status": "completed"},
                }],
                "dependencies": [],
            }
            routes.append({
                "match": f"PLAN-{case}",
                "replies": [
                    {"tool_calls": [{
                        "id": f"plan-{case}", "name": "plan_suggest",
                        "arguments": json.dumps(draft, ensure_ascii=False),
                    }]},
                    {"content": "计划已可审阅。"},
                ],
            })
            routes.append({
                "match_all": ["请为这个 Worker Task", execution_marker],
                "replies": [{"content": json.dumps({"subtasks": [{
                    "id": f"subtask-{case}", "kind": "robot_skill", "goal": execution_marker,
                    "spec": {"skill_name": skill, "skill_version": "0.1.0",
                             "input": skill_input},
                    "completion_criteria": {"robot_execution_status": "completed"},
                    "depends_on": [],
                }]}, ensure_ascii=False)}],
            })
            tool_args = {
                "skill_name": skill,
                "skill_version": "0.1.0",
                "input": skill_input,
                "request_key": f"gate-{case}",
            }
            routes.append({
                "match_all": ["只推进 current_subtask", execution_marker],
                "replies": [
                    {"tool_calls": [{
                        "id": f"call-{case}", "name": "robot_run",
                        "arguments": json.dumps(tool_args, ensure_ascii=False),
                    }]},
                    {"content": json.dumps({
                        "kind": "result", "summary": f"已提交 {skill} Robot Execution",
                        "evidence": [],
                    }, ensure_ascii=False)},
                ],
            })
        routes.append({"match": "", "replies": [{"content": "{}"}]})
        return {"scripts": routes}

    @staticmethod
    def _pose(revision: str, x: float = 0.8, y: float = 0.2, z: float = 0.0) -> dict[str, Any]:
        return {
            "frame_id": "world", "position_m": [x, y, z],
            "orientation_xyzw": [0.0, 0.0, 0.0, 1.0],
            "observed_at": "2026-08-11T00:00:00Z", "revision": revision,
        }

    def _navigation_input(self, revision: str, *, carrying: bool = False) -> dict[str, Any]:
        value = {
            "target": {
                "target_ref": f"region://{revision}", "pose": self._pose(revision, 1.2, 0.4),
                "map_type": "simulation", "map_generation": "gate-generation-1",
                "map_revision": 1, "constraints": {},
            },
            "navigation_purpose": "transit", "arrival_radius_m": 0.5,
            "maximum_speed_mps": 0.4, "minimum_clearance_m": 0.25,
            "maximum_replans": 2, "require_visual_confirmation": False,
        }
        if carrying:
            value["carrying_object"] = self._held_input()
            value["navigation_purpose"] = "carry_to_place"
        return value

    def _held_input(self) -> dict[str, Any]:
        """描述 Robot Agent 传递的持物事实；每个 Skill 仍须向 Ability 实时复核。"""
        held_revision = "gate-held-before-place"
        return {
            "object_ref": "object://pallet-a/box-17", "robot_ref": ROBOT_B,
            "gripper_ref": "component://gripper/main",
            "grasp_pose": self._pose(held_revision, 0.8, 0.2, 0.35),
            "object_pose": self._pose(held_revision, 0.8, 0.2, 0.35),
            "object_size_m": [0.3, 0.2, 0.15], "grasp_candidate_id": "candidate-top-1",
            "grasp_confidence": 0.97, "estimated_mass_kg": 1.0,
            "verified_at": "2026-08-10T00:00:00Z", "robot_state_revision": held_revision,
            "evidence_refs": [],
        }

    def _place_input(self) -> dict[str, Any]:
        return {
            "held_object": self._held_input(),
            "target": {
                "target_ref": "slot://pallet-b/cell-01", "region_ref": "region://pallet-b",
                "support_surface_ref": "surface://pallet-b", "position_tolerance_m": 0.03,
                "orientation_tolerance_rad": 0.15, "stability_duration_ms": 800,
                "expected_scene_revision": "gate-scene-place-1",
            },
            "constraints": {},
        }

    def _robot_deployment_yaml(
        self, robot_id: str, pilot_id: str, *, auto_complete: bool
    ) -> str:
        auto_complete_yaml = "true" if auto_complete else "false"
        return f'''api_version: 1
robot:
  id: {robot_id}
  display_name: R1 Pro Fake {robot_id[-2:]}
  model: r1pro
  backend: fake
  sdk:
    package: semantic-robot-sdk-r1pro
    backend_profile: fake-v1
    firmware_profile: fake-v1
    providers:
      kinematics: fake
      motion: local
      navigation: local
    options:
      auto_complete: {auto_complete_yaml}
      stop_confirmed: true
      initial_grasp_targets:
        left:
          object_id: object://pallet-a/box-17
          contact_opening_m: 0.04
          contact_force_n: 18.0
          minimum_holding_force_n: 2.0
  frames:
    world: world
    base: base_link
    end_effector: gripper_link
  safety:
    maximum_base_speed: 0.4
    maximum_joint_speed: 0.5
ability_framework:
  endpoint: http://127.0.0.1:{self.af_ports[robot_id]}
  managed_by_instance: true
abilities:
  navigation: {{}}
  manipulator_motion: {{}}
  end_effector: {{}}
  robot_state: {{}}
  sensor_capture: {{}}
  object_perception: {{}}
  grasp_planning: {{}}
pilot:
  id: {pilot_id}
  heartbeat_interval_seconds: 2
  worker_timeout_seconds: 30
  allow_ability_debug: true
robot_skills:
  - name: grasp-object
    version: 0.1.0
    enabled: true
  - name: semantic-navigation
    version: 0.1.0
    enabled: true
  - name: place-object
    version: 0.1.0
    enabled: true
'''
    def _request_json(self, method: str, path: str, body: Any | None = None,
                      *, authenticated: bool = True,
                      expected: tuple[int, ...] = (200, 202)) -> dict[str, Any]:
        payload = None if body is None else json.dumps(body).encode("utf-8")
        raw = self._request_bytes(method, path, payload, content_type="application/json",
                                  authenticated=authenticated, expected=expected)
        return json.loads(raw.decode("utf-8")) if raw else {}

    def _request_bytes(self, method: str, path: str, body: bytes | None,
                       *, content_type: str, authenticated: bool = True,
                       expected: tuple[int, ...] = (200, 202)) -> bytes:
        request = urllib.request.Request(self.http_base + path, data=body, method=method)
        request.add_header("Content-Type", content_type)
        if authenticated:
            request.add_header("Authorization", "Bearer " + self.token)
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                data = response.read()
                if response.status not in expected:
                    raise GateError(f"{method} {path} 返回 {response.status}: {data[:500]!r}")
                return data
        except urllib.error.HTTPError as exc:
            raise GateError(f"{method} {path} 返回 {exc.code}: {exc.read()[:1000]!r}") from exc

    def _wait_http(self, url: str, process: subprocess.Popen[bytes], timeout: float) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise GateError(f"进程在 HTTP 就绪前退出：{process.returncode}")
            try:
                with urllib.request.urlopen(url, timeout=0.5) as response:
                    if response.status == 200:
                        return
            except (urllib.error.URLError, TimeoutError):
                time.sleep(0.1)
        raise GateError(f"等待 HTTP 超时：{url}")

    def _ensure_processes_alive(self) -> None:
        failed = [(name, process.returncode) for name, process, _ in self.processes
                  if process.poll() is not None]
        if failed:
            raise GateError(f"真实进程意外退出：{failed}")

    @staticmethod
    def _free_ports(count: int) -> tuple[int, ...]:
        """同时占住候选端口，避免连续探测返回同一个端口。"""

        sockets: list[socket.socket] = []
        try:
            for _ in range(count):
                sock = socket.socket()
                sock.bind(("127.0.0.1", 0))
                sockets.append(sock)
            ports = tuple(int(sock.getsockname()[1]) for sock in sockets)
            if len(set(ports)) != count:
                raise GateError(f"动态端口分配发生重复：{ports}")
            return ports
        finally:
            for sock in sockets:
                sock.close()

    @staticmethod
    def _single_wheel(directory: Path, pattern: str) -> Path:
        matches = sorted(directory.glob(pattern))
        if len(matches) != 1:
            raise GateError(f"{directory}/{pattern} 应恰好一个 Wheel，实际 {matches}")
        return matches[0]

    @staticmethod
    def _zip_tree(source: Path, target: Path) -> None:
        with zipfile.ZipFile(target, "w", zipfile.ZIP_DEFLATED) as archive:
            for path in sorted(source.rglob("*")):
                if not path.is_file() or "__pycache__" in path.parts or path.suffix in {".pyc", ".pyo"}:
                    continue
                archive.write(path, path.relative_to(source))

    @staticmethod
    def _write_json(path: Path, value: Any) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    @staticmethod
    def _start_process(name: str, command: tuple[str, ...], cwd: Path, log: Path,
                       env: dict[str, str]) -> tuple[subprocess.Popen[bytes], Any]:
        log.parent.mkdir(parents=True, exist_ok=True)
        stream = log.open("wb")
        stream.write(("$ " + " ".join(command) + "\n").encode())
        stream.flush()
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=stream,
                                   stderr=subprocess.STDOUT)
        logging.info("已启动 %s pid=%s", name, process.pid)
        return process, stream

    @staticmethod
    def _run(command: tuple[str, ...], cwd: Path, log: Path,
             *, env: dict[str, str] | None = None, append: bool = False,
             check: bool = True) -> subprocess.CompletedProcess[bytes]:
        log.parent.mkdir(parents=True, exist_ok=True)
        with log.open("ab" if append else "wb") as stream:
            stream.write(("\n$ " + " ".join(command) + "\n").encode())
            stream.flush()
            result = subprocess.run(command, cwd=cwd, env=env, stdout=stream,
                                    stderr=subprocess.STDOUT, check=False)
        if check and result.returncode != 0:
            raise GateError(f"命令失败({result.returncode}): {' '.join(command)}；日志 {log}")
        return result

    @staticmethod
    def _capture(command: tuple[str, ...], cwd: Path) -> str:
        return subprocess.check_output(command, cwd=cwd, text=True)

    @staticmethod
    def _capture_json(command: tuple[str, ...], cwd: Path) -> dict[str, Any]:
        return json.loads(V050RealGate._capture(command, cwd))


def main() -> int:
    parser = argparse.ArgumentParser(description="v0.5 real semantic-server dual Robot gate")
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    gate = V050RealGate(args.output_dir)
    try:
        gate.run()
    except BaseException as exc:
        logging.exception("Gate 失败：%s；证据目录：%s", exc, gate.output_dir)
        return 1
    logging.info("证据目录：%s", gate.output_dir)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
