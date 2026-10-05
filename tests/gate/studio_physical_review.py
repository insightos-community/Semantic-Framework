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

"""Offline, read-only review of a studio_real_session round; never a physical gate.

Reads recorded evidence and exact candidate packages, never imports candidate code,
opens a database, calls a service, or writes into the candidate. Numeric agreement
does not establish natural posture, safe retreat, or continuity between samples.
"""
from __future__ import annotations

import argparse
import ast
from datetime import datetime
import hashlib
import itertools
import json
import math
from pathlib import Path
import re
import zipfile


class EvidenceUnavailable(ValueError):
    pass


def read_json(path):
    return json.loads(Path(path).read_text())


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise EvidenceUnavailable(f"Missing/non-finite numeric evidence: {value!r}")
    return float(value)


def vector(value, size=3):
    if not isinstance(value, list) or len(value) != size:
        raise EvidenceUnavailable(f"Expected {size}-component measured vector")
    return [number(item) for item in value]


def stamp(value):
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise EvidenceUnavailable("Evidence timestamp lacks timezone")
    return parsed.timestamp()


def unique(items, description):
    if len(items) != 1:
        raise EvidenceUnavailable(f"Expected exactly one {description}, found {len(items)}")
    return items[0]


def field_default(source, class_name, field_name):
    cls = unique([n for n in ast.parse(source).body if isinstance(n, ast.ClassDef)
                  and n.name == class_name], class_name)
    node = unique([n for n in cls.body if isinstance(n, ast.AnnAssign)
                   and isinstance(n.target, ast.Name) and n.target.id == field_name], field_name)
    value = node.value
    if isinstance(value, ast.Call) and isinstance(value.func, ast.Name) and value.func.id == "Field":
        value = unique([kw.value for kw in value.keywords if kw.arg == "default"], "Field default")
    return number(ast.literal_eval(value))


def placement_contract(source):
    """Accept only the independently reviewed VerifyPlacement/XY contract shape."""
    tree = ast.parse(source)
    stable = unique([n.value for n in ast.walk(tree) if isinstance(n, ast.Assign)
                     and any(isinstance(t, ast.Name) and t.id == "stable" for t in n.targets)],
                    "VerifyPlacement stable assignment")
    comparison = unique([n for n in ast.walk(stable) if isinstance(n, ast.Compare)
                         and isinstance(n.left, ast.Name) and n.left.id == "displacement"],
                        "displacement threshold")
    limit = number(ast.literal_eval(comparison.comparators[0]))
    expected = ast.parse(f'within and displacement <= {limit!r} and '
                         'bool(after.state.get("in_contact")) and gripper_empty', mode="eval").body
    if ast.dump(stable) != ast.dump(expected) or limit <= 0:
        raise EvidenceUnavailable("Unreviewed placement stability contract")
    within = unique([n for n in tree.body if isinstance(n, ast.FunctionDef)
                     and n.name == "_within_region"], "_within_region")
    expected_within = ast.parse('''
def _within_region(item: SceneObject, region: SceneRegion) -> bool:
    if region.extent is None:
        return _position_error(item.pose.position[:2], region.pose.position[:2]) <= 0.05
    return all(
        abs(item.pose.position[index] - region.pose.position[index])
        <= region.extent[index] / 2
        for index in (0, 1)
    )
''').body[0]
    if ast.dump(within) != ast.dump(expected_within):
        raise EvidenceUnavailable("Unreviewed placement target containment contract")
    return {"maximum_displacement_m": limit, "xy_without_extent_radius_m": 0.05,
            "xy_rule": "center within each region half-extent; not whole-box containment"}


