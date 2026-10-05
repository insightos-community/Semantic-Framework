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

"""真实 DeepSeek 驱动的 v0.5 原生 MuJoCo 拆码垛产品 Gate。

本 Gate 复用确定性 Gate 已验证的数据面，只把 Leader、Robot Task Planning、
Robot Task Execution 和最终总结切换到真实模型。密钥从开发安装库读取后通过
正式 Settings API 写入隔离测试库，任何日志和结果文件都不会包含密钥值。
"""

from __future__ import annotations

import argparse
import logging
import os
import sqlite3
import time
import urllib.parse
from pathlib import Path
from typing import Any

from v050_mujoco_multi import V050MujocoMultiGate
from v050_mujoco_product import PLAN_MARKER, SKILL_VERSIONS
from v050_real_gate import GateError, TERMINAL_WORKFLOWS


ROBOT_ID = "r1_pro_tote_gripper-1"
MODEL_ENDPOINT = "deepseek-v4-flash"
TERMINAL_RUNS = {"completed", "failed", "cancelled", "canceled", "interrupted"}


class V050MujocoDeepSeekGate(V050MujocoMultiGate):
    """用真实模型验证 layout001 单箱或一层的计划、执行和总结。"""

    def __init__(self, output_dir: Path, case: str) -> None:
        if case == "single":
            # 单箱继续复用已经真实跑通的右外侧箱，避免把物理回归混入模型扩展。
            super().__init__(output_dir, "top", "right_extract_first")
        elif case == "layer":
            # 一层必须让Leader从当前Map生成四个Task，Robot Agent再按实时环境
            # 决定每个箱体的抓取策略和执行参数，Gate不替模型写死四条物理链。
            super().__init__(output_dir, "layer", "auto")
        else:
            raise GateError(f"尚未实现的真实模型 Gate case: {case}")
        self.model_case = case
        self.summary["model_gate_layout"] = "layout001"
        self.source_store = Path(os.environ.get(
            "SEMANTIC_DEEPSEEK_SOURCE_DB",
            str(self.framework_repo / ".output" / "data" / "semantic.db"),
        )).resolve()
        self.summary["model_gate"] = {
            "case": case,
            "endpoint": MODEL_ENDPOINT,
            "runs": [],
            "metering": {
                "calls": 0,
                "prompt_tokens": 0,
                "completion_tokens": 0,
                "total_tokens": 0,
            },
        }

    def run(self) -> None:
        super().run()

    def _start_product_server(self) -> None:
        self.current_step = "start_deepseek_product_server"
        config = self.server_dir / "semantic-server.yaml"
        config.write_text(
            f"""server:
  http_addr: "127.0.0.1:{self.http_port}"
  ws_addr: "127.0.0.1:{self.ws_port}"
  read_timeout: 60s
  write_timeout: 60s
log:
  level: info
store:
  driver: sqlite
  sqlite_path: {self.server_dir / "semantic.db"}
llm:
  default: {MODEL_ENDPOINT}
  providers:
    {MODEL_ENDPOINT}:
      component: openai
      service: deepseek
      base_url: https://api.deepseek.com/v1
      model: {MODEL_ENDPOINT}
      capabilities: [text, tool_call, reasoning_effort]
      options: {{}}
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
        # 真实模型 Gate 不能继承 Mock 脚本，否则会形成“配置是 DeepSeek，
        # 实际仍由脚本响应”的假通过。
        env.pop("SEMANTIC_MOCK_SCRIPT", None)
        env.update({
            "SEMANTIC_ADMIN_PASSWORD": "v050-mujoco-deepseek-admin",
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
            {"username": "admin", "password": "v050-mujoco-deepseek-admin"},
            authenticated=False,
        )
        self.token = str(response["token"])
        self._install_managed_model_key()

    def _install_managed_model_key(self) -> None:
        """安全复制已有服务级密钥，不把值写入日志或测试报告。"""
        self.current_step = "install_deepseek_key"
        if not self.source_store.is_file():
            raise GateError(f"DeepSeek 配置库不存在：{self.source_store}")
        with sqlite3.connect(f"file:{self.source_store}?mode=ro", uri=True) as connection:
            row = connection.execute(
                "SELECT key_value FROM settings_keys WHERE name = ?", ("deepseek",)
            ).fetchone()
        key = str(row[0]) if row and row[0] else ""
        if len(key) < 8:
            raise GateError("semantic.db 未配置可用的 deepseek 服务级密钥")
        try:
            self._request_json(
                "PUT", "/api/v1/settings/keys/deepseek", {"key_value": key}
            )
        finally:
            key = ""
        settings = self._request_json("GET", "/api/v1/settings")
        if (settings.get("key_sources") or {}).get(MODEL_ENDPOINT) != "store":
            raise GateError("隔离 Server 未从托管密钥库解析 DeepSeek key")

    def _run_single_box_workflow(self) -> None:
        self.current_step = f"deepseek_layout001_{self.model_case}_workflow"
        prompt = self._single_box_prompt() if self.model_case == "single" else self._layer_prompt()
        self._send_plan_message(prompt)
        proposal = self._wait_real_ready_proposal(
            timeout=180 if self.model_case == "single" else 300
        )
        if pending := self._pending_interactions():
            raise GateError(f"目标完整明确，不应产生 Interaction：{pending}")
        approved = self._request_json(
            "POST",
            f"/api/v1/projects/{self.project_id}/plan-proposals/{proposal['id']}/approve",
            {"revision": int(proposal["revision"])},
        )
        workflow = (approved.get("workflow_view") or {}).get("workflow") or {}
        self.workflow_id = str(workflow.get("id") or "")
        if not self.workflow_id:
            raise GateError(f"Proposal 批准未原子创建 Workflow：{approved}")
        view = self._wait_real_workflow_terminal(
            timeout=900 if self.model_case == "single" else 2400
        )
        if view["workflow"].get("status") != "completed":
            raise GateError(f"真实模型 {self.model_case} Workflow 未完成：{view}")
        self.summary["workflow"] = view
        self._write_json(self.output_dir / "workflow.json", {"workflow_view": view})
        self._validate_real_task_and_subtasks(view)

    def _single_box_prompt(self) -> str:
        move = self.moves[0]
        source_pose = self._source_work_pose(move)
        target_pose = self._target_work_pose(move)
        return f"""{PLAN_MARKER}
请在当前 Project 中为下面这个明确目标生成可审阅的 Plan Proposal：

使用 Robot {ROBOT_ID}，将周转箱 {move.object_ref} 搬到堆叠列
{move.target_ref}。双侧工具是 component://tool/left 和 component://tool/right。
该箱位于layout001顶层右外侧，抓取意图使用right_extract_first。

来源物体可操作基座位姿为 {source_pose}；目标堆叠列的最终放置工位为
{target_pose}。

计划中只建立一个 Robot Task，Task 输入保留上述对象、目标列、工具、抓取意图和两个基座
位姿。允许的 Robot Skill 为 semantic-navigation、grasp-object、place-object，
版本由批准后实际 Robot 的已安装目录确定。Robot Agent应生成：来源导航、抓取并
整理携物姿态、一次携物导航、放置并恢复travel姿态这四个串行SubTask。

完成标准：箱体稳定位于目标列，双工具为空，Robot恢复travel姿态。先用map.query
确认上述身份；确认后不要询问是否开始规划，不要在批准前执行Robot动作，并直接
调用plan.suggest。"""


    def _layer_prompt(self) -> str:
        return f"""PLAN-V050-MUJOCO-LAYER-DEEPSEEK
请在当前Project的layout001中完成一层周转箱拆垛，并直接生成可审阅的Plan Proposal。

先调用map.query读取当前Map，识别来源托盘当前可访问的顶层周转箱，以及目标托盘
四个可用堆叠列。将同一行列后缀的来源箱体分配到对应目标列，每个箱体建立一个
Robot Task；对象数量、引用和Map实体必须来自查询结果，不能假设固定ID或旧generation。

使用当前在线Robot {ROBOT_ID}，双侧工具为component://tool/left和
component://tool/right。允许的Robot Skill只有semantic-navigation、grasp-object和
place-object，版本由批准后实际Robot目录确定。Task input必须把map.query返回的来源
对象稳定name原样保存为object_ref、目标堆叠列稳定name原样保存为target_ref；entity_id
只作可选规划来源，不能只保存r1-c2等局部标签，也不能自行拼接引用。Task input还可保存
几何提示，但不生成Stage、Action、轨迹，也不复制任何前序Skill完整结果。

每个Robot Agent应规划四个串行SubTask：来源导航、抓取并整理携物姿态、一次携物
导航、放置并恢复travel姿态。来源/目标基座Pose由Robot Agent在执行前结合当前Map、
Robot Profile、实时Robot状态和工作距离计算；不要使用approach Region。

四个Task之间只建立真实的来源遮挡或同列放置依赖；单Robot的串行执行由资源锁处理，
不要建立无意义的全局串行链。完成标准是四个箱体分别稳定进入对应目标列第一层、
双工具为空、Robot恢复travel姿态。目标已明确，不要询问是否开始规划；查询完成后
直接调用plan.suggest，批准前不得执行Robot动作。"""

    def _wait_real_ready_proposal(self, *, timeout: float) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            self._ensure_processes_alive()
            if pending := self._pending_interactions():
                raise GateError(f"明确单箱计划产生了不必要的 Interaction：{pending}")
            try:
                body = self._request_json(
                    "GET", f"/api/v1/projects/{self.project_id}/plan-proposals/active"
                )
                last = body.get("plan_proposal") or {}
                if last.get("status") == "ready":
                    if int(last.get("revision") or 0) != 1:
                        raise GateError(f"Leader 重复提交了 Plan Proposal：{last}")
                    return last
            except GateError as exc:
                if "返回 404" not in str(exc):
                    raise
            self._raise_on_failed_runs()
            time.sleep(0.5)
        raise GateError(f"真实 DeepSeek 未生成 ready Proposal：{last}")

    def _wait_real_workflow_terminal(self, *, timeout: float) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        last: dict[str, Any] = {}
        paused_since: dict[str, float] = {}
        while time.monotonic() < deadline:
            self._ensure_processes_alive()
            self._raise_on_failed_runs()
            if pending := self._pending_interactions():
                raise GateError(f"明确单箱 Task 出现未预期 Interaction：{pending}")
            last = self._workflow_view(self.workflow_id)
            executions = self._request_json(
                "GET", f"/api/v1/projects/{self.project_id}/robot-executions?page_size=100"
            ).get("robot_executions") or []
            expected_executions = len(self.moves) * 4
            if len(executions) > expected_executions:
                raise GateError(f"真实模型重复创建 Robot Execution：{executions}")
            preparation_failed = [
                task for task in last.get("tasks") or []
                if task.get("reason") == "task_preparation_failed"
            ]
            if preparation_failed:
                raise GateError(f"Robot Task Planning 未形成可执行 SubTask：{preparation_failed}")
            paused = [
                task for task in last.get("tasks") or []
                if task.get("status") == "paused"
            ]
            now = time.monotonic()
            paused_ids = {str(task.get("id")) for task in paused}
            for task_id in list(paused_since):
                if task_id not in paused_ids:
                    paused_since.pop(task_id, None)
            blocked: list[dict[str, Any]] = []
            for task in paused:
                task_id = str(task.get("id"))
                elapsed = now - paused_since.setdefault(task_id, now)
                reason = str(task.get("reason") or "")
                # failed事件与异步Recovery切换间有极短窗口；Gate只等待真实的
                # Recovery状态，不调用通用Resume，也不掩盖其他paused原因。
                if reason == "robot_execution_failed" and elapsed < 5:
                    continue
                if reason == "recovery_running" and elapsed < 180:
                    continue
                blocked.append(task)
            if blocked:
                raise GateError(f"真实执行进入需要处理的暂停状态：{blocked}")
            if last["workflow"].get("status") in TERMINAL_WORKFLOWS:
                return last
            time.sleep(0.5)
        raise GateError(f"真实 DeepSeek Workflow 未进入终态：{last}")

    def _validate_real_task_and_subtasks(self, view: dict[str, Any]) -> None:
        tasks = view.get("tasks") or []
        if len(tasks) != len(self.moves) or any(
            task.get("required_role") != "robot" for task in tasks
        ):
            raise GateError(
                f"{self.model_case} Proposal必须为每个箱体建立一个Robot Task：{tasks}"
            )
        # 复用多箱产品Gate的必要契约检查：每个Task四步、实际安装版本、
        # 不复制执行输入，并且每个SubTask都关联一条真实Robot Execution。
        self._validate_multi_tasks(view)

    def _pending_interactions(self) -> list[dict[str, Any]]:
        return self._request_json(
            "GET",
            f"/api/v1/interactions?status=pending&project_id={urllib.parse.quote(self.project_id)}",
        ).get("interactions") or []

    def _raise_on_failed_runs(self) -> None:
        body = self._request_json(
            "GET", f"/api/v1/projects/{self.project_id}/runs?page_size=100"
        )
        failed = [run for run in body.get("runs") or [] if run.get("status") == "failed"]
        if failed:
            raise GateError(f"真实 Agent Run 失败：{failed}")

    def _wait_workflow_summary(self) -> None:
        # 通用产品 Gate 的 30 秒足以覆盖 Mock 总结，但 DeepSeek 的真实流式
        # 总结可能超过该时限。Workflow 和物理执行已终结时继续等待同一个
        # 幂等 Summary Run，不应由测试清理提前取消它并误报产品失败。
        super()._wait_workflow_summary(timeout=120)
        self._collect_model_evidence()

    def _collect_model_evidence(self) -> None:
        deadline = time.monotonic() + 90
        runs: list[dict[str, Any]] = []
        while time.monotonic() < deadline:
            body = self._request_json(
                "GET", f"/api/v1/projects/{self.project_id}/runs?page_size=100"
            )
            runs = body.get("runs") or []
            if runs and all(run.get("status") in TERMINAL_RUNS for run in runs):
                break
            time.sleep(0.5)
        else:
            raise GateError(f"Workflow终态后仍有Agent Run未收敛：{runs}")
        failed = [run for run in runs if run.get("status") != "completed"]
        if failed:
            raise GateError(f"真实 Agent Run 未全部完成：{failed}")
        model_runs = [run for run in runs if run.get("endpoint")]
        dispatch_runs = [run for run in runs if not run.get("endpoint")]
        if not model_runs or any(run.get("endpoint") != MODEL_ENDPOINT for run in model_runs):
            raise GateError(f"存在非 DeepSeek 模型 Run：{model_runs}")

        expected_kinds = {
            "conversation": 2,
            "task_planning": len(self.moves),
            "task_execution": len(self.moves) * 4,
        }
        actual_kinds = {
            kind: sum(1 for run in model_runs if run.get("kind") == kind)
            for kind in expected_kinds
        }
        missing_kinds = {
            kind: expected - actual_kinds.get(kind, 0)
            for kind, expected in expected_kinds.items()
            if actual_kinds.get(kind, 0) < expected
        }
        if missing_kinds:
            raise GateError(f"真实Agent Run缺少必要阶段 {missing_kinds}：{model_runs}")
        metering = {"calls": 0, "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
        per_run: list[dict[str, Any]] = []
        for run in model_runs:
            trace_id = str(run.get("trace_id") or "")
            if not trace_id:
                raise GateError(f"真实 Agent Run 缺少 Trace：{run}")
            body = self._request_json(
                "GET", f"/api/v1/metering/traces/{trace_id}?page_size=100"
            )
            records = body.get("records") or []
            run_metering = {
                "run_id": run.get("id"), "kind": run.get("kind"),
                "calls": len(records), "prompt_tokens": 0,
                "completion_tokens": 0, "total_tokens": 0,
            }
            for record in records:
                run_metering["prompt_tokens"] += int(record.get("prompt_tokens") or 0)
                run_metering["completion_tokens"] += int(record.get("completion_tokens") or 0)
                run_metering["total_tokens"] += int(record.get("total_tokens") or 0)
            # Gate只记录真实ReAct成本，不用固定调用次数否决已经完成的产品链。
            # 重复同一工具且没有新增证据由Agent Runtime在运行中终止。
            per_run.append(run_metering)
            for field in ("calls", "prompt_tokens", "completion_tokens", "total_tokens"):
                metering[field] += int(run_metering[field])
        self.summary["model_gate"]["runs"] = runs
        self.summary["model_gate"]["model_runs"] = model_runs
        self.summary["model_gate"]["dispatch_runs"] = dispatch_runs
        self.summary["model_gate"]["metering"] = metering
        self.summary["model_gate"]["per_run_metering"] = per_run
        self._write_json(self.output_dir / "agent-runs.json", {
            "runs": runs, "model_runs": model_runs,
            "dispatch_runs": dispatch_runs, "metering": metering,
            "per_run_metering": per_run,
        })


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=("single", "layer"), default="single")
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    gate = V050MujocoDeepSeekGate(args.output_dir, args.case)
    try:
        gate.run()
    except BaseException:
        logging.exception("v0.5 原生 MuJoCo DeepSeek 产品 Gate 失败")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
