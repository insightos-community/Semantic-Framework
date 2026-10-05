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

"""Synthetic offline evidence only: no service, database, model or Robot actions."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

import studio_physical_review as review


ABILITY = '''
def verify():
    stable = within and displacement <= 0.01 and bool(after.state.get("in_contact")) and gripper_empty

def _within_region(item: SceneObject, region: SceneRegion) -> bool:
    if region.extent is None:
        return _position_error(item.pose.position[:2], region.pose.position[:2]) <= 0.05
    return all(
        abs(item.pose.position[index] - region.pose.position[index])
        <= region.extent[index] / 2
        for index in (0, 1)
    )
'''
DEPLOYMENT = '''robot:
  id: "fixture-robot"
  sdk:
    scene_instance_id: "fixture-scene"
    options: {"joint_position_tolerance_rad":0.009}
  tools:
    - tool_ref: "component://tool/left"
      side: "left"
    - tool_ref: "component://tool/right"
      side: "right"
  kinematics:
    named_postures:
      travel:
        arm_joint: 0.25
pilot:
  id: "not-the-robot-id"
'''
TARGET = {"task_id": "move", "object_ref": "real-box-id", "target_ref": "real-slot-id", "target_layer": 1}


def sample(index, position=None):
    when = f"2026-09-07T11:00:{index+1:02d}+00:00"
    tool = {"hook_contact": False, "clamp_contact": False, "sensor_fault": False,
            "hook_force_n": 0, "clamp_force_n": 0, "hook_support_ratio": 0,
            "position": 0.03, "target_position": 0.035}
    pose = lambda xyz: {"position": xyz, "quaternion_xyzw": [0, 0, 0, 1], "frame_id": "world"}
    return {"label": f"post-observation-{index}", "workflow_id": "wf", "sampled_at": when,
            "active_subtasks": [], "instance": {"instance_id": "fixture-scene", "generation": 1,
                                                 "state": "running", "sim_time": index+10},
            "snapshot": {"snapshot": {"instance_id": "fixture-scene", "generation": 1,
                "coordinate_frame": "world", "observed_at": when,
                "objects": [{"source_id": "real-box-id", "pose": pose(position or [2, 3, 0.32]),
                             "extent": [0.6, 0.4, 0.34], "state": {"in_contact": True}},
                            {"source_id": "pallet", "pose": pose([2, 3, 0.075]), "extent": [1.2, 1, 0.15]}],
                "regions": [{"source_id": "real-slot-id", "pose": pose([2, 3, 0.16]), "extent": [0.56, 0.36, 0.02],
                             "properties": {"support_surface_ref": "pallet", "support_z_m": 0.15}}],
                "robots": [{"robot_id": "fixture-robot", "generation": 1, "observed_at": when,
                            "joints": {"arm_joint": {"position": 0.2501, "velocity": 0.000001}},
                            "gripper_states": {"left": copy.deepcopy(tool), "right": copy.deepcopy(tool)},
                            "end_effectors": {"left": pose([1.5, 2, 0.6]), "right": pose([2.5, 2, 0.6])}}]}}}


def robot(row):
    return row["snapshot"]["snapshot"]["robots"][0]


class ReviewTests(unittest.TestCase):
    def setUp(self):
        self.deployment = review.deployment_contract(DEPLOYMENT)
        self.contract = {**review.placement_contract(ABILITY), "stability_duration_ms": 1000}
        self.rows = [(10+i, sample(i)) for i in range(4)]
        self.workflow = {"id": "wf", "ended_at": "2026-09-07T11:00:00Z"}

    def evaluate(self):
        return review.evaluate_samples(self.rows, TARGET, self.contract, self.deployment)

    def test_reads_real_nonzero_posture_and_reports_geometry_separately(self):
        result = self.evaluate()
        self.assertTrue(all(result["checks"].values()))
        self.assertAlmostEqual(result["samples"][-1]["max_travel_joint_error_rad"], 0.0001)
        self.assertAlmostEqual(result["samples"][-1]["upright_box_bottom_minus_support_m"], 0)
        self.assertEqual(result["observed_duration_ms"], 3000)
        self.assertNotIn("passed", result)

    def test_drift_checks_middle_sample_not_only_identical_endpoints(self):
        self.rows[1][1]["snapshot"]["snapshot"]["objects"][0]["pose"]["position"][0] += 0.02
        result = self.evaluate()
        self.assertFalse(result["checks"]["all_sample_pairs_within_stability_displacement"])
        self.assertAlmostEqual(result["maximum_pairwise_displacement_m"], 0.02)

    def test_short_post_window_is_not_stable(self):
        self.contract["stability_duration_ms"] = 5000
        self.assertFalse(self.evaluate()["checks"]["post_window_spans_contract"])

    def test_missing_joint_or_tool_is_not_assumed_zero_or_empty(self):
        del robot(self.rows[1][1])["joints"]["arm_joint"]
        with self.assertRaises(KeyError):
            self.evaluate()
        self.rows[1] = (11, sample(1))
        del robot(self.rows[1][1])["gripper_states"]["right"]["hook_contact"]
        with self.assertRaises(KeyError):
            self.evaluate()

    def test_actual_nontravel_joint_or_retained_contact_is_reported(self):
        robot(self.rows[-1][1])["joints"]["arm_joint"]["position"] = 0
        robot(self.rows[-1][1])["gripper_states"]["right"]["hook_contact"] = True
        result = self.evaluate()
        self.assertFalse(result["checks"]["all_samples_travel_joints_within_deployment_tolerance"])
        self.assertFalse(result["checks"]["all_samples_tools_contact_and_force_free"])

    def test_nan_cannot_evade_geometric_check(self):
        robot(self.rows[0][1])["joints"]["arm_joint"]["position"] = float("nan")
        with self.assertRaises(review.EvidenceUnavailable):
            self.evaluate()

    def test_scene_advancing_does_not_make_stale_robot_samples_fresh(self):
        robot(self.rows[1][1])["observed_at"] = robot(self.rows[0][1])["observed_at"]
        with self.assertRaisesRegex(review.EvidenceUnavailable, "not advancing"):
            self.evaluate()

    def test_duplicate_timestamps_and_mixed_generations_are_incomplete(self):
        self.assertEqual(len(review.post_samples(self.rows, self.workflow)), 4)
        self.rows[1][1]["snapshot"]["snapshot"]["generation"] = 2
        with self.assertRaisesRegex(review.EvidenceUnavailable, "Mixed"):
            review.post_samples(self.rows, self.workflow)
        self.rows[1] = (11, sample(0))
        with self.assertRaisesRegex(review.EvidenceUnavailable, "Non-advancing"):
            review.post_samples(self.rows, self.workflow)

    def test_other_workflow_or_preterminal_samples_are_rejected(self):
        self.rows[1][1]["workflow_id"] = "other"
        with self.assertRaises(review.EvidenceUnavailable):
            review.post_samples(self.rows, self.workflow)
        self.rows[1] = (11, sample(1))
        self.workflow["ended_at"] = "2026-09-07T11:00:03Z"
        with self.assertRaises(review.EvidenceUnavailable):
            review.post_samples(self.rows, self.workflow)

    def test_contract_shape_change_requires_new_review(self):
        with self.assertRaisesRegex(review.EvidenceUnavailable, "Unreviewed"):
            review.placement_contract(ABILITY.replace(' and gripper_empty', ''))

    def test_plan_resolves_stable_ids_not_prose_or_fixed_smoke_names(self):
        proposal = {"approved_scope": {"objects": ["real-box-id"], "regions": ["real-slot-id"]},
                    "structured_plan": {"tasks": [{"id": "move", "required_role": "robot", "input": {
                        "object_ref": "real-box-id", "target_ref": "real-slot-id", "target_layer": 1}}]}}
        map_before = {"map_snapshot": {"entities": [{"id": "entity-box", "properties": {"source_id": "real-box-id"}},
                                                  {"id": "entity-slot", "properties": {"source_id": "real-slot-id"}}]}}
        self.assertEqual(review.resolve_tasks(proposal, map_before), [TARGET])
        proposal["structured_plan"]["tasks"][0]["input"]["object_entity_id"] = "wrong"
        with self.assertRaisesRegex(review.EvidenceUnavailable, "disagrees"):
            review.resolve_tasks(proposal, map_before)

    def test_repeated_plan_id_is_mapped_only_within_each_approved_workflow(self):
        previous_tasks = []
        for index in range(5):
            workflow_id, task_id = f"wf-{index}", f"task-{index}"
            actual = {"id": task_id, "workflow_id": workflow_id, "required_role": "robot",
                      "input": {"object_ref": TARGET["object_ref"], "target_ref": TARGET["target_ref"]}}
            view = {"workflow": {"id": workflow_id}, "tasks": [*previous_tasks, actual]}
            resolved = review.approved_workflow_tasks([TARGET], view)
            self.assertEqual(resolved[0]["plan_task_id"], "move")
            self.assertEqual(resolved[0]["task_id"], task_id)
            previous_tasks.append(actual)

    def test_explicit_place_layer_is_accepted_but_conflicts_are_rejected(self):
        value = {"object_ref": "real-box-id", "target_ref": "real-slot-id", "place_layer": 1}
        proposal = {"approved_scope": {"objects": ["real-box-id"], "regions": ["real-slot-id"]},
                    "structured_plan": {"tasks": [{"id": "move", "required_role": "robot", "input": value}]}}
        map_before = {"map_snapshot": {"entities": [
            {"id": "entity-box", "properties": {"source_id": "real-box-id"}},
            {"id": "entity-slot", "properties": {"source_id": "real-slot-id"}}]}}
        self.assertEqual(review.resolve_tasks(proposal, map_before), [TARGET])
        value["target_layer"] = 2
        with self.assertRaisesRegex(review.EvidenceUnavailable, "conflicting"):
            review.resolve_tasks(proposal, map_before)
        del value["target_layer"]
        value["place_layer"] = True
        with self.assertRaisesRegex(review.EvidenceUnavailable, "explicitly planned first layer"):
            review.resolve_tasks(proposal, map_before)

    def test_remapped_actual_task_id_replaces_local_plan_id_without_mutating_plan(self):
        actual = {"id": "task-after-approval", "workflow_id": "wf", "required_role": "robot",
                  "input": {"source": {"object_ref": TARGET["object_ref"]},
                            "target": {"target_ref": TARGET["target_ref"]}}}
        view = {"workflow": {"id": "wf"}, "tasks": [actual]}
        resolved = review.approved_workflow_tasks([TARGET], view)
        self.assertEqual(resolved, [{**TARGET, "plan_task_id": "move", "task_id": "task-after-approval"}])
        self.assertEqual(TARGET["task_id"], "move")
        view["tasks"].append({**actual, "id": "ambiguous-second-task"})
        with self.assertRaisesRegex(review.EvidenceUnavailable, "exactly one approved Workflow Task"):
            review.approved_workflow_tasks([TARGET], view)

    def test_same_plan_id_cannot_override_mismatched_actual_object_or_target(self):
        for changed in ("object_ref", "target_ref"):
            with self.subTest(changed=changed):
                value = {"object_ref": TARGET["object_ref"], "target_ref": TARGET["target_ref"], changed: "other"}
                view = {"workflow": {"id": "wf"}, "tasks": [{"id": "move", "workflow_id": "wf",
                        "required_role": "robot", "input": value}]}
                with self.assertRaisesRegex(review.EvidenceUnavailable, "exactly one approved Workflow Task"):
                    review.approved_workflow_tasks([TARGET], view)

    def test_exact_package_hash_and_executed_duration_override(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = root/"place.zip"
            with zipfile.ZipFile(package, "w") as archive:
                archive.writestr("scripts/models.py", "class PlacementTarget:\n    stability_duration_ms: int = Field(default=1200, gt=0)\n")
            wheel = root/"bundles/fixture/wheels/semantic_r1pro_abilities-test-py3-none-any.whl"
            wheel.parent.mkdir(parents=True)
            (wheel.parent.parent/"bundle.yaml").write_text(f"pythonWheels:\n  - wheels/{wheel.name}\n")
            with zipfile.ZipFile(wheel, "w") as archive:
                archive.writestr("r1pro_abilities/handlers/object_perception.py", ABILITY)
            manifest = {"installed": {"skills": [{"name": "place-object", "version": "fixture-version",
                         "package": str(package), "sha256": review.digest(package)}]},
                        "hashes": {str(wheel): review.digest(wheel)}}
            execution = {"skill_version": "fixture-version", "input": {"target": {"stability_duration_ms": 2500}}}
            contract = review.exact_contract(root, manifest, execution)
            self.assertEqual(contract["stability_duration_ms"], 2500)
            self.assertEqual(contract["model_default_duration_ms"], 1200)
            manifest["installed"]["skills"][0]["sha256"] = "invalid"
            with self.assertRaisesRegex(review.EvidenceUnavailable, "hash differs"):
                review.exact_contract(root, manifest, execution)

    def test_active_wheel_comes_from_patch_and_bundle_not_retained_baseline_or_latest(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            wheels = root/"bundles/fixture/wheels"
            wheels.mkdir(parents=True)
            old = wheels/"semantic_r1pro_abilities-0.4.0.dev0-py3-none-any.whl"
            active = wheels/"semantic_r1pro_abilities-0.4.0.dev1-py3-none-any.whl"
            unrelated = wheels/"semantic_r1pro_abilities-9.9.9-py3-none-any.whl"
            for path in (old, active, unrelated):
                path.write_bytes(path.name.encode())
            bundle = wheels.parent/"bundle.yaml"
            bundle.write_text(f"spec:\n  artifacts:\n    pythonWheels:\n      - wheels/{active.name}\n    pilot: bin/pilot\n")
            manifest = {"ability_patch": {"package": str(active), "sha256": review.digest(active),
                                         "baseline": {"package": str(old), "sha256": review.digest(old)}}}
            self.assertEqual(review.active_ability_wheel(root, manifest), active)
            self.assertEqual(review.active_ability_wheel(root, {}), active)
            bundle.write_text(f"pythonWheels:\n  - wheels/{old.name}\n")
            with self.assertRaisesRegex(review.EvidenceUnavailable, "differs"):
                review.active_ability_wheel(root, manifest)

    def test_incomplete_round_never_claims_physical_success_or_writes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            before = list(root.rglob("*"))
            result = review.review(root)
            self.assertEqual(result["status"], "incomplete")
            self.assertEqual(result["physical_result"], "pending_manual_review")
            self.assertTrue(result["manual_review_required"])
            self.assertEqual(list(root.rglob("*")), before)

    def test_image_join_rejects_cross_execution_and_json_auth_body(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            mapping = {"pilot_instance_id": "pilot", "local_artifact_id": "local", "execution_id": "other",
                       "status": "synced", "media_type": "image/jpeg", "downloaded": True,
                       "server_artifact_id": "art", "filename": "image.bin"}
            record = {"execution": {"id": "exec"}, "events": [{"type": "stage.evidence", "sequence": 1,
                      "payload": {"stage": "release", "evidence_refs": ["pilot-artifact://pilot/local"]}}]}
            (root/"artifact-index.json").write_text(json.dumps([mapping]))
            (root/"image.bin").write_bytes(b"\xff\xd8\xffsynthetic-unit-test-only")
            self.assertEqual(review.image_evidence(root, [record])["stage_images"], [])
            mapping["execution_id"] = "exec"
            (root/"artifact-index.json").write_text(json.dumps([mapping]))
            self.assertEqual(len(review.image_evidence(root, [record])["stage_images"]), 1)
            (root/"image.bin").write_bytes(b'{"error":"unauthorized"}')
            self.assertEqual(review.image_evidence(root, [record])["stage_images"], [])


if __name__ == "__main__":
    unittest.main()