def declared_ability_wheels(bundle_path):
    entries, in_wheels, indentation = [], False, 0
    for line in bundle_path.read_text().splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        current_indent = len(line)-len(line.lstrip())
        if line.strip() == "pythonWheels:":
            in_wheels, indentation = True, current_indent
            continue
        if in_wheels and current_indent <= indentation:
            in_wheels = False
        if in_wheels:
            match = re.fullmatch(r"\s*- (wheels/semantic_r1pro_abilities-[A-Za-z0-9_.+]+-py3-none-any\.whl)\s*", line)
            if match:
                entries.append((bundle_path.parent/match[1]).resolve())
    return entries


def active_ability_wheel(candidate, manifest):
    patch = manifest.get("ability_patch")
    if patch:
        wheel = Path(patch["package"]).resolve()
        if not wheel.is_relative_to((candidate/"bundles").resolve()):
            raise EvidenceUnavailable("Ability patch does not identify a copied candidate bundle wheel")
        declared = unique(declared_ability_wheels(wheel.parent.parent/"bundle.yaml"), "declared active Ability wheel")
        if declared != wheel or digest(wheel) != patch["sha256"]:
            raise EvidenceUnavailable("Ability patch differs from Bundle declaration or patch hash")
        return wheel
    return unique([wheel for path in candidate.glob("bundles/*/bundle.yaml")
                   for wheel in declared_ability_wheels(path)], "declared active Ability wheel")


def exact_contract(candidate, manifest, place_execution):
    installed = unique([s for s in manifest["installed"]["skills"]
                        if s["name"] == "place-object" and s["version"] == place_execution["skill_version"]],
                       "exact installed place Skill version")
    package = Path(installed["package"])
    if digest(package) != installed["sha256"]:
        raise EvidenceUnavailable("Exact place Skill package hash differs from manifest")
    with zipfile.ZipFile(package) as archive:
        member = unique([n for n in archive.namelist() if n.endswith("scripts/models.py")], "Skill models.py")
        default_ms = field_default(archive.read(member).decode(), "PlacementTarget", "stability_duration_ms")
    wheel = active_ability_wheel(candidate, manifest)
    sha = digest(wheel)
    recorded = manifest["hashes"].get(str(wheel.resolve()))
    # Earlier harness manifests hashed the source bundle before copying it. Still
    # require one exact bundle/wheels/filename entry and identical copied bytes.
    if recorded is None:
        suffix = "/" + "/".join(wheel.parts[-3:])
        recorded = unique([value for path, value in manifest["hashes"].items() if path.endswith(suffix)],
                          "recorded exact Ability bundle wheel hash")
    if recorded != sha:
        raise EvidenceUnavailable("Exact Ability wheel missing from or differs from manifest")
    with zipfile.ZipFile(wheel) as archive:
        source = archive.read("r1pro_abilities/handlers/object_perception.py").decode()
    contract = placement_contract(source)
    duration = number(place_execution["input"]["target"].get("stability_duration_ms", default_ms))
    if duration <= 0:
        raise EvidenceUnavailable("Invalid executed stability duration")
    return {**contract, "stability_duration_ms": duration, "model_default_duration_ms": default_ms,
            "place_version": installed["version"], "place_package_sha256": installed["sha256"],
            "ability_wheel": str(wheel), "ability_wheel_sha256": sha,
            "ability_source_sha256": hashlib.sha256(source.encode()).hexdigest()}


