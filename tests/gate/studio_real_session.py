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

"""Staged real Studio acceptance: prepare/build, then serve (never starts a scene).

Uses only Python's standard library. Source DB is always opened read-only. No
cleanup/delete operation is provided; an existing output directory is refused.
Physical actions belong to the separately invoked browser round driver.
"""

from __future__ import annotations

import argparse
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import sqlite3
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile

FRAMEWORK = Path(__file__).resolve().parents[2]
WORKSPACE = FRAMEWORK.parent
MODEL = "deepseek-v4-flash"
AGENT_SKILLS = ["depalletizing-workflow-planning", "depalletizing-robot-task"]
MEMORY = """本 Project 是 R1 Pro 拆码垛验收现场。来源托盘是待拆垛区，目标托盘是接收区。
来源与目标的对应关系按当前 Map 中同一行列后缀匹配；搬到目标对应列的第一层。
当前最上面一层、对象数量和稳定引用须查询本轮 Map，不能沿用上一轮 generation、对象 ID 或 Pose。
放稳是实际箱体落入对应列且保持稳定；最终双工具为空并恢复 Robot Profile 定义的 travel 姿态。
操作策略和技能调用约定见本 Project 绑定的拆码垛 Agent Skills；具体输入契约以当前 Robot 的实际已安装目录为准。
"""


def save(path: Path, value, *, private=False):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
    if private:
        path.chmod(0o600)


