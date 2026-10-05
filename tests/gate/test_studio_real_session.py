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

"""Read-only preparation helpers; no services, credentials, or physical actions."""
import argparse
from contextlib import closing
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest
import zipfile
from unittest.mock import patch

import studio_real_session as driver


class PreparationTests(unittest.TestCase):
    def test_ability_patch_changes_only_copied_wheel_manifest_and_environment(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            bundle = root / "copy"
            (bundle / "wheels").mkdir(parents=True)
            (bundle / "python/venv").mkdir(parents=True)
            old = bundle / "wheels/semantic_r1pro_abilities-0.4.0.dev0-py3-none-any.whl"
            old.write_bytes(b"original")
            package = root / "semantic_r1pro_abilities-0.4.0.dev1-py3-none-any.whl"
            package.write_bytes(b"reviewed")
            manifest = bundle / "bundle.yaml"
            manifest.write_text(f"pythonWheels:\n  - wheels/{old.name}\n  - wheels/unchanged-sdk.whl\n")
            (bundle / "wheels").chmod(0o555)
            manifest.chmod(0o444)
            with patch.object(driver.subprocess, "run") as install:
                identity = driver.candidate_ability_package(bundle, package)
            command = install.call_args.args[0]
            self.assertEqual(command[0], str(bundle / "python/venv/bin/python"))
            self.assertIn("--no-deps", command)
            self.assertIn("--no-index", command)
            self.assertEqual(old.read_bytes(), b"original")
            self.assertEqual(package.read_bytes(), b"reviewed")
            self.assertIn(package.name, manifest.read_text())
            self.assertIn("wheels/unchanged-sdk.whl", manifest.read_text())
            self.assertEqual(identity["sha256"], driver.digest(package))
            self.assertEqual(identity["baseline"]["sha256"], driver.digest(old))
            with self.assertRaises(RuntimeError):
                driver.candidate_ability_package(bundle, package)

    def test_candidate_replacement_is_explicit_versioned_and_does_not_mutate_baseline(self):
        installed = {"skills": [{"name": "navigate", "version": "1.0.0", "source": "old.zip", "sha256": "old"}]}
        with tempfile.TemporaryDirectory() as temporary:
            package = Path(temporary) / "reviewed.zip"
            with zipfile.ZipFile(package, "w") as archive:
                archive.writestr("SKILL.md", "---\nname: navigate\nversion: 1.0.1\n---\n")
            result = driver.candidate_skills(installed, [package])
            self.assertEqual(result["skills"][0]["version"], "1.0.1")
            self.assertEqual(result["skills"][0]["baseline"], {"version": "1.0.0", "sha256": "old"})
            self.assertEqual(installed["skills"][0]["version"], "1.0.0")
            with self.assertRaises(RuntimeError):
                driver.candidate_skills(installed, [package, package])
            with self.assertRaises(RuntimeError):
                driver.candidate_skills(result, [package])

    def test_web_build_and_preview_environment_cannot_inherit_live_or_fixture_targets(self):
        inherited = {"VITE_SERVER_HTTP": "http://127.0.0.1:8080", "VITE_SERVER_WS": "ws://127.0.0.1:8081",
                     "VITE_STUDIO_FIXTURES": "true", "VITE_DEVICE_FIXTURES": "true", "PATH": "original"}
        result = driver.web_environment(inherited, "http://127.0.0.1:8180", "ws://127.0.0.1:8181")
        self.assertEqual(result["VITE_SERVER_HTTP"], "http://127.0.0.1:8180")
        self.assertEqual(result["VITE_SERVER_WS"], "ws://127.0.0.1:8181")
        self.assertEqual(result["VITE_STUDIO_FIXTURES"], "false")
        self.assertEqual(result["VITE_DEVICE_FIXTURES"], "false")
        self.assertEqual(result["PATH"], "original")
        self.assertEqual(inherited["VITE_SERVER_HTTP"], "http://127.0.0.1:8080")

    def test_runtime_clone_changes_only_identity_and_endpoint(self):
        source = "schema_version: 2\ninstallation_id: original\nendpoint: http://127.0.0.1:8090\nenvironment_path: /installed/venv\n"
        actual = driver.clone_runtime(source, "isolated", 8190)
        self.assertEqual(actual, source.replace("installation_id: original", "installation_id: isolated")
                         .replace("127.0.0.1:8090", "127.0.0.1:8190"))

    def test_runtime_without_schema_or_endpoint_fails_closed(self):
        with self.assertRaises(RuntimeError):
            driver.clone_runtime("schema_version: 1\ninstallation_id: old\n", "new", 8190)

    def test_installed_versions_come_from_join_not_filename_or_registry_latest(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = root / "actually-installed.zip"
            package.write_bytes(b"current-installed-payload")
            source = root / "source.db"
            with closing(sqlite3.connect(source)) as db:
                db.executescript("""
                    CREATE TABLE robot_pilots(body BLOB, last_seen_at TEXT);
                    CREATE TABLE robot_skill_packages(name TEXT, version TEXT, body BLOB);
                    CREATE TABLE robot_pilot_skills(pilot_instance_id TEXT, name TEXT, version TEXT, enabled INTEGER, body BLOB);
                """)
                db.execute("INSERT INTO robot_pilots VALUES (?, ?)", (json.dumps({"backend": "mujoco", "robot_id": "actual-robot", "pilot_instance_id": "actual-pilot"}), "2026-01-01"))
                for version in ("1.2.3", "9.9.9"):
                    db.execute("INSERT INTO robot_skill_packages VALUES ('navigate', ?, ?)", (version, json.dumps({"package_path": str(package)})))
                db.execute("INSERT INTO robot_pilot_skills VALUES ('actual-pilot','navigate','1.2.3',1,?)", (json.dumps({"status": "installed"}),))
                db.commit()
            before = source.read_bytes()
            result = driver.inventory(source)
            self.assertEqual(result["skills"][0]["version"], "1.2.3")
            self.assertEqual(result["skills"][0]["sha256"], driver.digest(package))
            self.assertEqual(source.read_bytes(), before)

    def test_prepare_refuses_existing_output_before_any_launch_or_read(self):
        with tempfile.TemporaryDirectory() as temporary:
            framework = Path(temporary)
            existing = framework / ".output" / "retained"
            existing.mkdir(parents=True)
            with patch.object(driver, "FRAMEWORK", framework), patch.object(driver, "check_ports") as ports:
                with self.assertRaisesRegex(RuntimeError, "existing output"):
                    driver.prepare(argparse.Namespace(output=existing))
                ports.assert_not_called()


if __name__ == "__main__":
    unittest.main()