def deployment_contract(text):
    """Read only the narrow generated deployment format, not arbitrary YAML."""
    text = unique(re.findall(r'^robot:\n((?:[ \t]+[^\n]*\n)+)', text, re.M), "deployment Robot section")
    def scalar(pattern, label):
        return unique(re.findall(pattern, text, re.M), label)
    robot_id = json.loads(scalar(r'^  id: (".*")$', "deployment Robot ID"))
    instance_id = json.loads(scalar(r'^    scene_instance_id: (".*")$', "deployment instance ID"))
    options = json.loads(scalar(r'^    options: (\{.*\})$', "SDK options"))
    tolerance = number(options.get("joint_position_tolerance_rad"))
    block = scalar(r'^      travel:\n((?:        [^\n]+\n)+)', "travel posture")
    postures = {}
    for line in block.splitlines():
        match = re.fullmatch(r"        ([A-Za-z0-9_]+):\s+([-+0-9.eE]+)", line)
        if match is None or match[1] in postures:
            raise EvidenceUnavailable("Unsupported/duplicate travel joint declaration")
        postures[match[1]] = number(float(match[2]))
    sides = re.findall(r'^      side: "([^"]+)"$', text, re.M)
    if not postures or not sides or len(sides) != len(set(sides)) or tolerance <= 0:
        raise EvidenceUnavailable("Incomplete or ambiguous deployment posture/tools")
    return {"robot_id": robot_id, "instance_id": instance_id, "travel": postures,
            "joint_position_tolerance_rad": tolerance, "tool_sides": sides}


def resolve_tasks(proposal, map_before):
    entities = map_before["map_snapshot"]["entities"]
    scope = proposal.get("approved_scope", {})
    allowed_objects = scope.get("objects", [])
    allowed_regions = scope.get("regions", []) + allowed_objects
    targets = []
    for task in proposal["structured_plan"]["tasks"]:
        if task.get("required_role") != "robot":
            continue
        value = task.get("input", {})
        source, target = value.get("source", {}), value.get("target", {})
        object_ref = value.get("object_ref") or source.get("object_ref")
        target_ref = value.get("target_ref") or target.get("target_ref")
        if not object_ref or not target_ref:
            raise EvidenceUnavailable(f"Task {task['id']} has no explicit object/target mapping; do not infer from prose")
        if object_ref not in allowed_objects or target_ref not in allowed_regions:
            raise EvidenceUnavailable("Task object/target not explicitly in approved scope")
        for ref, entity_id in [(object_ref, value.get("object_entity_id") or source.get("entity_id")),
                               (target_ref, value.get("target_entity_id") or target.get("entity_id"))]:
            entity = unique([e for e in entities if e.get("properties", {}).get("source_id") == ref],
                            f"map source_id {ref}")
            if entity_id and entity["id"] != entity_id:
                raise EvidenceUnavailable(f"Plan entity ID disagrees with map source_id {ref}")
        # Plan inputs are application data, not a fixed framework schema. Accept
        # an explicitly declared place_layer too; never infer a layer from prose.
        declared_layers = [value[key] for key in ("target_layer", "place_layer") if key in value]
        if "layer" in target:
            declared_layers.append(target["layer"])
        if len({str(item) for item in declared_layers}) > 1:
            raise EvidenceUnavailable("Plan contains conflicting target layer declarations")
        layer = declared_layers[0] if declared_layers else None
        if layer not in (1, "1", "第一层") or isinstance(layer, bool):
            raise EvidenceUnavailable("Only an explicitly planned first layer is supported; review stacking manually")
        targets.append({"task_id": task["id"], "object_ref": object_ref, "target_ref": target_ref, "target_layer": 1})
    if not targets or len({t["object_ref"] for t in targets}) != len(targets):
        raise EvidenceUnavailable("No unique Robot object mappings in plan")
    return targets


def approved_workflow_tasks(planned_targets, workflow_view):
    """Proposal IDs are local labels; approval may remap actual Task IDs."""
    workflow_id = workflow_view["workflow"]["id"]
    actual_tasks = [task for task in workflow_view["tasks"]
                    if task.get("workflow_id") == workflow_id and task.get("required_role") == "robot"]
    resolved = []
    for target in planned_targets:
        matches = []
        for task in actual_tasks:
            value = task.get("input", {})
            object_ref = value.get("object_ref") or value.get("source", {}).get("object_ref")
            target_ref = value.get("target_ref") or value.get("target", {}).get("target_ref")
            if (object_ref, target_ref) == (target["object_ref"], target["target_ref"]):
                matches.append(task)
        actual = unique(matches, f"approved Workflow Task for {target['object_ref']} -> {target['target_ref']}")
        if not actual.get("id") or any(item["task_id"] == actual["id"] for item in resolved):
            raise EvidenceUnavailable("Approved Workflow Task mapping is not one-to-one")
        resolved.append({**target, "plan_task_id": target["task_id"], "task_id": actual["id"]})
    return resolved


