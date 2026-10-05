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

"""刷新制品选择回归测试，不启动服务或修改已安装 Bundle。"""

import hashlib
import os
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest.mock import patch
import zipfile

import refresh_v050_mujoco as refresh


class AbilityBuildTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.ability = self.root / "ability"
        self.type_package = self.root / "type-package"
        self.wheels = self.root / "fresh-build/wheels"
        for path in (self.ability / "dist", self.type_package, self.wheels):
            path.mkdir(parents=True)
        (self.ability / "pyproject.toml").write_text(
            '[project]\nname="semantic-r1pro-abilities"\nversion="0.4.0.dev1"\n'
        )
        self.name = "semantic_r1pro_abilities-0.4.0.dev1-py3-none-any.whl"
        self.manifest = self.type_package / "bundle.yaml"
        self.manifest.write_text(f"pythonWheels:\n  - wheels/{self.name}\n")

    def test_source_version_must_match_manifest(self):
        self.assertEqual(refresh.ability_build_version(self.ability, self.type_package), "0.4.0.dev1")
        self.manifest.write_text(self.manifest.read_text().replace("dev1", "dev0"))
        with self.assertRaisesRegex(refresh.RefreshError, "同步 Deployment 清单"):
            refresh.ability_build_version(self.ability, self.type_package)

    def test_version_is_read_from_source_not_fixed_to_current_release(self):
        metadata = self.ability / "pyproject.toml"
        metadata.write_text(metadata.read_text().replace("0.4.0.dev1", "0.4.1"))
        self.manifest.write_text(self.manifest.read_text().replace("0.4.0.dev1", "0.4.1"))
        self.assertEqual(refresh.ability_build_version(self.ability, self.type_package), "0.4.1")

    def test_build_uses_fresh_output_even_when_dist_contains_old_and_same_version_wheels(self):
        old = self.ability / "dist" / self.name.replace("dev1", "dev0")
        old.write_bytes(b"old")
        (self.ability / "dist" / self.name).write_bytes(b"stale same version")
        commands = []

        def run(command, cwd, env=None):
            commands.append(command)
            self.assertEqual(cwd, self.ability)
            if command[0] == "uv":
                self.assertEqual(command[command.index("--out-dir") + 1], str(self.wheels))
                (self.wheels / self.name).write_bytes(b"built now")

        with patch.object(refresh, "run", side_effect=run):
            wheel = refresh.build_ability_wheel(self.ability, "python3.13", self.wheels, "0.4.0.dev1", {})
        self.assertEqual(commands[0], ["make", "test"])
        self.assertEqual(wheel.read_bytes(), b"built now")
        self.assertEqual(old.read_bytes(), b"old")
        self.assertEqual((self.ability / "dist" / self.name).read_bytes(), b"stale same version")

    def test_missing_new_output_does_not_fall_back_to_dist(self):
        (self.ability / "dist" / self.name).write_bytes(b"stale")
        with patch.object(refresh, "run"), self.assertRaisesRegex(refresh.RefreshError, "本次 Ability Wheel"):
            refresh.build_ability_wheel(self.ability, "python3.13", self.wheels, "0.4.0.dev1", {})

    def test_installed_version_checked_with_bundle_python_and_reported(self):
        wheel = self.wheels / self.name
        wheel.write_bytes(b"new wheel")
        bundle = self.root / "bundle"
        with patch.object(refresh.subprocess, "check_output", return_value="0.4.0.dev1\n") as command:
            result = refresh.verify_bundle_ability(bundle, wheel, "0.4.0.dev1")
        self.assertEqual(command.call_args.args[0][0], str(bundle / "python/venv/bin/python"))
        self.assertIn("-I", command.call_args.args[0])
        self.assertIn("import pinocchio", command.call_args.args[0][-1])
        self.assertIn("libtinyxml2.so.9", command.call_args.args[0][-1])
        self.assertEqual(result, {
            "version": "0.4.0.dev1", "wheel": self.name,
            "sha256": hashlib.sha256(b"new wheel").hexdigest(),
        })
        with patch.object(refresh.subprocess, "check_output", return_value="0.4.0.dev0\n"):
            with self.assertRaisesRegex(refresh.RefreshError, "未激活"):
                refresh.verify_bundle_ability(bundle, wheel, "0.4.0.dev1")

    def test_native_dependency_failure_blocks_activation_without_host_paths(self):
        failure = subprocess.CalledProcessError(1, ["python"], output="libtinyxml2.so.9 missing")
        with patch.dict(os.environ, {"LD_LIBRARY_PATH": "/host/libs", "PYTHONPATH": "/host/python"}):
            with patch.object(refresh.subprocess, "check_output", side_effect=failure) as command:
                with self.assertRaisesRegex(refresh.RefreshError, "原生依赖加载失败，未激活"):
                    refresh.verify_bundle_ability(self.root, self.wheels / self.name, "0.4.0.dev1")
        self.assertNotIn("LD_LIBRARY_PATH", command.call_args.kwargs["env"])
        self.assertNotIn("PYTHONPATH", command.call_args.kwargs["env"])

    def test_deployment_declares_soname9_in_manifest_and_lock(self):
        deployment = Path(__file__).resolve().parents[2] / "semantic-robot-deployment"
        package = deployment / "type-packages/r1pro-mujoco"
        if not package.exists():
            self.skipTest("Deployment sibling repository is not checked out")
        required = refresh.required_cache_wheels(package)
        self.assertIn("cmeel_tinyxml2_9-9.0.0-0-py3-none-manylinux_2_28_x86_64.whl", required)
        self.assertIn("cmeel-tinyxml2-9==9.0.0", (package / "python-requirements.lock").read_text())

    def test_skill_zip_uses_source_version_and_includes_stage_capture(self):
        source = self.root / "skill"
        (source / "scripts").mkdir(parents=True)
        (source / "SKILL.md").write_text("---\nname: grasp-object\nversion: 0.4.22\n---\n")
        (source / "scripts/stage_evidence.py").write_text("# stage capture\n")
        target = self.root / "skill.zip"
        self.assertEqual(refresh.zip_skill(source, target), "0.4.22")
        with zipfile.ZipFile(target) as archive:
            self.assertIn("scripts/stage_evidence.py", archive.namelist())


if __name__ == "__main__":
    unittest.main()
