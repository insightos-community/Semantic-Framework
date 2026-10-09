# Local debugging of a native MuJoCo Robot Skill

[English](README.md) | [简体中文](README.zh-CN.md)

This entry point is only for debugging one real Robot Skill physics chain; it
does not start the Semantic Server, Web, Workflow, or Agent. `semantic-pilot`
is still responsible for the Worker, Action routing, Ability invocation, and
safety stop — you cannot execute the Skill Python directly in place of Pilot.

## Prerequisites

Before running the commands, you must already have:

1. a running MuJoCo scene instance;
2. the `robot-deployment.yaml` corresponding to that scene instance;
3. an AbilityFramework with the seven Ability types started;
4. a Python with the Robot Skill SDK and the concrete Skill's dependencies
   installed;
5. a Skill Catalog containing the `grasp_object`, `semantic_navigation`, and
   `place_object` directories.

The same Robot cannot be controlled by both the resident `semantic-pilot` and
the local debug command at the same time. Before running, stop the
corresponding resident Robot instance first, then restart the AF and Abilities
with `debug-stack`; do not bypass the cross-process mutual-exclusion boundary
just because "it looks idle right now".

`RobotDeployment` provides the Robot ID, Runtime endpoint, scene instance ID,
AbilityFramework endpoint, frame, tool, IK, and safety configuration. The Skill
input only holds the business objective and does not repeat these deployment
parameters.

## Build

```bash
cd /home/wwy/agent_refractor/.worktrees/semantic-framework-robot
go build -o .output/bin/semantic-pilot ./cmd/semantic-pilot

cd /home/wwy/agent_refractor/semantic-robot-deployment
go build -o bin/semantic-robot-instance ./cmd/semantic-robot-instance
```

## Starting the local component stack

First stop the full supervisor of the same rendered instance. This makes the
resident Pilot safely stop its current work, then stops the Abilities and AF in
turn; it does not stop the MuJoCo scene Runtime:

```bash
DEPLOY_ROOT=/home/wwy/agent_refractor/semantic-robot-deployment
FRAMEWORK_ROOT=/home/wwy/agent_refractor/.worktrees/semantic-framework-robot
INSTANCE_ROOT="$FRAMEWORK_ROOT/.output/v050-mujoco-product/three-skills-instance9"

"$DEPLOY_ROOT/bin/semantic-robot-instance" stop \
  --instance "$INSTANCE_ROOT" \
  --timeout 30s

"$DEPLOY_ROOT/bin/semantic-robot-instance" debug-stack \
  --instance "$INSTANCE_ROOT"
```

`debug-stack` is a foreground process. Keep this terminal running once you see
`status: ready`. It shares `instance.lock` with the full instance, but does not
start Pilot, does not connect to the Server, and does not use an access token.

If `stop` reports the instance was already stopped, you can run `debug-stack`
directly. If it reports `interrupted`, do not continue starting local
debugging — confirm the Robot's physical state first.

When you need to observe the Runtime's real cameras, open the sample page
shipped with the repository in another terminal:

```bash
xdg-open "$FRAMEWORK_ROOT/examples/mujoco-skill-debug/live-camera.html?endpoint=http://127.0.0.1:18090&robot=r1_pro_tote_gripper-1"
```

This page only polls the Runtime's `camera.rgb`; it does not simulate Robot
state, nor does it replace the subsequent Semantic Web Physics Viewer.

## Executing a grasp

```bash
FRAMEWORK_ROOT=/home/wwy/agent_refractor/.worktrees/semantic-framework-robot
SKILLS_ROOT=/home/wwy/agent_refractor/semantic-robot-skills
INSTANCE_ROOT="$FRAMEWORK_ROOT/.output/v050-mujoco-product/three-skills-instance9"
BUNDLE_ROOT="$FRAMEWORK_ROOT/.output/v050-mujoco-product/bundle-stable-load"

"$FRAMEWORK_ROOT/.output/bin/semantic-pilot" skill run \
  --profile "$INSTANCE_ROOT/robot-deployment.yaml" \
  --skill grasp-object@0.3.0 \
  --input "$FRAMEWORK_ROOT/examples/mujoco-skill-debug/grasp-object.json" \
  --skill-catalog "$SKILLS_ROOT/semantic_robot_skills/skills" \
  --python "$BUNDLE_ROOT/python/venv/bin/python" \
  --events "$FRAMEWORK_ROOT/.output/mujoco-skill-debug/grasp-events.jsonl" \
  --result "$FRAMEWORK_ROOT/.output/mujoco-skill-debug/grasp-result.json" \
  --timeout 10m
```

The `INSTANCE_ROOT` and `BUNDLE_ROOT` above are examples of current development
artifacts, not stable install paths. After recreating the scene instance or
rebuilding the type package, replace them with the paths generated for the new
instance. It is forbidden to only modify `scene_instance_id` while continuing
to use another scene's Robot ID or Ability instances.

In the command output:

- `events.jsonl` holds the Stage, Action, exact Ability instance, and feedback
  events;
- `result.json` holds the Skill final state, business result, errors, and
  safety stop result;
- Ctrl+C or a timeout requests `Skill stop → Ability stop → Robot hold`.

If execution enters `waiting_agent`, the local command fails explicitly; local
debugging does not simulate the Robot Agent with fixed replies. When you need
to change strategy, modify the input and re-execute from an explicit safe
state, or return to the official Semantic Framework chain.

The exit order must be: first wait for `skill run` to complete, or press
Ctrl+C in that terminal and see the stop result; then press Ctrl+C on
`debug-stack`. Do not shut down the AF and Abilities first.

## Parameter sources

| Parameter | Required | Source |
|---|---:|---|
| `--profile` | Yes | The RobotDeployment generated for the current scene/Robot |
| `--skill` | Yes | The exact `name@version` in the Skill Catalog |
| `--input` | Yes | Business input JSON; `-` also reads from stdin |
| `--skill-catalog` | Yes in development | The Robot Skill source directory; in installed mode defaults to the active directory in the Profile |
| `--python` | Explicit recommended in development | The type package Python, which must already have the Robot Skill SDK and dependencies installed |
| `--ability-framework` | No | Overrides the Profile endpoint for diagnostics only |
| `--python-path` | No | Temporary import path when source dependencies are not installed; repeatable |
| `--execution-id` | No | Specify when reproducing experiments; auto-generated by default |
| `--events` | No | JSONL event file; defaults to stderr |
| `--result` | No | Final JSON file; defaults to stdout |
| `--timeout` | No | Maximum execution time; defaults to 15 minutes |

Local mode has no `--project`, `--workflow`, `--task`, or `--robot`: the Robot
comes from the RobotDeployment, and the other objects do not exist in this
debug chain.