def read_rows(path):
    rows = []
    for line_number, line in enumerate(path.read_text().splitlines(), 1):
        if line.strip():
            try:
                rows.append((line_number, json.loads(line)))
            except json.JSONDecodeError as error:
                raise EvidenceUnavailable(f"Incomplete physical JSONL at line {line_number}") from error
    return rows


def post_samples(rows, workflow):
    selected = [(line, row) for line, row in rows if row.get("label", "").startswith("post-observation-")]
    if len(selected) < 2:
        raise EvidenceUnavailable("Need at least two independent post-workflow physical samples")
    end = stamp(workflow["ended_at"])
    identity, previous = None, None
    for line, row in selected:
        scene = row["snapshot"]["snapshot"]
        current = (scene["instance_id"], scene["generation"])
        instance = row["instance"]
        if identity is not None and current != identity:
            raise EvidenceUnavailable(f"Mixed physical instance/generation at line {line}")
        if current != (instance["instance_id"], instance["generation"]) or instance.get("state") != "running":
            raise EvidenceUnavailable(f"Snapshot/instance mismatch at line {line}")
        times = (stamp(scene["observed_at"]), stamp(row["sampled_at"]), number(instance["sim_time"]))
        if row.get("workflow_id") != workflow["id"] or row.get("active_subtasks") or times[0] < end:
            raise EvidenceUnavailable(f"Sample does not belong to completed Workflow window: line {line}")
        if scene.get("coordinate_frame") != "world" or (previous and any(a <= b for a, b in zip(times, previous))):
            raise EvidenceUnavailable(f"Non-advancing or non-world physical evidence at line {line}")
        identity, previous = current, times
    return selected


def orientation_angle(left, right):
    a, b = vector(left, 4), vector(right, 4)
    norms = math.sqrt(sum(v*v for v in a)) * math.sqrt(sum(v*v for v in b))
    if norms < 1e-12:
        raise EvidenceUnavailable("Invalid physical quaternion")
    return 2 * math.acos(min(1.0, abs(sum(x*y for x, y in zip(a, b))) / norms))