def digest(path: Path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def inventory(source_db: Path):
    with closing(sqlite3.connect(source_db.as_uri() + "?mode=ro", uri=True)) as db:
        pilots = db.execute("SELECT body FROM robot_pilots ORDER BY last_seen_at DESC").fetchall()
        pilot = next((json.loads(row[0]) for row in pilots
                      if json.loads(row[0]).get("backend") == "mujoco"), None)
        if not pilot:
            raise RuntimeError("Source DB has no installed MuJoCo Pilot")
        rows = db.execute("""SELECT s.name, s.version, p.body FROM robot_pilot_skills s
            JOIN robot_skill_packages p ON p.name=s.name AND p.version=s.version
            WHERE s.pilot_instance_id=? AND s.enabled=1
            AND json_extract(s.body,'$.status')='installed' ORDER BY s.name""",
                          (pilot["pilot_instance_id"],)).fetchall()
    skills = []
    for name, version, body in rows:
        source = Path(json.loads(body)["package_path"])
        if not source.is_absolute():
            source = FRAMEWORK / source
        source = source.resolve(strict=True)
        skills.append(dict(name=name, version=version, source=str(source), sha256=digest(source)))
    if not skills:
        raise RuntimeError("Source Pilot has no enabled installed Skill packages")
    return {"robot_id": pilot["robot_id"], "pilot_instance_id": pilot["pilot_instance_id"],
            "pilot_version": pilot.get("pilot_version"), "skills": skills}


def check_ports(ports):
    held = []
    try:
        for port in ports:
            sock = socket.socket()
            held.append(sock)
            sock.bind(("127.0.0.1", port))
    finally:
        for sock in held:
            sock.close()


def candidate_skills(installed, packages):
    """Explicit reviewed replacements only; never discover a Registry 'latest'."""
    result = json.loads(json.dumps(installed))
    by_name = {skill["name"]: skill for skill in result["skills"]}
    seen = set()
    for package in packages:
        package = package.resolve(strict=True)
        with zipfile.ZipFile(package) as archive:
            header = archive.read("SKILL.md").decode("utf-8").split("---", 2)[1]
        fields = {}
        for field in ("name", "version"):
            match = re.search(rf"^{field}:\s*([a-zA-Z0-9_.+-]+)\s*$", header, re.MULTILINE)
            if not match:
                raise RuntimeError(f"Candidate SKILL.md requires an explicit {field}")
            fields[field] = match[1]
        name = fields["name"]
        if name not in by_name or name in seen:
            raise RuntimeError("Candidate must replace one existing installed Skill exactly once")
        previous = by_name[name]
        if fields["version"] == previous["version"]:
            raise RuntimeError("Changed candidate Skill must use a distinct version")
        previous.update(version=fields["version"], source=str(package), sha256=digest(package),
                        baseline=dict(version=previous["version"], sha256=previous["sha256"]))
        seen.add(name)
    return result


def web_environment(inherited, http_base, ws_base):
    # Both build-time flags and preview-time proxy targets must be explicit.
    env = dict(inherited)
    env.update(VITE_STUDIO_FIXTURES="false", VITE_DEVICE_FIXTURES="false",
               VITE_SERVER_HTTP=http_base, VITE_SERVER_WS=ws_base)
    return env


def candidate_ability_package(bundle: Path, package: Path):
    """Apply one reviewed pure-Python Ability patch to the copied test Bundle."""
    package = package.resolve(strict=True)
    if not re.fullmatch(r"semantic_r1pro_abilities-[A-Za-z0-9_.+]+-py3-none-any\.whl", package.name):
        raise RuntimeError("Expected an explicit R1 Pro Ability pure-Python Wheel")
    manifest = bundle / "bundle.yaml"
    content = manifest.read_text()
    entries = re.findall(r"wheels/(semantic_r1pro_abilities-[A-Za-z0-9_.+]+-py3-none-any\.whl)", content)
    if len(entries) != 1 or entries[0] == package.name:
        raise RuntimeError("Candidate Ability Wheel must replace one manifest entry with a distinct version")
    original = bundle / "wheels" / entries[0]
    destination = bundle / "wheels" / package.name
    identity = {"source": str(package), "package": str(destination), "sha256": digest(package),
                "baseline": {"package": str(original), "sha256": digest(original)}}
    # Published Bundles are read-only. Make only this copied installation
    # writable for its explicit patch; the source Bundle keeps its permissions.
    for root in (bundle / "wheels", bundle / "python/venv"):
        for item in [root, *root.rglob("*")]:
            if not item.is_symlink():
                item.chmod(item.stat().st_mode | 0o200)
    manifest.chmod(manifest.stat().st_mode | 0o200)
    bundle.chmod(bundle.stat().st_mode | 0o200)
    shutil.copyfile(package, destination)
    # Invoke the copied interpreter, not a pip script with an old absolute shebang.
    with (bundle / "ability-patch-install.log").open("w") as log:
        subprocess.run([str(bundle / "python/venv/bin/python"), "-m", "pip", "install", "--no-index",
                        "--no-deps", "--force-reinstall", str(destination)],
                       stdout=log, stderr=log, check=True)
    manifest.write_text(content.replace("wheels/" + entries[0], "wheels/" + package.name))
    return identity


def clone_runtime(text, installation_id, port):
    for field, value in (("installation_id", installation_id),
                         ("endpoint", f"http://127.0.0.1:{port}")):
        text, count = re.subn(rf"^{field}:.*$", f"{field}: {value}", text, flags=re.MULTILINE)
        if count != 1:
            raise RuntimeError(f"Runtime descriptor must contain one top-level {field}")
    if not re.search(r"^schema_version: 2\s*$", text, re.MULTILINE):
        raise RuntimeError("Expected current installed schema 2 Runtime descriptor")
    return text


def prepare(args):
    output = args.output.resolve()
    if not output.is_relative_to(FRAMEWORK / ".output") or output == FRAMEWORK / ".output":
        raise RuntimeError("Output must be a new descendant of semantic-framework/.output")
    if output.exists():
        raise RuntimeError("Refusing existing output; choose a new candidate directory")
    ports = [args.http_port, args.ws_port, args.runtime_port, args.web_port,
             *range(args.ability_port, args.ability_port + 10)]
    if len(set(ports)) != len(ports) or any(p in (8080, 8081, 8090) for p in ports):
        raise RuntimeError("Ports overlap each other or the user's existing services")
    check_ports(ports)
    installed = candidate_skills(inventory(args.source_db.resolve()), args.skill_package)
    runtime_text = clone_runtime(args.runtime.read_text(), "studio-" + output.name, args.runtime_port)
    profile = re.search(r"^\s+runtime_profile_id:\s*(\S+)", runtime_text, re.MULTILINE)
    if not profile or not args.bundle.is_dir():
        raise RuntimeError("Installed Runtime profile or Robot bundle is missing")
    output.mkdir(parents=True, mode=0o700)
    for name in ("bin", "runtimes.d", "packages", "data", "robot-data", "rounds"):
        (output / name).mkdir()
    shutil.copytree(FRAMEWORK / "configs", output / "configs")
    (output / "runtimes.d" / "studio.yaml").write_text(runtime_text)
    # Bundle discovery deliberately skips symlink directories. Copy the installed
    # bytes so the candidate also cannot change the user's installed bundle.
    (output / "bundles").mkdir()
    shutil.copytree(args.bundle.resolve(), output / "bundles" / args.bundle.name)
    ability_patch = (candidate_ability_package(output / "bundles" / args.bundle.name, args.ability_package)
                     if args.ability_package else None)
    for skill in installed["skills"]:
        destination = output / "packages" / f"{skill['name']}-{skill['version']}.zip"
        shutil.copyfile(skill["source"], destination)
        skill["package"] = str(destination)
    http = f"http://127.0.0.1:{args.http_port}"
    ws = f"ws://127.0.0.1:{args.ws_port}"
    config = {
        "server": {"http_addr": f"127.0.0.1:{args.http_port}", "ws_addr": f"127.0.0.1:{args.ws_port}",
                   "read_timeout": "60s", "write_timeout": "60s"},
        "log": {"level": "info"}, "store": {"driver": "sqlite", "sqlite_path": str(output / "data/semantic.db")},
        "llm": {"default": MODEL, "providers": {MODEL: {"component": "openai", "service": "deepseek",
            "base_url": "https://api.deepseek.com/v1", "model": MODEL,
            "capabilities": ["text", "tool_call", "reasoning_effort"], "options": {},
            "price": {"prompt": 0, "completion": 0}}}},
        "agents": {"profiles_dir": str(output / "configs/agents"), "teams_dir": str(output / "configs/agents/teams")},
        "skills": {"dir": str(output / "configs/skills")}, "execution": {"allow_host": False},
        "simulation": {"runtimes_dir": str(output / "runtimes.d"), "catalog_dir": str(output / "configs/scenes.d")},
        "robot_runtime": {"enabled": True, "bundles_dir": str(output / "bundles"),
            "data_root": str(output / "robot-data"), "server_http_url": http, "server_websocket_url": ws + "/ws/pilot",
            "ability_port_first": args.ability_port, "ability_port_last": args.ability_port + 9}, "mcp_servers": []}
    save(output / "semantic-server.yaml", config)  # JSON is valid YAML; no external parser needed.
    env = web_environment(os.environ, http, ws)
    with (output / "build.log").open("w") as log:
        subprocess.run(["go", "build", "-o", str(output / "bin/semantic-server"), "./cmd/semantic-server"],
                       cwd=FRAMEWORK, stdout=log, stderr=log, check=True)
        subprocess.run(["mise", "exec", "--", "npx", "vite", "build", "--outDir", str(output / "web")],
                       cwd=WORKSPACE / "semantic-web", env=env, stdout=log, stderr=log, check=True)
    runtime_sources = WORKSPACE / "semantic-simulation/mujoco-runtime"
    physical_sources = WORKSPACE / "semantic-scene/mujoco-asset"
    source_files = [p for root in (runtime_sources, physical_sources) for p in root.rglob("*")
                    if p.suffix in (".py", ".xml", ".yaml", ".json", ".toml", ".lock")
                    and not any(part in (".venv", ".git", ".output", "__pycache__") for part in p.parts)]
    inputs = [output / "bin/semantic-server", output / "semantic-server.yaml", output / "runtimes.d/studio.yaml",
              *source_files, *sorted((output / "configs").rglob("*")),
              *sorted((output / "web").rglob("*")),
              *sorted((output / "bundles" / args.bundle.name).rglob("*")), args.runtime]
    manifest = {"version": 1, "prepared_at": time.time(), "output": str(output), "installed": installed,
        "source_db": str(args.source_db.resolve()), "model": MODEL, "http_base": http, "ws_base": ws,
        "web_base": f"http://127.0.0.1:{args.web_port}", "ports": ports, "web_port": args.web_port,
        "installation_id": "studio-" + output.name, "runtime_profile_id": profile[1], "variant": args.variant,
        "ability_patch": ability_patch,
        "hashes": {str(p.resolve()): digest(p) for p in inputs if p.is_file()}}
    save(output / "manifest.json", manifest)
    print(f"Prepared {output}; no Server, Runtime, scene or Robot action started.")


class API:
    def __init__(self, base):
        self.base, self.token = base, ""

    def request(self, method, path, data=None, binary=False):
        body = data if binary else json.dumps(data).encode() if data is not None else None
        headers = {"Content-Type": "application/zip" if binary else "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        req = urllib.request.Request(self.base + path, body, headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=60) as response:
                return json.load(response)
        except urllib.error.HTTPError as exc:
            # Do not expose response bodies from Settings/auth (may contain credentials).
            raise RuntimeError(f"{method} {path}: HTTP {exc.code}") from None


def serve(args):
    output = args.output.resolve(strict=True)
    manifest = json.loads((output / "manifest.json").read_text())
    if (output != Path(manifest["output"]) or (output / "session.private.json").exists()
            or (output / "data/semantic.db").exists()):
        raise RuntimeError("Candidate already served or moved; preserve it and prepare a new candidate")
    for path, expected in manifest["hashes"].items():
        if digest(Path(path)) != expected:
            raise RuntimeError(f"Candidate input changed after preparation: {path}")
    check_ports(manifest["ports"])
    processes = []
    api = API(manifest["http_base"])
    env = os.environ.copy()
    env.pop("SEMANTIC_MOCK_SCRIPT", None)
    password = secrets.token_urlsafe(32)
    env.update(SEMANTIC_ADMIN_PASSWORD=password, SEMANTIC_MUJOCO_GL="egl", PYTHONDONTWRITEBYTECODE="1",
        SEMANTIC_MUJOCO_WORKDIR=str(WORKSPACE / "semantic-simulation/mujoco-runtime"),
        SEMANTIC_MUJOCO_ASSET_ROOT=str(WORKSPACE / "semantic-scene/mujoco-asset"))

    def launch(name, command, cwd, process_env):
        stream = (output / f"{name}.log").open("a")
        process = subprocess.Popen(command, cwd=cwd, env=process_env, stdout=stream, stderr=stream, start_new_session=True)
        processes.append((process, stream))
        save(output / "owned-processes.json", [{"pid": p.pid, "process_group": p.pid} for p, _ in processes])
        return process

    try:
        server = launch("server", [str(output / "bin/semantic-server"), "-c", str(output / "semantic-server.yaml")], output, env)
        deadline = time.monotonic() + 60
        while True:
            if server.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("Isolated Server did not become healthy; inspect server.log")
            try:
                api.request("GET", "/api/v1/system/healthz")
                break
            except (RuntimeError, OSError):
                time.sleep(0.5)
        api.token = api.request("POST", "/api/v1/auth/login", {"username": "admin", "password": password})["token"]
        save(output / "login.private.json", {"username": "admin", "password": password,
            "web_base": manifest["web_base"]}, private=True)
        password = ""
        with closing(sqlite3.connect(Path(manifest["source_db"]).as_uri() + "?mode=ro", uri=True)) as db:
            row = db.execute("SELECT key_value FROM settings_keys WHERE name='deepseek'").fetchone()
        key = row[0] if row else ""
        if not key:
            raise RuntimeError("Source DB has no managed DeepSeek key")
        api.request("PUT", "/api/v1/settings/keys/deepseek", {"key_value": key})
        key, row = "", None
        if api.request("GET", "/api/v1/settings").get("key_sources", {}).get(MODEL) != "store":
            raise RuntimeError("Isolated Server is not using its managed DeepSeek key")
        for skill in manifest["installed"]["skills"]:
            package = Path(skill["package"])
            if digest(package) != skill["sha256"]:
                raise RuntimeError("Copied Robot Skill package hash changed")
            api.request("POST", "/api/v1/robot-skills", package.read_bytes(), binary=True)
            # Keep the candidate's desired catalog aligned with these exact
            # reviewed packages; the copied Deployment may pin older versions.
            robot_id = urllib.parse.quote(manifest["installed"]["robot_id"], safe="")
            name, version = (urllib.parse.quote(skill[field], safe="") for field in ("name", "version"))
            api.request("POST", f"/api/v1/devices/{robot_id}/skills/{name}/{version}/install")
        project = api.request("POST", "/api/v1/projects", {"name": "Studio real " + output.name})["project"]
        base = "/api/v1/projects/" + project["id"]
        api.request("PUT", base + "/bindings", {"agent_ids": [], "skill_names": AGENT_SKILLS})
        memory = api.request("GET", base + "/memory")["memory"]
        api.request("PUT", base + "/memory", {"content": MEMORY, "revision": memory["revision"]})
        api.request("PUT", base + "/simulation/runtime-preference", {
            "runtime_profile_id": manifest["runtime_profile_id"], "preferred_runtime_installation_id": manifest["installation_id"]})
        scenes = api.request("GET", "/api/v1/simulation/scene-catalog?project_id=" + project["id"])["scenes"]
        scene = next(s for s in scenes if s["scene_id"] == "depalletizing-r1pro")
        version = next(v for v in scene["versions"] if any(x["variant_id"] == manifest["variant"] for x in v["variants"]))
        project_scene = api.request("POST", base + "/simulation/project-scenes", {
            "catalog_scene_id": scene["scene_id"], "scene_version": version["version"],
            "default_variant_id": manifest["variant"]})["project_scene"]
        web_env = web_environment(env, manifest["http_base"], manifest["ws_base"])
        web = launch("web", ["mise", "exec", "--", "npx", "vite", "preview", "--host", "127.0.0.1", "--port",
            str(manifest["web_port"]), "--strictPort", "--outDir", str(output / "web")], WORKSPACE / "semantic-web", web_env)
        proxy = API(manifest["web_base"])
        proxy.token = api.token
        deadline = time.monotonic() + 30
        while True:
            if web.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("Web proxy did not confirm the isolated Project identity")
            try:
                # A health check alone cannot distinguish the user's live Server.
                # Only the freshly created isolated Project exists behind this token.
                proxied = proxy.request("GET", base)["project"]
                if proxied["id"] != project["id"]:
                    raise RuntimeError("Web proxy returned another Project")
                break
            except (RuntimeError, OSError):
                time.sleep(0.5)
        save(output / "session.private.json", {"token": api.token, "project_id": project["id"],
            "http_base": manifest["http_base"], "web_base": manifest["web_base"], "output": str(output),
            "project_scene": project_scene, "installed": manifest["installed"], "variant": manifest["variant"]}, private=True)
        print(f"Isolated Studio prepared at {manifest['web_base']}/projects/{project['id']}/studio", flush=True)
        print("No scene started. Invoke browser plan / execute separately. Ctrl+C stops only this session's owned processes.", flush=True)
        while all(p.poll() is None for p, _ in processes):
            time.sleep(1)
        raise RuntimeError("An isolated process exited; inspect its log")
    finally:
        for process, stream in reversed(processes):
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
            stream.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["prepare", "serve"])
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--source-db", type=Path, default=FRAMEWORK / ".output/data/semantic.db")
    parser.add_argument("--runtime", type=Path, default=FRAMEWORK / ".output/runtimes.d/dev-native-mujoco.yaml")
    parser.add_argument("--bundle", type=Path, default=FRAMEWORK / ".output/robot-bundles/r1pro-mujoco-0.5.0-dev")
    parser.add_argument("--variant", choices=["layout001", "layout_smoke"], default="layout001")
    parser.add_argument("--ability-package", type=Path,
                        help="Explicit reviewed Ability Wheel applied only to the copied candidate Bundle")
    parser.add_argument("--skill-package", type=Path, action="append", default=[],
                        help="Explicit reviewed Skill ZIP for this isolated candidate only; repeatable")
    for name, port in (("http", 8180), ("ws", 8181), ("runtime", 8190), ("web", 4180), ("ability", 19100)):
        parser.add_argument(f"--{name}-port", type=int, default=port)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        (prepare if args.command == "prepare" else serve)(args)
    except KeyboardInterrupt:
        print("Owned session stopped; evidence and database retained.")


if __name__ == "__main__":
    main()
