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

"""v0.5 原生 MuJoCo 一层/整垛确定性产品链 Gate。

这条 Gate 复用单箱已经验证的 Server、受管 Robot、Workflow 和 Robot
Execution 通路，只扩大环境中的搬箱任务数。每个箱体仍由一个 Robot Task
负责，Task 内固定为来源导航、抓取、一次携物导航和放置四个 SubTask。
"""

from __future__ import annotations

import argparse
import json
import logging
import math
import time
import urllib.parse
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from v050_mujoco_product import (
    ROBOT_ID,
    SKILLS,
    SKILL_VERSIONS,
    V050MujocoProductGate,
)
from v050_real_gate import GateError


# 确定性物理 Gate 使用“箱体中心到基座中心”的距离。Profile 中的
# manipulation_work_distance_m 则从箱体表面起算；当前周转箱半深 0.20m，
# 因此 0.48m 产品工作距离对应这里的 0.68m。
WORK_STANDOFF_M = 0.68
# 确定性 Gate 的 mock 不会在 grasp-object 后计算实时箱体—底盘变换，
# 因此只冻结本布局密集同层箱所需的最小侧向余量；前后方向仍服从上面的
# 底盘合法站距。产品Robot Agent必须从实时携物关系求名义工位，再投影到
# 可通行、可放置的工位，不能按槽位中心硬对齐底盘。
TRANSPORT_LATERAL_CLEARANCE_M = 0.063
PALLET_CENTER_Y_M = 1.50


@dataclass(frozen=True)
class BoxMove:
    object_ref: str
    target_ref: str
    source_xy: tuple[float, float]
    target_xy: tuple[float, float]
    target_layer: int

    @property
    def marker(self) -> str:
        return f"MUJOCO-MOVE-{self.object_ref}"


def _moves(case: str) -> list[BoxMove]:
    columns = [
        ("r1-c1", (-0.31, 1.29), (1.19, 1.29)),
        ("r1-c2", (0.31, 1.29), (1.81, 1.29)),
        ("r2-c1", (-0.31, 1.71), (1.19, 1.71)),
        ("r2-c2", (0.31, 1.71), (1.81, 1.71)),
    ]
    layers = (3,) if case in {"top", "layer"} else (3, 2, 1)
    result: list[BoxMove] = []
    for target_layer, source_layer in enumerate(layers, start=1):
        for suffix, source_xy, target_xy in columns:
            result.append(BoxMove(
                object_ref=f"tote-large-l{source_layer}-{suffix}",
                target_ref=f"pallet-b-slot-{suffix}",
                source_xy=source_xy,
                target_xy=target_xy,
                target_layer=target_layer,
            ))
    return result[:1] if case == "top" else result