def evaluate_samples(samples, target, contract, deployment):
    values, poses, times, sims = [], [], [], []
    previous_robot_at = None
    for line, row in samples:
        scene = row["snapshot"]["snapshot"]
        obj = unique([o for o in scene["objects"] if o["source_id"] == target["object_ref"]], "physical object")
        region = unique([r for r in scene["regions"] if r["source_id"] == target["target_ref"]], "physical region")
        robot = unique([r for r in scene["robots"] if r["robot_id"] == deployment["robot_id"]], "physical Robot")
        if robot["generation"] != scene["generation"] or deployment["instance_id"] != scene["instance_id"]:
            raise EvidenceUnavailable("Deployment/Robot physical identity differs from scene")
        robot_at = stamp(robot["observed_at"])
        if previous_robot_at is not None and robot_at <= previous_robot_at:
            raise EvidenceUnavailable("Robot joint/tool evidence timestamp is not advancing")
        previous_robot_at = robot_at
        position, region_position = vector(obj["pose"]["position"]), vector(region["pose"]["position"])
        extent = vector(obj["extent"])
        region_extent = vector(region["extent"]) if region.get("extent") is not None else None
        if any(v <= 0 for v in extent) or (region_extent and any(v <= 0 for v in region_extent)):
            raise EvidenceUnavailable("Invalid physical object/region extent")
        support_z = number(region["properties"]["support_z_m"])
        support_ref = region["properties"]["support_surface_ref"]
        support = unique([o for o in scene["objects"] if o["source_id"] == support_ref], "physical support surface")
        support_top = vector(support["pose"]["position"])[2] + vector(support["extent"])[2] / 2
        xy_error = math.dist(position[:2], region_position[:2])
        within = (all(abs(position[i]-region_position[i]) <= region_extent[i]/2 for i in (0, 1))
                  if region_extent else xy_error <= contract["xy_without_extent_radius_m"])
        joints = robot["joints"]
        errors = {name: abs(number(joints[name]["position"])-goal) for name, goal in deployment["travel"].items()}
        velocities = {name: abs(number(joints[name]["velocity"])) for name in deployment["travel"]}
        tools = {}
        for side in deployment["tool_sides"]:
            state = robot["gripper_states"][side]
            flags = {key: state[key] for key in ("hook_contact", "clamp_contact", "sensor_fault")}
            if any(type(value) is not bool for value in flags.values()):
                raise EvidenceUnavailable("Missing/non-boolean raw tool contact/fault signal")
            forces = {key: number(state[key]) for key in ("hook_force_n", "clamp_force_n", "hook_support_ratio")}
            tools[side] = {**flags, **forces, "position": number(state["position"]),
                           "target_position": number(state["target_position"]),
                           "end_effector_position": vector(robot["end_effectors"][side]["position"])}
        values.append({"line": line, "observed_at": scene["observed_at"], "position_m": position,
                       "quaternion_xyzw": vector(obj["pose"]["quaternion_xyzw"], 4), "xy_error_m": xy_error,
                       "within_target": within, "support_contact": obj["state"].get("in_contact") is True,
                       "support_surface_ref": support_ref, "support_z_m": support_z,
                       "physical_support_top_m": support_top,
                       "upright_first_layer_center_z_m": support_z + extent[2]/2,
                       "upright_box_bottom_minus_support_m": position[2]-extent[2]/2-support_z,
                       "orientation_error_rad": orientation_angle(obj["pose"]["quaternion_xyzw"], region["pose"]["quaternion_xyzw"]),
                       "max_travel_joint_error_rad": max(errors.values()),
                       "max_travel_joint_velocity_rad_s": max(velocities.values()), "tools": tools})
        poses.append(position)
        times.append(stamp(scene["observed_at"]))
        sims.append(number(row["instance"]["sim_time"]))
    max_drift = max(math.dist(a, b) for a, b in itertools.combinations(poses, 2))
    duration = (times[-1]-times[0])*1000
    checks = {"post_window_spans_contract": duration >= contract["stability_duration_ms"],
              "all_sample_pairs_within_stability_displacement": max_drift <= contract["maximum_displacement_m"],
              "all_samples_within_target": all(v["within_target"] for v in values),
              "all_samples_support_contact": all(v["support_contact"] for v in values),
              "all_samples_travel_joints_within_deployment_tolerance": all(
                  v["max_travel_joint_error_rad"] <= deployment["joint_position_tolerance_rad"] for v in values),
              "all_samples_tools_contact_and_force_free": all(
                  not any(tool[k] for k in ("hook_contact", "clamp_contact", "sensor_fault",
                                            "hook_force_n", "clamp_force_n", "hook_support_ratio"))
                  for v in values for tool in v["tools"].values())}
    return {**target, "samples": values, "checks": checks, "observed_duration_ms": duration,
            "simulation_duration_s": sims[-1]-sims[0], "maximum_sampling_gap_ms": max(
                b-a for a, b in zip(times, times[1:]))*1000, "maximum_pairwise_displacement_m": max_drift,
            "caution": "Discrete samples cannot prove no motion between samples. Height, natural posture and retreat require visual review."}


