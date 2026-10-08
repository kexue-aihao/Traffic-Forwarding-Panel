"""Exercise password recovery through the real manager with a Docker stand-in."""

import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
BASH = shutil.which("bash") or r"C:\Program Files\Git\bin\bash.exe"
PASSWORD_PATTERN = r"[A-Za-z0-9]{8}(?:-[A-Za-z0-9]{8}){3}"


def shell_path(path):
    value = Path(path).resolve().as_posix()
    return "/" + value[0].lower() + value[2:] if os.name == "nt" else value


class PasswordRecoveryTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="tfp-manager-test-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.bin = self.root / "mock-bin"
        self.bin.mkdir()
        self.install = self.root / "panel"
        self.install.mkdir()
        for name in ("compose.yaml", ".env"):
            (self.install / name).touch()
        self.env = dict(os.environ, TFP_TEST_ROOT=shell_path(self.root), MSYS_NO_PATHCONV="1")
        # Password recovery itself needs no root privileges in this isolated test.
        script = (ROOT / "scripts/panel-manager.sh").read_text(encoding="utf-8")
        self.script = self.root / "panel-manager.sh"
        self.script.write_text(script.replace("((EUID == 0))", "true"), encoding="utf-8", newline="\n")
        self.mock("docker", r'''
case "$*" in
    info|"compose version") exit 0 ;;
    "compose ps -q panel") printf 'fixture-container\n' ;;
    "inspect --format {{.State.Running}} fixture-container")
        printf '%s\n' "${PANEL_RUNNING:-true}" ;;
    "compose exec -T panel /panel -reset-password "*|"compose run --rm -T --no-deps panel -reset-password "*)
        printf '%s\n' "$@" > "$TFP_TEST_ROOT/reset-args"
        cat > "$TFP_TEST_ROOT/reset-input"
        if [[ ${RESET_STATUS:-0} != 0 ]]; then
            printf 'account not found\n' >&2
            exit "$RESET_STATUS"
        fi
        printf '本机账号操作完成。\n'
        ;;
    *) printf 'Unexpected Docker invocation: %s\n' "$*" >&2; exit 99 ;;
esac
''')

    def mock(self, name, body):
        path = self.bin / name
        path.write_text("#!/usr/bin/env bash\nset -euo pipefail\n" + body + "\n", encoding="utf-8", newline="\n")
        path.chmod(0o755)

    def run_manager(self, action="reset-password", extra=(), input="", env=None):
        args = [action, "--dir", shell_path(self.install), *extra]
        command = "export PATH=" + shlex.quote(shell_path(self.bin)) + ':"$PATH"; exec bash '
        command += shlex.quote(shell_path(self.script)) + " " + shlex.join(args)
        result = subprocess.run(
            [BASH, "-c", command], input=input.encode("utf-8"), capture_output=True,
            env=dict(self.env, **(env or {})), timeout=30,
        )
        result.stdout = result.stdout.decode("utf-8")
        result.stderr = result.stderr.decode("utf-8")
        return result

    def check_reset(self, result, username, running=True):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        match = re.search(r"^新密码：(.*)$", result.stdout, re.MULTILINE)
        self.assertIsNotNone(match, result.stdout)
        password = match[1]
        self.assertRegex(password, "^" + PASSWORD_PATTERN + "$")
        self.assertEqual((self.root / "reset-input").read_text(encoding="utf-8"), password + "\n")
        expected = ["compose", "exec", "-T", "panel", "/panel"] if running else [
            "compose", "run", "--rm", "-T", "--no-deps", "panel",
        ]
        self.assertEqual((self.root / "reset-args").read_text(encoding="utf-8").splitlines(), expected + ["-reset-password", username])
        self.assertIn("账号：" + username, result.stdout)
        self.assertEqual(result.stdout.count(password), 1)
        self.assertNotIn(password, result.stderr)
        self.assertEqual((self.install / ".env").read_bytes(), b"")
        return password

    def test_running_panel_receives_generated_password_on_stdin(self):
        self.check_reset(self.run_manager(extra=("--admin", "site-admin")), "site-admin")

    def test_stopped_panel_uses_one_off_container(self):
        self.check_reset(self.run_manager(env={"PANEL_RUNNING": "false"}), "admin", running=False)

    def test_menu_needs_only_username_and_keeps_exit_selection(self):
        result = self.run_manager("menu", input="3\ncustom-admin\n0\n")
        self.check_reset(result, "custom-admin")
        self.assertEqual(result.stdout.count("Traffic-Forwarding-Panel 管理"), 2)

    def test_reset_failure_does_not_display_a_password(self):
        result = self.run_manager(env={"RESET_STATUS": "43"})
        self.assertEqual(result.returncode, 43, result.stdout + result.stderr)
        self.assertNotIn("新密码：", result.stdout)
        self.assertNotIn("密码重置成功", result.stdout)
        self.assertIn("account not found", result.stderr)
        password = (self.root / "reset-input").read_text(encoding="utf-8").strip()
        self.assertNotIn(password, result.stdout + result.stderr)

    def test_menu_reports_failure_and_returns_to_menu(self):
        result = self.run_manager("menu", input="3\nmissing-admin\n0\n", env={"RESET_STATUS": "43"})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("密码重置未完成（退出码 43）", result.stderr)
        self.assertNotIn("新密码：", result.stdout)
        self.assertEqual(result.stdout.count("Traffic-Forwarding-Panel 管理"), 2)

    def test_random_source_failure_does_not_attempt_reset(self):
        self.mock("od", "exit 1")
        result = self.run_manager()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("无法生成随机密码", result.stderr)
        self.assertNotIn("新密码：", result.stdout)
        self.assertFalse((self.root / "reset-args").exists())


if __name__ == "__main__":
    unittest.main()
