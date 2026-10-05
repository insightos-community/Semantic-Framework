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

"""v0.5 原生 MuJoCo 单箱产品链 Gate。

这条 Gate 不直接调用 Skill、Ability 或 SDK。它从 Conversation Plan Mode
开始，经过 Proposal 批准、Task 后绑定、Robot Agent 和 robot.run，最后以
Runtime SceneSnapshot 验收真实物体状态。
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import socket
import subprocess
import time
import urllib.parse
from pathlib import Path
from typing import Any

from v050_real_gate import GateError, TERMINAL_WORKFLOWS, V050RealGate
from workspace import WorkspaceError, resolve_mujoco_asset, resolve_mujoco_runtime


ROBOT_ID = "r1_pro_tote_gripper-1"
SKILL_VERSIONS = {
    "semantic-navigation": "0.4.6",
    "grasp-object": "0.4.21",
    "place-object": "0.4.41",
}
SKILLS = tuple(SKILL_VERSIONS)
PLAN_MARKER = "PLAN-V050-MUJOCO-SINGLE-BOX"
TASK_MARKER = "MUJOCO-SINGLE-BOX"


class V050MujocoProductGate(V050RealGate):
    """使用真实原生 MuJoCo 数据平面验收确定性的产品编排链。"""

    def __init__(self, output_dir: Path) -> None:
        super().__init__(output_dir)
        self.asset_repo = Path()
        self.mujoco_repo = Path()
        self.bundle_dir = Path(os.environ.get(
            "SEMANTIC_MUJOCO_GATE_BUNDLE",
            self.framework_repo / ".output" / "robot-bundles" /
            "r1pro-mujoco-0.5.0-dev",
        )).resolve()
        self.runtimes_dir = Path(os.environ.get(
            "SEMANTIC_MUJOCO_GATE_RUNTIMES_DIR",
            self.framework_repo / "configs" / "runtimes.d",
        )).resolve()
        self.skill_packages = self.framework_repo / ".output" / "v050-mujoco-refresh" / "robot-skills"
        self.server_binary = self.framework_repo / ".output" / "bin" / "semantic-server"
        self.managed_data_root = self.output_dir / "managed-robot-runtime"
        self.scene_instance_id = ""
        self.workflow_id = ""
        self.robot_execution_ids: list[str] = []
        self.physical_samples: list[dict[str, Any]] = []
        self._last_physical_sample_at = float("-inf")
        self.ability_port_first, self.ability_port_last = self._free_port_range(12)
        self.summary = {
            "status": "failed",
            "scene": {},
            "device": {},
            "workflow": {},
            "executions": [],
            "physical_result": {},
            "shutdown": {},
            "failed_step": None,
            "error": None,
        }

    def run(self) -> None:
        try:
            self._prepare_output()
            self._check_product_inputs()
            self._start_product_server()
            self._publish_product_skills()
            self._create_product_project()
            self._start_smoke_scene()
            self._wait_robot_ready()
            self._run_single_box_workflow()
            self._verify_product_result()
            self._stop_scene_and_verify_terminal_state()
            self.summary["status"] = "passed"
            logging.info("v0.5 原生 MuJoCo 单箱产品 Gate 已通过")
        except BaseException as exc:
            self.summary["failed_step"] = self.current_step
            self.summary["error"] = str(exc)
            self._capture_failure_state()
            self._stop_failed_scene()
            raise
        finally:
            for _, process, stream in reversed(self.processes):
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
                stream.close()
            self._write_json(self.output_dir / "gate-summary.json", self.summary)

    def _stop_failed_scene(self) -> None:
        """失败时趁 Server 和 Runtime 仍在线走正式停止链，避免遗留孤儿 Ability。"""

        if not self.project_id or not self.scene_instance_id:
            return
        try:
            self._request_json(
                "POST",
                (
                    f"/api/v1/projects/{self.project_id}/simulation/instances/"
                    f"{self.scene_instance_id}/stop"
                ),
                {},
            )
        except BaseException as exc:
            logging.warning("Gate 失败后的 Scene stop 未完成: %s", exc)

    def _capture_failure_state(self) -> None:
        """在受管进程退出前保存真实场景状态，避免物理失败只能靠日志猜测。"""

        if not self.project_id or not self.scene_instance_id:
            return
        try:
            snapshot = self._request_json(
                "GET",
                (
                    f"/api/v1/projects/{self.project_id}/simulation/instances/"
                    f"{self.scene_instance_id}/snapshot"
                ),
            )["snapshot"]
            self._write_json(self.output_dir / "failure-scene-snapshot.json", snapshot)
            self.summary["failure_scene_snapshot"] = "failure-scene-snapshot.json"
            if self.physical_samples:
                self._write_json(
                    self.output_dir / "failure-motion-samples.json",
                    {"samples": self.physical_samples},
                )
                self.summary["failure_motion_samples"] = "failure-motion-samples.json"
        except BaseException as exc:  # noqa: BLE001 - 失败证据不能覆盖原始Gate错误
            logging.warning("保存失败场景Snapshot失败: %s", exc)

    def _check_product_inputs(self) -> None:
        self.current_step = "check_product_inputs"
        try:
            self.asset_repo = resolve_mujoco_asset(self.framework_repo)
            self.mujoco_repo = resolve_mujoco_runtime(self.framework_repo)
        except WorkspaceError as exc:
            raise GateError(str(exc)) from exc
        required = [
            self.server_binary,
            self.bundle_dir / "bin" / "semantic-robot-instance",
            self.framework_repo / "configs" / "runtimes.d" / "native-mujoco.yaml",
            self.framework_repo / "configs" / "scenes.d" / "mujoco-platforms.yaml",
            self.asset_repo / "prototypes" / "r1pro-tote-gripper",
            self.mujoco_repo / "pyproject.toml",
        ]
        required.extend(self.skill_packages / f"{name}-{SKILL_VERSIONS[name]}.zip" for name in SKILLS)
        missing = [str(path) for path in required if not path.exists()]
        if missing:
            raise GateError("缺少 MuJoCo 产品 Gate 输入：" + ", ".join(missing))

    def _start_product_server(self) -> None:
        self.current_step = "start_product_server"
        config = self.server_dir / "semantic-server.yaml"
        config.write_text(
            f"""server:
  http_addr: "127.0.0.1:{self.http_port}"
  ws_addr: "127.0.0.1:{self.ws_port}"
  read_timeout: 30s
  write_timeout: 30s