def image_evidence(round_dir, records):
    mappings = read_json(round_dir/"artifact-index.json") if (round_dir/"artifact-index.json").exists() else []
    images, unavailable = [], []
    for record in records:
        execution = record["execution"]
        for event in record["events"]:
            payload = event.get("payload", {})
            if event["type"] == "stage.evidence_unavailable":
                unavailable.append({"execution_id": execution["id"], "stage": payload.get("stage"),
                                    "sequence": event["sequence"], "reason": payload.get("deviation")})
            observation = payload.get("observation", {})
            refs = payload.get("evidence_refs", []) + observation.get("evidence_refs", [])
            for ref in refs:
                if not ref.startswith("pilot-artifact://"):
                    continue
                pilot, separator, local = ref.removeprefix("pilot-artifact://").partition("/")
                if not separator:
                    continue
                matched = [m for m in mappings if m.get("pilot_instance_id") == pilot
                           and m.get("local_artifact_id") == local and m.get("execution_id") == execution["id"]
                           and m.get("status") == "synced" and m.get("media_type", "").startswith("image/")]
                if len(matched) != 1 or not payload.get("stage"):
                    continue
                mapping = matched[0]
                image_path = round_dir/mapping.get("filename", "missing-image")
                if mapping.get("downloaded") is not True or not image_path.is_file():
                    continue
                # Offline download must really contain an image, not an auth/JSON error body.
                data = image_path.read_bytes()
                if not (data.startswith(b"\xff\xd8\xff") or data.startswith(b"\x89PNG\r\n\x1a\n")):
                    continue
                item = {"execution_id": execution["id"], "stage": payload["stage"],
                        "artifact_id": mapping["server_artifact_id"], "file": str(image_path),
                        "sha256": hashlib.sha256(data).hexdigest()}
                if item not in images:
                    images.append(item)
    return {"stage_images": images, "unavailable_reports": unavailable}