class V050MujocoMultiGate(V050MujocoProductGate):
    """在正式 layout001 上验收一层或完整三层搬运。"""

    def __init__(
        self,
        output_dir: Path,
        case: str,
        grasp_strategy: str = "auto",
        source_standoff_m: float = WORK_STANDOFF_M,
        source_lateral_offset_m: float = 0.0,
    ) -> None:
        if case not in {"top", "layer", "pallet"}:
            raise GateError(f"不支持的多箱 Gate case: {case}")
        if grasp_strategy not in {
            "auto", "direct_bilateral", "left_extract_first", "right_extract_first"
        }:
            raise GateError(f"不支持的抓取策略: {grasp_strategy}")
        self.case = case
        self.grasp_strategy = grasp_strategy
        self.source_standoff_m = source_standoff_m
        self.source_lateral_offset_m = source_lateral_offset_m
        self.moves = _moves(case)
        if case == "top" and grasp_strategy == "right_extract_first":
            # 镜像单箱Gate选择右侧外缘箱，避免让“右侧先抓”朝相邻箱内部外拉。
            self.moves = _moves("layer")[1:2]
        super().__init__(output_dir)
        self.summary["multi_box"] = {
            "case": case,
            "grasp_strategy": grasp_strategy,
            "box_count": len(self.moves),
            "expected_robot_executions": len(self.moves) * 4,
            "source_lateral_offset_m": source_lateral_offset_m,
        }

    def _create_product_project(self) -> None:
        self.current_step = "create_multi_box_project"
        title = {
            "top": "高层单箱",
            "layer": "一层四箱",
            "pallet": "完整三层托盘",
        }[self.case]
        project = self._request_json(
            "POST",
            "/api/v1/projects",
            {"name": f"v0.5 原生 MuJoCo {title}产品 Gate"},
            expected=(201,),
        )["project"]
        self.project_id = str(project["id"])
        conversation = self._request_json(
            "POST",
            f"/api/v1/projects/{self.project_id}/conversations",
            {"title": f"原生 MuJoCo {title}拆码垛"},
            expected=(201,),
        )["conversation"]
        self.conversation_id = str(conversation["id"])
        self._request_json(
            "PUT",
            f"/api/v1/projects/{self.project_id}/bindings",
            {
                "agent_ids": [],
                "skill_names": [
                    "depalletizing-workflow-planning",
                    "depalletizing-robot-task",
                ],
            },
        )

    def _start_smoke_scene(self) -> None:
        self.current_step = "start_layout001"
        added = self._request_json(
            "POST",
            f"/api/v1/projects/{self.project_id}/simulation/project-scenes",
            {
                "catalog_scene_id": "depalletizing-r1pro",
                "scene_version": "1.0.0",
                "default_variant_id": "layout001",
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
                "request_id": f"v050-mujoco-product-{self.case}-{self.output_dir.name}",
                "variant_id": "layout001",
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

    def _run_single_box_workflow(self) -> None:
        self.current_step = f"{self.case}_workflow"
        marker = f"PLAN-V050-MUJOCO-{self.case.upper()}"
        self._send_plan_message(
            f"{marker} 按当前 layout001 依次搬运 {len(self.moves)} 个周转箱，"
            "每箱只建立一个 Robot Task，目标完整明确，请直接生成 Proposal。"
        )
        proposal = self._wait_ready_proposal()
        pending = self._request_json(
            "GET",
            f"/api/v1/interactions?status=pending&project_id={urllib.parse.quote(self.project_id)}",
        ).get("interactions") or []
        if pending:
            raise GateError(f"明确的多箱目标不应产生 Interaction：{pending}")
        approved = self._request_json(
            "POST",
            f"/api/v1/projects/{self.project_id}/plan-proposals/{proposal['id']}/approve",
            {"revision": int(proposal["revision"])},
        )
        workflow = (approved.get("workflow_view") or {}).get("workflow") or {}
        self.workflow_id = str(workflow.get("id") or "")
        if not self.workflow_id:
            raise GateError(f"Proposal批准未创建Workflow：{approved}")
        timeout = {"top": 500, "layer": 1500, "pallet": 4500}[self.case]
        view = self._wait_product_workflow_terminal(timeout=timeout)
        if view["workflow"].get("status") != "completed":
            raise GateError(f"{self.case} Workflow未完成：{view}")
        self.summary["workflow"] = view
        self._write_json(self.output_dir / "workflow.json", {"workflow_view": view})
        self._validate_multi_tasks(view)

    def _validate_multi_tasks(self, view: dict[str, Any]) -> None:
        tasks = view.get("tasks") or []
        subtasks = view.get("subtasks") or []
        if len(tasks) != len(self.moves):
            raise GateError(f"Task数量错误，期望{len(self.moves)}，实际{len(tasks)}")
        by_task: dict[str, list[dict[str, Any]]] = {}
        for item in subtasks:
            by_task.setdefault(str(item.get("task_id")), []).append(item)
        expected_sequence = [
            "semantic-navigation", "grasp-object", "semantic-navigation", "place-object"
        ]
        for task in tasks:
            task_subtasks = sorted(
                by_task.get(str(task.get("id")), []),
                key=lambda item: int(item.get("position") or 0),
            )
            sequence = [(item.get("spec") or {}).get("skill_name") for item in task_subtasks]
            if sequence != expected_sequence:
                raise GateError(f"Task {task.get('id')} 的四步序列错误：{sequence}")
            for item in task_subtasks:
                spec = item.get("spec") or {}
                if item.get("status") != "completed":
                    raise GateError(f"SubTask未完成：{item}")
                expected_version = SKILL_VERSIONS[str(spec.get("skill_name") or "")]
                if spec.get("skill_version") != expected_version:
                    raise GateError(f"SubTask版本错误，期望{expected_version}：{item}")
                intent = spec.get("intent") or {}
                if "input" in spec or "carrying_object" in intent or "held_object" in intent:
                    raise GateError(f"SubTask保存执行输入或复制了完整持物状态：{item}")
                execution_id = str(item.get("execution_ref") or "")
                if not execution_id:
                    raise GateError(f"SubTask缺少Robot Execution：{item}")
                self.robot_execution_ids.append(execution_id)
        if len(self.robot_execution_ids) != len(self.moves) * 4:
            raise GateError(f"Robot Execution数量错误：{self.robot_execution_ids}")

    def _verify_product_result(self) -> None:
        self.current_step = f"verify_{self.case}_result"
        self._verify_execution_stages()
        snapshot = self._request_json(
            "GET",
            (
                f"/api/v1/projects/{self.project_id}/simulation/instances/"
                f"{self.scene_instance_id}/snapshot"
            ),
        )["snapshot"]
        objects = {item.get("source_id"): item for item in snapshot.get("objects") or []}
        regions = {item.get("source_id"): item for item in snapshot.get("regions") or []}
        positions: dict[str, list[float]] = {}
        for move in self.moves:
            tote = objects.get(move.object_ref) or {}
            region = regions.get(move.target_ref) or {}
            position = list(((tote.get("pose") or {}).get("position") or []))
            region_position = list(((region.get("pose") or {}).get("position") or []))
            region_extent = list(region.get("extent") or [])
            expected_z = 0.15 + 0.34 * (move.target_layer - 1) + 0.17
            if (
                len(position) != 3
                or len(region_position) != 3
                or len(region_extent) != 3
                # 产品完成条件是箱体中心属于目标 Region，并已稳定接触支撑面。
                # Gate 不再另造一个比正式 VerifyPlacement 更严的固定中心误差。
                or any(
                    abs(float(position[index]) - float(region_position[index]))
                    > float(region_extent[index]) / 2
                    for index in (0, 1)
                )
                or abs(float(position[2]) - expected_z) > 0.08
            ):
                raise GateError(
                    f"{move.object_ref}未进入{move.target_ref}第{move.target_layer}层："
                    f"actual={position}, expected_z={expected_z}"
                )
            positions[move.object_ref] = position
        robots = {item.get("robot_id"): item for item in snapshot.get("robots") or []}
        robot = robots.get(ROBOT_ID) or {}
        tool_states = robot.get("gripper_states") or {}
        if any(
            state.get("hook_contact") or state.get("clamp_contact")
            for state in tool_states.values()
        ):
            raise GateError(f"最终双工具仍接触箱体：{tool_states}")
        self.summary["physical_result"] = {
            "positions_m": positions,
            "tool_states": tool_states,
            "robot_in_hold": robot.get("in_hold"),
        }
        self._write_json(self.output_dir / "scene-snapshot.json", snapshot)
        self._wait_workflow_summary()

    def _verify_execution_stages(self) -> None:
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
                raise GateError(f"Robot Execution未完成：{execution}")
            completed = {
                event.get("payload", {}).get("stage")
                for event in detail.get("events") or []
                if event.get("type") in {"stage.completed", "stage.recovered"}
                and event.get("payload", {}).get("stage_status") == "completed"
            }
            required = expected_stages[str(execution["skill_name"])]
            if not required.issubset(completed):
                raise GateError(
                    f"{execution['skill_name']}缺少Stage：{sorted(required - completed)}"
                )
            if '"simulated": true' in json.dumps(detail, ensure_ascii=False).lower():
                raise GateError(f"{execution_id}混入Fake Provider")
            dense_clearance = None
            if execution["skill_name"] == "place-object":
                dense_clearance = self._verify_dense_place_clearance(
                    execution_id, detail.get("events") or []
                )
            self.summary["executions"].append({
                "id": execution_id,
                "skill_name": execution["skill_name"],
                "status": execution["status"],
                "stages": sorted(completed),
                "dense_release_clearance": dense_clearance,
            })
            self._write_json(self.output_dir / "executions" / f"{execution_id}.json", detail)

    @staticmethod
    def _verify_dense_place_clearance(
        execution_id: str,
        events: list[dict[str, Any]],
    ) -> dict[str, Any] | None:
        """验证密集列短推前，先释放的空钩已经离开箱体实体范围。

        产品流程先水平退出凹槽，再从临时偏置位抬过箱沿，最后才把箱体推回
        目标列。此时空钩既要退出侧面凹槽，钩尖也要越过箱沿；不能在狭窄的
        邻箱间隙里继续横向挤出整套夹具宽度。本检查只读取正式RobotState和
        物体观测，不使用命令目标冒充实际结果。
        """

        started = [
            event for event in events if event.get("type") == "action.started"
        ]
        early_release = next((
            event for event in started
            if ".early.component://tool/" in str(
                (event.get("payload") or {}).get("action_key") or ""
            )
        ), None)
        if early_release is None:
            return None
        release_key = str((early_release.get("payload") or {}).get("action_key") or "")
        early_tool = release_key.partition(".early.")[2]

        def observation_for_action_key(key_suffix: str, kind: str) -> dict[str, Any]:
            action = next((
                event for event in started
                if str((event.get("payload") or {}).get("action_key") or "").endswith(
                    key_suffix
                )
            ), None)
            if action is None:
                raise GateError(f"{execution_id}缺少{key_suffix}动作")
            action_id = (action.get("payload") or {}).get("action_id")
            observation = next((
                event for event in events
                if event.get("type") == "observation.recorded"
                and (event.get("payload") or {}).get("action_id") == action_id
                and ((event.get("payload") or {}).get("observation") or {}).get("kind")
                == kind
            ), None)
            if observation is None:
                raise GateError(f"{execution_id}的{key_suffix}缺少{kind}观测")
            return ((observation.get("payload") or {}).get("observation") or {}).get(
                "value"
            ) or {}

        robot_state = observation_for_action_key(
            ".supported-slide.robot-state", "robot.state"
        )
        object_state = observation_for_action_key(
            ".supported-slide.before-push", "target_pose"
        )
        tool_state = (robot_state.get("tool_states") or {}).get(early_tool) or {}
        side = str(tool_state.get("side") or "")
        end_effector = (robot_state.get("end_effectors") or {}).get(side) or {}
        tool_position = end_effector.get("position") or []
        object_position = ((object_state.get("pose") or {}).get("position_m") or [])
        object_extent = object_state.get("extent_m") or []
        if len(tool_position) != 3 or len(object_position) != 3 or len(object_extent) != 3:
            raise GateError(f"{execution_id}短推前的工具或物体几何观测不完整")
        outside_x_m = max(
            0.0,
            abs(float(tool_position[0]) - float(object_position[0]))
            - float(object_extent[0]) / 2,
        )
        outside_y_m = max(
            0.0,
            abs(float(tool_position[1]) - float(object_position[1]))
            - float(object_extent[1]) / 2,
        )
        horizontal_clearance_m = math.hypot(outside_x_m, outside_y_m)
        # load frame 位于钩尖上方约17mm。水平退钩是否完成以真实接触已消失
        # 为准，不再用毫米级水平净距重复判定；这里重点验证用户可见的问题：
        # 另一只手开始推箱前，空钩必须已经越过箱沿。
        vertical_clearance_m = (
            float(tool_position[2])
            - (float(object_position[2]) + float(object_extent[2]) / 2)
            - 0.017
        )
        if vertical_clearance_m <= 0.0:
            raise GateError(
                f"{execution_id}短推前{early_tool}仍未退出箱体范围："
                f"horizontal={horizontal_clearance_m:.4f}m, "
                f"vertical={vertical_clearance_m:.4f}m"
            )
        if tool_state.get("hook_contact") or tool_state.get("clamp_contact"):
            raise GateError(f"{execution_id}短推前{early_tool}仍接触箱体：{tool_state}")
        return {
            "tool_ref": early_tool,
            "tool_position_m": [float(value) for value in tool_position],
            "object_position_m": [float(value) for value in object_position],
            "horizontal_clearance_m": horizontal_clearance_m,
            "vertical_clearance_m": vertical_clearance_m,
        }

    def _product_mock_script(self) -> dict[str, Any]:
        marker = f"PLAN-V050-MUJOCO-{self.case.upper()}"
        tasks: list[dict[str, Any]] = []
        dependencies: list[dict[str, str]] = []
        routes: list[dict[str, Any]] = []
        previous_by_column: dict[str, str] = {}
        for index, move in enumerate(self.moves, start=1):
            task_id = f"task-{move.object_ref}"
            tasks.append({
                "id": task_id,
                "required_role": "robot",
                "required_capabilities": list(SKILLS),
                "resource_requirements": {
                    "robot_ids": [ROBOT_ID],
                    "robot_models": ["r1_pro_chassis"],
                    "backends": ["mujoco"],
                },
                "goal": f"{move.marker} 将 {move.object_ref} 搬到 {move.target_ref}",
                "input": self._task_input(move),
                "completion_criteria": {
                    "placement_stable": True,
                    "gripper_empty": True,
                    "travel_posture": True,
                },
            })
            column = move.target_ref.removeprefix("pallet-b-slot-")
            if previous_task_id := previous_by_column.get(column):
                dependencies.append({"task_id": task_id, "depends_on_task_id": previous_task_id})
            previous_by_column[column] = task_id
            steps = self._skill_steps(move, task_id)
            routes.append({
                "match_all": ["请为这个 Worker Task", move.marker],
                "replies": [{
                    "content": json.dumps(
                        {
                            "summary": (
                                f"Robot Agent已为{move.object_ref}规划来源导航、抓取、"
                                "一次携物导航和放置四个步骤。"
                            ),
                            "subtasks": self._subtasks(steps),
                        },
                        ensure_ascii=False,
                    )
                }],
            })
            for step in steps:
                subtask_id = step["id"]
                routes.append({
                    "match_all": ["只推进 current_subtask", f'"id":"{subtask_id}"'],
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
        plan = {
            "goal": {
                "top": "完成高层单箱拆码垛",
                "layer": "完成一层周转箱拆码垛",
                "pallet": "完成整垛周转箱拆码垛",
            }[self.case],
            "summary": f"由一台真实MuJoCo Robot依次搬运{len(self.moves)}个周转箱",
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
                "moved_box_count": len(self.moves),
                "placement_stable": True,
                "gripper_empty": True,
                "travel_posture": True,
            },
            "tasks": tasks,
            "dependencies": dependencies,
        }
        routes.insert(0, {
            "match": marker,
            "replies": [
                {"tool_calls": [{
                    "id": f"plan-mujoco-{self.case}",
                    "name": "plan_suggest",
                    "arguments": json.dumps(plan, ensure_ascii=False),
                }]},
                {"content": f"{self.case}计划已生成，等待批准。"},
            ],
        })
        # Recovery Run 会携带同一 Task 的历史消息，其中可能仍包含前一次
        # GraspAgentDecision。失败收敛必须优先匹配当前触发原因，不能让历史
        # 抓取请求抢先命中并返回一份已经失效的抓取决策。
        routes.insert(1, {
            "match": "Robot Skill 已明确失败且物理执行已经终止",
            "replies": [{
                "content": json.dumps({
                    "decision": "fail_task",
                    "summary": "物理执行已终止，本轮验收停止，不重放已开始的动作。",
                    "replacements": [],
                }, ensure_ascii=False),
            }],
        })
        routes.append({
            "match_all": [
                "Robot Skill 已在安全 checkpoint 请求",
                "PlacementAgentDecision",
            ],
            "replies": [{
                "content": json.dumps({
                    "decision": "abort",
                    "reason": "释放后撤离动作未完成，保留物理现场并结束本次验收。",
                }, ensure_ascii=False),
            }],
        })
        routes.append({
            "match_all": [
                "Robot Skill 已在安全 checkpoint 请求",
                "NavigationAgentDecision",
            ],
            "replies": [{
                "content": json.dumps({
                    "action": "abort_subtask",
                    "reason": "当前路线无法执行，保留物理现场并结束本次验收。",
                    "evidence_refs": [],
                }, ensure_ascii=False),
            }],
        })
        # 第一次单侧外拉后，实时规划器可能只返回反侧外拉候选。确定性物理
        # Gate模拟 Robot Agent选择该既有策略继续释放操作空间；它不生成轨迹，
        # 也不在第二次仍无双侧候选时继续左右往返。真实产品链由模型基于同一
        # available_candidates上下文做相同类型化决策。
        for revision in range(0, 22):
            for strategy in ("left_extract_first", "right_extract_first"):
                routes.append({
                    "match_all": [
                        "Robot Skill 已在安全 checkpoint 请求",
                        "GraspAgentDecision",
                        f"\"decision_revision\":{revision},",
                        "\"grasp_attempts\":1",
                        f"\"strategy\":\"{strategy}\"",
                    ],
                    "replies": [{
                        "content": json.dumps({
                            "expected_plan_revision": revision,
                            "action": "change_strategy",
                            "strategy": strategy,
                            "reason": "采用外拉后实时生成的反侧候选继续释放操作空间。",
                            "evidence_refs": [],
                        }, ensure_ascii=False),
                    }],
                })

        # Skill 的 plan_revision 会随候选切换递增。Gate 只负责在物理
        # 失败时安全中止，必须回显请求里的精确 revision，不能用静态旧值把
        # 测试夹具错误伪装成产品的 STALE_AGENT_DECISION。
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
                        "reason": "当前工位没有可达抓取候选，保留物理现场并结束本次验收。",
                        "evidence_refs": [],
                    }, ensure_ascii=False),
                }],
            })
        routes.append({
            "match": "下面是已经进入终态的 Workflow 结构化事实",
            "replies": [{"content": (
                f"{self.case}拆码垛已完成：{len(self.moves)}个周转箱均已稳定放置，"
                "双工具为空，Robot已恢复travel姿态。"
            )}],
        })
        routes.append({"match": "", "replies": [{"content": "{}"}]})
        return {"scripts": routes}

    def _task_input(self, move: BoxMove) -> dict[str, Any]:
        return {
            "object_ref": move.object_ref,
            "target_ref": move.target_ref,
            "tool_refs": ["component://tool/left", "component://tool/right"],
            "source_work_pose": self._source_work_pose(move),
            "target_work_pose": self._target_work_pose(move),
        }

    def _skill_steps(self, move: BoxMove, task_id: str) -> list[dict[str, Any]]:
        grasp_strategy = self.grasp_strategy
        source_navigation = {
            "target": {
                "target_ref": f"{move.object_ref}-work-pose",
                "pose": self._source_work_pose(move),
            },
            "navigation_purpose": "approach_grasp",
            "maximum_speed_mps": 0.15,
            "arrival_radius_m": 0.03,
            "minimum_clearance_m": 0.05,
        }
        grasp = {
            "target": {
                "object_ref": move.object_ref,
                "pose_hint": None,
                "extent_hint_m": [0.6, 0.4, 0.34],
                "category_hint": "tote",
            },
            "tool_refs": ["component://tool/left", "component://tool/right"],
            "preferred_strategy": grasp_strategy,
            "minimum_lift_height_m": 0.08,
        }
        carry_navigation = {
            "target": {
                "target_ref": move.target_ref,
                "pose": self._target_work_pose(move),
            },
            "navigation_purpose": "carry_to_place",
            "carried_object_ref": move.object_ref,
            "maximum_speed_mps": 0.05,
            "arrival_radius_m": 0.03,
            "minimum_clearance_m": 0.05,
        }
        place = {
            "object_ref": move.object_ref,
            "target": {
                "target_ref": move.target_ref,
                "pose_hint": None,
                "extent_hint_m": [0.56, 0.36, 0.02],
                "category_hint": "placement_slot",
                "stability_duration_ms": 1000,
            },
        }
        values = [
            ("source-navigation", "导航到来源物体可操作位置", "semantic-navigation",
             {"purpose": "source_approach", "object_ref": move.object_ref}, source_navigation),
            ("grasp", "抓取并整理为携物姿态", "grasp-object",
             {"object_ref": move.object_ref,
              "preferred_strategy": grasp_strategy}, grasp),
            ("carry-navigation", "一次携物导航到目标列", "semantic-navigation",
             {"purpose": "carry_to_place", "object_ref": move.object_ref,
              "target_ref": move.target_ref}, carry_navigation),
            ("place", "放置并恢复travel姿态", "place-object",
             {"object_ref": move.object_ref, "target_ref": move.target_ref}, place),
        ]
        return [{
            "id": f"{task_id}-{suffix}",
            "goal": goal,
            "skill_name": skill_name,
            "intent": intent,
            "input": skill_input,
        } for suffix, goal, skill_name, intent, skill_input in values]

    def _source_work_pose(self, move: BoxMove) -> dict[str, Any]:
        """为确定性Gate冻结一个以实时箱体中心为基准的已知工位。

        单侧外拉开始后，承载臂不能再借共享躯干补偿，否则真实下钩会被
        带出侧面凹槽。Gate可沿抓取侧预留有限横向余量，用于验证“抓取前选择
        合适工位”而不是“接触后扭腰强拉”。这个偏置只用于物理诊断；产品
        Robot Agent仍应根据实时箱体、邻物、所选策略和Robot工作空间计算。
        """

        lateral_sign = {
            "left_extract_first": -1.0,
            "right_extract_first": 1.0,
        }.get(self.grasp_strategy, 0.0)
        return self._pallet_work_pose(
            move.source_xy[0] + lateral_sign * self.source_lateral_offset_m,
            move.source_xy[1],
            self.source_standoff_m,
        )

    def _target_work_pose(self, move: BoxMove) -> dict[str, Any]:
        """冻结满足底盘站距并保留携物侧向余量的目标工位。"""

        inward_sign = 1.0 if move.target_xy[0] < 1.5 else -1.0
        return self._pallet_work_pose(
            move.target_xy[0] + inward_sign * TRANSPORT_LATERAL_CLEARANCE_M,
            move.target_xy[1],
            WORK_STANDOFF_M,
        )

    def _pallet_work_pose(
        self,
        x: float,
        row_y: float,
        standoff_m: float,
    ) -> dict[str, Any]:
        """从当前箱体所在的托盘外侧生成基座工位。

        后排箱体若仍从托盘前侧套用固定站距，基座终点会落入托盘的膨胀
        障碍物内。这里仅修正确定性 layout001 Gate 的几何夹具：前排从前侧
        接近并朝向 +Y，后排从后侧接近并朝向 -Y。产品 Robot Agent仍应从
        实时场景可通行区域推导工位，不能把行号或这个中心坐标写进Skill。
        """

        rear_side = row_y > PALLET_CENTER_Y_M
        pose = self._work_pose(
            x,
            row_y + standoff_m if rear_side else row_y - standoff_m,
        )
        if rear_side:
            pose["orientation_xyzw"] = [0.0, 0.0, -0.7071068, 0.7071068]
        return pose

    @staticmethod
    def _subtasks(steps: list[dict[str, Any]]) -> list[dict[str, Any]]:
        result: list[dict[str, Any]] = []
        for index, step in enumerate(steps):
            depends_on = [] if index == 0 else [steps[index - 1]["id"]]
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
                "depends_on": depends_on,
            })
        return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=("top", "layer", "pallet"), required=True)
    parser.add_argument(
        "--grasp-strategy",
        choices=("auto", "direct_bilateral", "left_extract_first", "right_extract_first"),
        default="auto",
    )
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument(
        "--source-lateral-offset-m",
        type=float,
        default=0.0,
        help="仅用于物理Gate验证单侧外拉前的机械臂余量；正值按策略向承载侧偏置",
    )
    parser.add_argument(
        "--source-standoff-m",
        type=float,
        default=WORK_STANDOFF_M,
        help="仅用于物理Gate验证来源工位距离；产品运行时由Robot Agent计算",
    )
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    gate = V050MujocoMultiGate(
        args.output_dir,
        args.case,
        args.grasp_strategy,
        source_standoff_m=args.source_standoff_m,
        source_lateral_offset_m=args.source_lateral_offset_m,
    )
    try:
        gate.run()
    except BaseException:
        logging.exception("v0.5 原生MuJoCo多箱产品Gate失败")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