log:
  level: info
store:
  driver: sqlite
  sqlite_path: {self.server_dir / "semantic.db"}
llm:
  default: mock
  providers:
    mock:
      component: mock
      model: mock
      capabilities: [text, tool_call]
      price: {{prompt: 0, completion: 0}}
agents:
  profiles_dir: {self.framework_repo / "configs" / "agents"}
  teams_dir: {self.framework_repo / "configs" / "agents" / "teams"}
skills:
  dir: {self.framework_repo / "configs" / "skills"}
execution:
  allow_host: false
simulation:
  runtimes_dir: {self.runtimes_dir}
  catalog_dir: {self.framework_repo / "configs" / "scenes.d"}
robot_runtime:
  enabled: true
  bundles_dir: {self.bundle_dir.parent}
  data_root: {self.managed_data_root}
  server_http_url: {self.http_base}
  server_websocket_url: {self.ws_base}/ws/pilot
  ability_port_first: {self.ability_port_first}
  ability_port_last: {self.ability_port_last}
mcp_servers: []
""",
            encoding="utf-8",
        )
        env = os.environ.copy()
        env.update({
            "SEMANTIC_ADMIN_PASSWORD": "v050-mujoco-product-admin",
            "SEMANTIC_MOCK_SCRIPT": json.dumps(self._product_mock_script(), ensure_ascii=False),
            "SEMANTIC_MUJOCO_WORKDIR": str(self.mujoco_repo),
            "SEMANTIC_MUJOCO_ASSET_ROOT": str(self.asset_repo),
            "SEMANTIC_MUJOCO_GL": os.environ.get("SEMANTIC_MUJOCO_GL", "egl"),
        })
        process, stream = self._start_process(
            "semantic-server",
            (str(self.server_binary), "-c", str(config)),
            self.framework_repo,
            self.output_dir / "server.log",
            env,
        )
        self.processes.append(("semantic-server", process, stream))
        self._wait_http(f"{self.http_base}/api/v1/system/healthz", process, 30)
        response = self._request_json(
            "POST",
            "/api/v1/auth/login",
            {"username": "admin", "password": "v050-mujoco-product-admin"},
            authenticated=False,
        )
        self.token = str(response["token"])

    def _publish_product_skills(self) -> None:
        self.current_step = "publish_product_skills"
        for name in SKILLS:
            archive = self.skill_packages / f"{name}-{SKILL_VERSIONS[name]}.zip"
            self._request_bytes(
                "POST",
                "/api/v1/robot-skills",
                archive.read_bytes(),
                content_type="application/zip",
                expected=(201,),
            )

    def _create_product_project(self) -> None:
        self.current_step = "create_product_project"
        project = self._request_json(
            "POST",
            "/api/v1/projects",
            {"name": "v0.5 原生 MuJoCo 单箱产品 Gate"},
            expected=(201,),
        )["project"]
        self.project_id = str(project["id"])
        conversation = self._request_json(
            "POST",
            f"/api/v1/projects/{self.project_id}/conversations",
            {"title": "原生 MuJoCo 拆码垛"},
            expected=(201,),
        )["conversation"]
        self.conversation_id = str(conversation["id"])
        bindings = self._request_json(
            "PUT",
            f"/api/v1/projects/{self.project_id}/bindings",
            {
                "agent_ids": [],
                "skill_names": [
                    "depalletizing-workflow-planning",
                    "depalletizing-robot-task",
                ],
            },
        )["bindings"]
        if set(bindings.get("skill_names") or []) != {
            "depalletizing-workflow-planning",
            "depalletizing-robot-task",
        }:
            raise GateError(f"Project 拆码垛 Agent Skill 未正确绑定：{bindings}")

    def _start_smoke_scene(self) -> None:
        self.current_step = "start_layout_smoke"
        added = self._request_json(
            "POST",
            f"/api/v1/projects/{self.project_id}/simulation/project-scenes",
            {
                "catalog_scene_id": "depalletizing-r1pro",
                "scene_version": "1.0.0",
                "default_variant_id": "layout_smoke",
            },
            expected=(201,),
        )["project_scene"]
        project_scene_id = str(added["project_scene_id"])
        response = self._request_json(
            "POST",
            (
                f"/api/v1/projects/{self.project_id}/simulation/project-scenes/"
                f"{urllib.parse.quote(project_scene_id)}/instances"
            ),
            {
                "request_id": "v050-mujoco-product-layout-smoke",
                "variant_id": "layout_smoke",
                "runtime_installation_id": "local-native-mujoco",
                "seed": 7,
                "headless": True,
                "render_backend": "egl",
            },
            expected=(201,),
        )
        instance = response["instance"]
        self.scene_instance_id = str(instance["instance_id"])
        self.summary["scene"]["started"] = instance

    def _wait_robot_ready(self) -> None:
        self.current_step = "wait_managed_robot_ready"
        deadline = time.monotonic() + 150
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            self._ensure_processes_alive()
            snapshot = self._request_json("GET", "/api/v1/devices/snapshot")["snapshot"]
            devices = {item.get("robot_id"): item for item in snapshot.get("robots", [])}
            device = devices.get(ROBOT_ID) or {}
            last = device
            installed = {
                (item.get("name"), item.get("version")): item
                for item in device.get("installed_skills") or []
            }
            skills_ready = all(
                installed.get((name, SKILL_VERSIONS[name]), {}).get("status") == "installed"
                and installed.get((name, SKILL_VERSIONS[name]), {}).get("enabled") is True
                for name in SKILLS
            )
            if (
                device.get("pilot", {}).get("status") == "online"
                and device.get("ability_framework", {}).get("status") == "ready"
                and len(device.get("abilities") or []) == 7
                and skills_ready
            ):
                self.summary["device"]["ready"] = device
                return
            time.sleep(0.5)
        raise GateError(f"受管 MuJoCo Robot 未 ready：{last}")

    def _run_single_box_workflow(self) -> None:
        self.current_step = "single_box_workflow"
        self._send_plan_message(
            f"{PLAN_MARKER} 将 tote-large-smoke 搬到 pallet-b-slot-r1-c1，"
            "目标完整明确，请直接生成 Proposal。"
        )
        proposal = self._wait_ready_proposal()
        pending = self._request_json(
            "GET",
            f"/api/v1/interactions?status=pending&project_id={urllib.parse.quote(self.project_id)}",
        ).get("interactions") or []
        if pending:
            raise GateError(f"明确目标不应产生 Interaction：{pending}")
        approved = self._request_json(
            "POST",
            (
                f"/api/v1/projects/{self.project_id}/plan-proposals/"
                f"{proposal['id']}/approve"
            ),
            {"revision": int(proposal["revision"])},
        )
        workflow = (approved.get("workflow_view") or {}).get("workflow") or {}
        self.workflow_id = str(workflow.get("id") or "")
        if not self.workflow_id:
            raise GateError(f"Proposal 批准未原子创建 Workflow：{approved}")
        view = self._wait_product_workflow_terminal(timeout=240)
        if view["workflow"].get("status") != "completed":
            raise GateError(f"单箱 Workflow 未完成：{view}")
        self.summary["workflow"] = view
        self._write_json(self.output_dir / "workflow.json", {"workflow_view": view})

        tasks = view.get("tasks") or []
        subtasks = sorted(view.get("subtasks") or [], key=lambda item: item.get("position", 0))
        if len(tasks) != 1:
            raise GateError(f"单箱计划必须只有一个 Robot Task：{tasks}")
        expected = [
            ("semantic-navigation", "source-navigation"),
            ("grasp-object", "grasp"),
            ("semantic-navigation", "carry-navigation"),
            ("place-object", "place"),
        ]
        actual = [
            ((item.get("spec") or {}).get("skill_name"), item.get("id"))
            for item in subtasks
        ]
        if actual != expected or any(item.get("status") != "completed" for item in subtasks):
            raise GateError(f"Robot Agent 未生成并完成四个串行 SubTask：{actual}")
        for item in subtasks:
            spec = item.get("spec") or {}
            expected_version = SKILL_VERSIONS[str(spec.get("skill_name") or "")]
            if spec.get("skill_version") != expected_version:
                raise GateError(f"SubTask 未固定实际 Skill 版本 {expected_version}：{item}")
            execution_id = str(item.get("execution_ref") or "")
            if not execution_id:
                raise GateError(f"SubTask 缺少真实 Robot Execution：{item}")
            self.robot_execution_ids.append(execution_id)

    def _verify_product_result(self) -> None:
        self.current_step = "verify_product_result"
        expected_stages = {
            "semantic-navigation": {"validate_target", "plan_route", "navigate", "verify_arrival"},
            "grasp-object": {
                "observe_target", "approach", "grasp", "lift_and_verify", "prepare_transport",
            },
            "place-object": {
                "verify_held_object", "observe_target_slot", "plan_approach", "approach",
                "release", "retreat", "verify_stability", "restore_travel_posture",
            },
        }
        for execution_id in self.robot_execution_ids:
            detail = self._execution_detail(execution_id)
            execution = detail["execution"]
            if execution.get("status") != "completed":
                raise GateError(f"Robot Execution 未完成：{execution}")
            completed = {
                event.get("payload", {}).get("stage")
                for event in detail.get("events") or []
                if event.get("type") in {"stage.completed", "stage.recovered"}
                and event.get("payload", {}).get("stage_status") == "completed"
            }
            required = expected_stages[str(execution["skill_name"])]
            if not required.issubset(completed):
                raise GateError(
                    f"{execution['skill_name']} 缺少 Stage：{sorted(required - completed)}"
                )
            if '"simulated": true' in json.dumps(detail, ensure_ascii=False).lower():
                raise GateError(f"{execution_id} 混入 Fake Provider 结果")
            self.summary["executions"].append({
                "id": execution_id,
                "skill_name": execution["skill_name"],
                "skill_version": execution["skill_version"],
                "status": execution["status"],
                "stages": sorted(completed),
            })
            self._write_json(self.output_dir / "executions" / f"{execution_id}.json", detail)

        snapshot = self._request_json(
            "GET",
            (
                f"/api/v1/projects/{self.project_id}/simulation/instances/"
                f"{self.scene_instance_id}/snapshot"
            ),
        )["snapshot"]
        objects = {item.get("source_id"): item for item in snapshot.get("objects") or []}
        tote = objects.get("tote-large-smoke") or {}
        position = ((tote.get("pose") or {}).get("position") or [])
        if (
            len(position) != 3
            or abs(float(position[0]) - 1.19) > 0.08
            or abs(float(position[1]) - 1.29) > 0.08
            or abs(float(position[2]) - 0.32) > 0.08
        ):
            raise GateError(f"箱体未真实进入目标堆叠列：{position}")
        robots = {item.get("robot_id"): item for item in snapshot.get("robots") or []}
        robot = robots.get(ROBOT_ID) or {}
        tool_states = robot.get("gripper_states") or {}
        if any(
            state.get("hook_contact") or state.get("clamp_contact")
            for state in tool_states.values()
        ):
            raise GateError(f"放置后双工具仍接触箱体：{tool_states}")
        self.summary["physical_result"] = {
            "object_ref": "tote-large-smoke",
            "position_m": position,
            "tool_states": tool_states,
            "robot_in_hold": robot.get("in_hold"),
        }
        self._write_json(self.output_dir / "scene-snapshot.json", snapshot)
        self._wait_workflow_summary()

    def _stop_scene_and_verify_terminal_state(self) -> None:
        self.current_step = "stop_scene"
        response = self._request_json(
            "POST",
            (
                f"/api/v1/projects/{self.project_id}/simulation/instances/"
                f"{self.scene_instance_id}/stop"
            ),
            {},
        )
        self.summary["scene"]["stopped"] = response.get("instance")
        deadline = time.monotonic() + 60
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            snapshot = self._request_json("GET", "/api/v1/devices/snapshot")["snapshot"]
            devices = {item.get("robot_id"): item for item in snapshot.get("robots") or []}
            last = devices.get(ROBOT_ID) or {}
            if not last or last.get("pilot", {}).get("status") == "offline":
                break
            time.sleep(0.5)
        else:
            raise GateError(f"Scene stop 后受管 Pilot 未离线：{last}")
        for execution_id in self.robot_execution_ids:
            status = self._execution_detail(execution_id)["execution"].get("status")
            if status != "completed":
                raise GateError(
                    f"Scene stop 不得把已完成 Execution 回退为 {status}: {execution_id}"
                )
        self.summary["shutdown"] = {
            "pilot_offline": True,
            "completed_execution_count": len(self.robot_execution_ids),
        }

    def _wait_ready_proposal(self) -> dict[str, Any]:
        deadline = time.monotonic() + 30
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            try:
                body = self._request_json(
                    "GET",
                    f"/api/v1/projects/{self.project_id}/plan-proposals/active",
                )
                last = body.get("plan_proposal") or {}
                if last.get("status") == "ready":
                    return last
            except GateError as exc:
                if "返回 404" not in str(exc):
                    raise
            time.sleep(0.2)
        raise GateError(f"Plan Mode 未生成 ready Proposal：{last}")

    def _wait_product_workflow_terminal(self, *, timeout: float) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            last = self._workflow_view(self.workflow_id)
            self._sample_physical_state(last)
            if last["workflow"].get("status") in TERMINAL_WORKFLOWS:
                return last
            paused = [
                task
                for task in last.get("tasks") or []
                if task.get("status") == "paused"
            ]
            if paused:
                compact = [
                    {
                        "task_id": task.get("id"),
                        "reason": task.get("reason"),
                        "robot_execution_id": (task.get("evidence") or {}).get(
                            "robot_execution_id"
                        ),
                    }
                    for task in paused
                ]
                raise GateError(f"Workflow 已暂停，不能自行进入终态：{compact}")
            time.sleep(0.35)
        raise GateError(f"Workflow 未进入终态：{last}")

    def _sample_physical_state(self, workflow_view: dict[str, Any]) -> None:
        """保留测试侧的紧凑物理轨迹，用于把首次物体位移对应到SubTask。

        采样不进入Runtime、SDK、Ability或Skill契约，也不参与成功判定；它只在
        Gate失败时落盘，避免根据最终倒下的箱体反推是哪一段动作发生碰撞。
        """

        if not self.project_id or not self.scene_instance_id:
            return
        now = time.monotonic()
        if now - self._last_physical_sample_at < 3.0:
            return
        # 完整SceneSnapshot会短暂占用Runtime物理锁。Gate只需要低频失败证据，
        # 不能用高频观测把真实物理循环拖慢，再把测试自身的干扰误判为动作超时。
        self._last_physical_sample_at = now
        try:
            snapshot = self._request_json(
                "GET",
                (
                    f"/api/v1/projects/{self.project_id}/simulation/instances/"
                    f"{self.scene_instance_id}/snapshot"
                ),
            )["snapshot"]
        except GateError:
            return
        subtasks = workflow_view.get("subtasks") or []
        active = next(
            (
                item
                for item in subtasks
                if item.get("status") in {"running", "paused", "stopping"}
            ),
            None,
        )
        self.physical_samples.append({
            "observed_at": snapshot.get("observed_at"),
            "active_subtask": {
                "id": active.get("id"),
                "status": active.get("status"),
                "execution_ref": active.get("execution_ref"),
            } if active else None,
            "objects": [
                {
                    "source_id": item.get("source_id"),
                    "pose": item.get("pose"),
                }
                for item in snapshot.get("objects") or []
                if str(item.get("category") or "") == "tote"
            ],
            "robots": [
                {
                    "robot_id": item.get("robot_id"),
                    "joints": item.get("joints"),
                    "end_effectors": item.get("end_effectors"),
                    "gripper_states": item.get("gripper_states"),
                }
                for item in snapshot.get("robots") or []
            ],
        })

    def _wait_workflow_summary(self, *, timeout: float = 30) -> None:
        deadline = time.monotonic() + timeout
        last: list[dict[str, Any]] = []
        while time.monotonic() < deadline:
            body = self._request_json(
                "GET",
                f"/api/v1/chat/sessions/{self.conversation_id}/messages?page=1&page_size=100",
            )
            last = body.get("messages") or []
            for message in last:
                metadata = message.get("metadata") or {}
                if (
                    metadata.get("message_kind") == "workflow_summary"
                    and metadata.get("workflow_id") == self.workflow_id
                ):
                    self.summary["workflow_summary"] = message.get("content")
                    self._write_json(self.output_dir / "conversation.json", body)
                    return
            time.sleep(0.2)
        raise GateError(f"Workflow 终态未触发 Leader 总结：{last[-5:]}")

    def _product_mock_script(self) -> dict[str, Any]:
        plan = {
            "goal": "完成单箱原生 MuJoCo 拆码垛",
            "summary": "由真实 R1 Pro MuJoCo Robot 搬运一个周转箱",
            "approved_scope": {
                "robot_ids": [ROBOT_ID],
                "robot_models": ["r1_pro_chassis"],
                "allowed_skills": list(SKILLS),
            },
            "constraints": {
                "physical_execution": True,
                "no_fake_provider": True,
                "no_pose_teleport": True,
            },
            "completion_criteria": {
                "object_ref": "tote-large-smoke",
                "target_ref": "pallet-b-slot-r1-c1",
                "placement_stable": True,
                "gripper_empty": True,
                "travel_posture": True,
            },
            "tasks": [{
                "id": "task-mujoco-single-box",
                "required_role": "robot",
                "required_capabilities": list(SKILLS),
                "resource_requirements": {
                    "robot_ids": [ROBOT_ID],
                    "robot_models": ["r1_pro_chassis"],
                    "backends": ["mujoco"],
                },
                "goal": f"{TASK_MARKER} 将 tote-large-smoke 搬到 pallet-b-slot-r1-c1",
                "input": {
                    "object_ref": "tote-large-smoke",
                    "target_ref": "pallet-b-slot-r1-c1",
                    "tool_refs": ["component://tool/left", "component://tool/right"],
                    "source_work_pose": self._work_pose(0.0, 0.5),
                    "target_work_pose": self._work_pose(1.19, 0.5),
                },
                "completion_criteria": {
                    "placement_stable": True,
                    "gripper_empty": True,
                    "travel_posture": True,
                },
            }],
            "dependencies": [],
        }
        steps = self._skill_steps()
        subtasks = self._subtasks(steps)
        routes: list[dict[str, Any]] = [{
            "match": PLAN_MARKER,
            "replies": [
                {"tool_calls": [{
                    "id": "plan-mujoco-single-box",
                    "name": "plan_suggest",
                    "arguments": json.dumps(plan, ensure_ascii=False),
                }]},
                {"content": "单箱计划已生成，等待批准。"},
            ],
        }, {
            "match_all": ["请为这个 Worker Task", TASK_MARKER],
            "replies": [{"content": json.dumps({
                "summary": "Robot Agent已规划来源导航、抓取、一次携物导航和放置四个步骤。",
                "subtasks": subtasks,
            }, ensure_ascii=False)}],
        }]
        for step in steps:
            subtask_id = step["id"]
            routes.append({
                "match_all": ["只推进 current_subtask", f"\"id\":\"{subtask_id}\""],
                "replies": [
                    {"tool_calls": [{
                        "id": f"get-{subtask_id}",
                        "name": "robot_get",
                        "arguments": "{}",
                    }]},
                    {"tool_calls": [{
                        "id": f"run-{subtask_id}",
                        "name": "robot_run",
                        "arguments": json.dumps({
                            "skill_name": step["skill_name"],
                            "skill_version": SKILL_VERSIONS[step["skill_name"]],
                            "input": step["input"],
                        }, ensure_ascii=False),
                    }]},
                    {"content": json.dumps({
                        "kind": "result",
                        "summary": f"已建立 {step['skill_name']} Robot Execution",
                        "evidence": [],
                    }, ensure_ascii=False)},
                ],
            })
        routes.append({
            "match": "下面是已经进入终态的 Workflow 结构化事实",
            "replies": [{"content": (
                "单箱拆码垛已完成：tote-large-smoke 已稳定放入 "
                "pallet-b-slot-r1-c1，双工具为空，Robot 已恢复 travel 姿态。"
            )}],
        })
        for revision in range(0, 22):
            routes.append({
                "match_all": [
                    "Robot Skill 已在安全 checkpoint 请求",
                    "GraspAgentDecision",
                    f"\"decision_revision\":{revision},",
                    "retry_lift",
                ],
                "replies": [{
                    "content": json.dumps({
                        "expected_plan_revision": revision,
                        "action": "retry_lift",
                        "reason": "抬升尚未下发，重新尝试附着后的抬升。",
                        "evidence_refs": [],
                    }, ensure_ascii=False),
                }],
            })
        for revision in range(0, 22):
            routes.append({
                "match_all": [
                    "Robot Skill 已在安全 checkpoint 请求",
                    "GraspAgentDecision",
                    f"\"decision_revision\":{revision},",
                ],
                "replies": [{
                    "content": json.dumps({
                        "expected_plan_revision": revision,
                        "action": "abort_subtask",
                        "reason": "当前抓取现场无法安全自动恢复，保留物理现场并结束本次验收。",
                        "evidence_refs": [],
                    }, ensure_ascii=False),
                }],
            })
        routes.append({"match": "", "replies": [{"content": "{}"}]})
        empty_index = next(index for index, route in enumerate(routes) if route.get("match") == "")
        grasp_indexes = [
            index for index, route in enumerate(routes)
            if "GraspAgentDecision" in str(route)
        ]
        if not grasp_indexes or max(grasp_indexes) >= empty_index:
            raise AssertionError("GraspAgentDecision 路由必须排在空匹配 {} 之前")
        return {"scripts": routes}

    def _skill_steps(self) -> list[dict[str, Any]]:
        source_navigation = {
            "target": {"target_ref": "source-work-pose", "pose": self._work_pose(0.0, 0.5)},
            "navigation_purpose": "approach_grasp",
            "maximum_speed_mps": 0.15,
            "arrival_radius_m": 0.03,
            "minimum_clearance_m": 0.05,
        }
        grasp = {
            "target": {
                "object_ref": "tote-large-smoke",
                "pose_hint": None,
                "extent_hint_m": [0.6, 0.4, 0.34],
                "category_hint": "tote",
            },
            "tool_refs": ["component://tool/left", "component://tool/right"],
            "preferred_strategy": "auto",
            "minimum_lift_height_m": 0.08,
        }
        carry_navigation = {
            "target": {
                "target_ref": "pallet-b-slot-r1-c1",
                "pose": self._work_pose(1.19, 0.5),
            },
            "navigation_purpose": "carry_to_place",
            "carried_object_ref": "tote-large-smoke",
            "maximum_speed_mps": 0.05,
            "arrival_radius_m": 0.03,
            "minimum_clearance_m": 0.05,
        }
        place = {
            "object_ref": "tote-large-smoke",
            "target": {
                "target_ref": "pallet-b-slot-r1-c1",
                "pose_hint": None,
                "extent_hint_m": [0.56, 0.36, 0.02],
                "category_hint": "placement_slot",
                "stability_duration_ms": 1000,
            },
        }
        return [
            {"id": "source-navigation", "goal": "导航到来源物体的可操作位置",
             "skill_name": "semantic-navigation",
             "intent": {"purpose": "source_approach", "object_ref": "tote-large-smoke"},
             "input": source_navigation},
            {"id": "grasp", "goal": "抓取周转箱并整理为携物转运姿态",
             "skill_name": "grasp-object",
             "intent": {"object_ref": "tote-large-smoke", "preferred_strategy": "auto"},
             "input": grasp},
            {"id": "carry-navigation", "goal": "一次携物导航到目标堆叠列的放置工位",
             "skill_name": "semantic-navigation",
             "intent": {"purpose": "carry_to_place", "object_ref": "tote-large-smoke",
                        "target_ref": "pallet-b-slot-r1-c1"},
             "input": carry_navigation},
            {"id": "place", "goal": "放置周转箱并恢复行走姿态",
             "skill_name": "place-object",
             "intent": {"object_ref": "tote-large-smoke",
                        "target_ref": "pallet-b-slot-r1-c1"},
             "input": place},
        ]

    def _subtasks(self, steps: list[dict[str, Any]]) -> list[dict[str, Any]]:
        result = []
        for index, step in enumerate(steps):
            result.append({
                "id": step["id"],
                "kind": "robot_skill",
                "goal": step["goal"],
                "spec": {
                    "skill_name": step["skill_name"],
                    "skill_version": SKILL_VERSIONS[step["skill_name"]],
                    "intent": step["intent"],
                },
                "completion_criteria": {"robot_execution_status": "completed"},
                "depends_on": [] if index == 0 else [steps[index - 1]["id"]],
            })
        return result

    @staticmethod
    def _work_pose(x: float, y: float) -> dict[str, Any]:
        return {
            "frame_id": "world",
            "position_m": [x, y, 0.02],
            "orientation_xyzw": [0.0, 0.0, 0.7071068, 0.7071068],
        }

    @staticmethod
    def _free_port_range(count: int) -> tuple[int, int]:
        for first in range(20000, 30000, count):
            sockets: list[socket.socket] = []
            try:
                for port in range(first, first + count):
                    sock = socket.socket()
                    sock.bind(("127.0.0.1", port))
                    sockets.append(sock)
                return first, first + count - 1
            except OSError:
                continue
            finally:
                for sock in sockets:
                    sock.close()
        raise GateError("没有可用的 Ability Runtime 端口段")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    gate = V050MujocoProductGate(args.output_dir)
    try:
        gate.run()
    except BaseException:
        logging.exception("v0.5 原生 MuJoCo 产品 Gate 失败")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