def review(round_dir):
    round_dir = Path(round_dir).resolve()
    report = {"round_directory": str(round_dir), "physical_result": "pending_manual_review", "missing": [],
              "checks": {}, "objects": [], "manual_review_required": [
                  "Verify first-layer support/geometry, not just a payload or XY region hit.",
                  "Review multiple actual frames through release, tool disengagement, retreat and final natural travel posture.",
                  "Review between-sample motion; no uninterrupted physical stability is inferred from sparse snapshots."]}
    try:
        candidate = round_dir.parent.parent
        manifest = read_json(candidate/"manifest.json")
        proposal = read_json(round_dir/"proposal.json")
        map_before = read_json(round_dir/"map-before.json")
        targets = resolve_tasks(proposal, map_before)
        report["planned_objects"] = targets
        workflow_view = read_json(round_dir/"workflow-final.json")
        workflow = workflow_view["workflow"]
        targets = approved_workflow_tasks(targets, workflow_view)
        report["approved_task_mapping"] = targets
        metadata = read_json(round_dir/"round.json")
        if metadata.get("workflow_id") != workflow["id"]:
            raise EvidenceUnavailable("Round/Workflow identity mismatch")
        report["workflow_id"] = workflow["id"]
        report["checks"]["workflow_completed"] = workflow["status"] == "completed"
        samples = post_samples(read_rows(round_dir/"physical.jsonl"), workflow)
        scene = samples[0][1]["snapshot"]["snapshot"]
        map_evidence = {ref for entity in map_before["map_snapshot"]["entities"] for ref in entity.get("evidence", [])
                        if ref.startswith("scene-snapshot:")}
        if map_evidence != {f"scene-snapshot:{scene['instance_id']}"}:
            raise EvidenceUnavailable("Map-before is not sourced from the physical scene instance")
        deployments = []
        for path in candidate.glob("robot-data/robots/*/*/robot-deployment.yaml"):
            contract = deployment_contract(path.read_text())
            if contract["instance_id"] == scene["instance_id"] and contract["robot_id"] == manifest["installed"]["robot_id"]:
                deployments.append({**contract, "file": str(path), "sha256": digest(path)})
        deployment = unique(deployments, "matching physical deployment")
        report["deployment"] = deployment
        records = [read_json(path) for path in sorted(round_dir.glob("execution-*.json"))]
        records = [r for r in records if r["execution"].get("workflow_id") == workflow["id"]]
        if not records:
            raise EvidenceUnavailable("Missing Workflow Execution event exports")
        report["checks"]["all_round_executions_exported"] = (
            bool(metadata.get("execution_ids")) and set(metadata["execution_ids"]) == {r["execution"]["id"] for r in records})
        report["checks"]["execution_exports_complete"] = all(r.get("complete_pagination") is True for r in records)
        report["checks"]["all_recorded_executions_completed"] = all(r["execution"]["status"] == "completed" for r in records)
        for target in targets:
            record = unique([r for r in records if r["execution"]["skill_name"] == "place-object"
                             and r["execution"].get("task_id") == target["task_id"]], "planned place Execution")
            execution = record["execution"]
            if execution["input"].get("object_ref") != target["object_ref"] or execution["input"]["target"].get("target_ref") != target["target_ref"]:
                raise EvidenceUnavailable("Actual place input disagrees with approved object/target")
            contract = exact_contract(candidate, manifest, execution)
            result = evaluate_samples(samples, target, contract, deployment)
            result["contract"] = contract
            observations = [e["payload"]["observation"] for e in record["events"]
                            if e.get("payload", {}).get("observation", {}).get("kind") == "placement.object_stability"]
            observations = [o for o in observations if o.get("subject_ref") == target["object_ref"]
                            and o.get("source") == f"mujoco-ground-truth://{scene['instance_id']}"
                            and o.get("value", {}).get("state", {}).get("target_ref") == target["target_ref"]]
            if not observations:
                report["missing"].append(f"No independently sourced placement observation: {target['object_ref']}")
            else:
                last = observations[-1]
                result["independent_placement_observation"] = last
                value = last["value"]
                result["checks"]["independent_placement_observation_consistent"] = (
                    all(value.get(k) is True for k in ("stable", "gripper_empty", "support_contact", "within_target", "independent_verification"))
                    and number(value.get("observed_duration_ms")) >= contract["stability_duration_ms"]
                    and number(value.get("observed_displacement_m")) <= contract["maximum_displacement_m"])
            report["objects"].append(result)
        report["images"] = image_evidence(round_dir, records)
        if not report["images"]["stage_images"]:
            report["missing"].append("No downloaded real Stage image with exact Execution/Pilot/Artifact association")
        successful_stages = {(image["execution_id"], image["stage"]) for image in report["images"]["stage_images"]}
        missing_stages = {(event["execution_id"], event["stage"]) for event in report["images"]["unavailable_reports"]} - successful_stages
        if missing_stages:
            report["missing"].append(f"{len(missing_stages)} reported Stage image gaps remain without a downloaded image")
        report["evidence_sha256"] = {path.name: digest(path) for path in [
            round_dir/"physical.jsonl", round_dir/"map-before.json", round_dir/"proposal.json",
            round_dir/"workflow-final.json", round_dir/"round.json", *sorted(round_dir.glob("execution-*.json"))]}
    except (EvidenceUnavailable, OSError, KeyError, TypeError, ValueError, zipfile.BadZipFile) as error:
        report["missing"].append(str(error))
    checks = list(report["checks"].values()) + [v for obj in report["objects"] for v in obj["checks"].values()]
    report["status"] = ("evidence_conflict" if False in checks else
                        "incomplete" if report["missing"] else "manual_review_required")
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("round", type=Path, help="Candidate rounds/NN directory; read-only")
    result = review(parser.parse_args().round)
    print(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False))
    return {"evidence_conflict": 1, "incomplete": 2, "manual_review_required": 0}[result["status"]]


if __name__ == "__main__":
    raise SystemExit(main())
