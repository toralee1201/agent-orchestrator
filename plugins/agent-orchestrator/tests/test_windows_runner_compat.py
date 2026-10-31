from __future__ import annotations

import json
import importlib.util
import os
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path


RUNNER = Path(__file__).resolve().parents[1] / "scripts" / "cli_agent_job.py"
RUNNER_SPEC = importlib.util.spec_from_file_location("agent_orchestrator_runner", RUNNER)
assert RUNNER_SPEC and RUNNER_SPEC.loader
runner = importlib.util.module_from_spec(RUNNER_SPEC)
RUNNER_SPEC.loader.exec_module(runner)


class RunnerProcessCompatibilityTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "nt", "Windows PID probe regression")
    def test_pid_alive_falls_back_after_winerror_87_for_live_worker(self) -> None:
        child = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(5)"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            **runner.process_group_popen_kwargs(),
        )
        try:
            error = OSError(87, "The parameter is incorrect")
            error.winerror = 87
            with mock.patch.object(runner.os, "kill", side_effect=error):
                self.assertTrue(runner.pid_alive(child.pid))
        finally:
            child.terminate()
            child.wait(timeout=5)

    @unittest.skipUnless(os.name == "nt", "Windows PID probe regression")
    def test_pid_alive_rejects_exited_worker(self) -> None:
        child = subprocess.Popen(
            [sys.executable, "-c", "pass"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            **runner.process_group_popen_kwargs(),
        )
        child.wait(timeout=5)
        self.assertFalse(runner.pid_alive(child.pid))

    def test_opencode_profile_matches_v2_run_command(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            workspace = root / "workspace"
            workspace.mkdir()
            prompt = root / "task.md"
            prompt.write_text("Open yahoo.co.jp", encoding="utf-8")
            profile = runner.validate_profile(
                "opencode", runner.BUILTIN_PROFILES["opencode"]
            )

            command, transport = runner.build_command(
                profile,
                workspace,
                prompt,
                "opencode/gpt-5.4-nano",
                None,
                None,
                None,
                None,
            )

            self.assertEqual(transport, "arg")
            self.assertEqual(command[1], "run")
            self.assertIn("opencode/gpt-5.4-nano", command)
            self.assertIn("Open yahoo.co.jp", command)
            self.assertEqual(command[2:4], ["--format", "json"])
            self.assertNotIn("--cwd", command)
            self.assertNotIn("--quiet", command)

    def test_opencode_model_discovery_uses_resolved_windows_executable(self) -> None:
        profile = runner.validate_profile(
            "opencode", runner.BUILTIN_PROFILES["opencode"]
        )

        discovery = runner.discovery_result(
            profile,
            "discover_models_argv",
            Path.cwd(),
            None,
            10,
            False,
        )

        self.assertEqual(discovery["discovery"], "succeeded", discovery)
        self.assertIn("opencode/gpt-5.4-nano", discovery.get("output", ""))

    def test_ensure_dashboard_starts_and_reuses_detached_server(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            state_home = Path(temp) / "state"
            env = os.environ.copy()
            env["AGENT_ORCHESTRATOR_HOME"] = str(state_home)
            first = subprocess.run(
                [
                    sys.executable,
                    str(RUNNER),
                    "ensure-dashboard",
                    "--no-open",
                    "--port",
                    "0",
                    "--json",
                ],
                capture_output=True,
                text=True,
                env=env,
                timeout=15,
                check=False,
            )
            self.assertEqual(first.returncode, 0, first.stderr or first.stdout)
            first_runtime = json.loads(first.stdout)
            runtime_path = state_home / "dashboard.json"
            self.assertTrue(runtime_path.is_file())

            try:
                second = subprocess.run(
                    [
                        sys.executable,
                        str(RUNNER),
                        "ensure-dashboard",
                        "--no-open",
                        "--port",
                        "0",
                        "--json",
                    ],
                    capture_output=True,
                    text=True,
                    env=env,
                    timeout=15,
                    check=False,
                )
                self.assertEqual(second.returncode, 0, second.stderr or second.stdout)
                second_runtime = json.loads(second.stdout)
                self.assertEqual(first_runtime["pid"], second_runtime["pid"])
                self.assertEqual(first_runtime["url"], second_runtime["url"])
                self.assertTrue(second_runtime["reused"])
            finally:
                pid = first_runtime["pid"]
                os.kill(pid, signal.SIGTERM)


if __name__ == "__main__":
    unittest.main()
