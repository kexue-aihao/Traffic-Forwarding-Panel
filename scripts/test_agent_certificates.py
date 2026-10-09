"""Run the real installer in a temporary filesystem with mocked OS/ACME commands.

No host services are changed and no public CA is contacted. TLS validation and
live certificate rotation are covered separately by cmd/agent Go tests.
"""
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
BASH = shutil.which("bash") or r"C:\Program Files\Git\bin\bash.exe"


def shell_path(path):
    path = Path(path).resolve()
    value = path.as_posix()
    return "/" + value[0].lower() + value[2:] if os.name == "nt" else value


class CertificateInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="tfp-certificate-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / "mock-bin"
        self.bin.mkdir()
        for path in ("etc/systemd/system", "usr/local/bin", "etc/tfp-agent", "var/lib/tfp-agent"):
            (self.root / path).mkdir(parents=True, exist_ok=True)
        self.env = dict(os.environ, TFP_TEST_ROOT=shell_path(self.root), FAKE_IP4="8.8.8.8", FAKE_IP6="", MSYS_NO_PATHCONV="1")
        self.mock("id", "echo 0")
        self.mock("uname", 'if [ "$1" = -s ]; then echo Linux; else echo x86_64; fi')
        self.mock("journalctl", "exit 0")
        if os.name == "nt":
            self.mock("chmod", "exit 0")
            self.mock("install", r'''
directory=0
while [ "$#" -gt 0 ]; do
  case "$1" in -d) directory=1; shift ;; -m) shift 2 ;; *) break ;; esac
done
if [ "$directory" = 1 ]; then mkdir -p "$@"; else cp "$1" "$2"; fi
''')
        self.mock("curl", r'''
case "$*" in
  *https://api.ipify.org*) [ -n "$FAKE_IP4" ] || exit 1; printf '%s' "$FAKE_IP4"; exit ;;
  *https://api6.ipify.org*) [ -n "$FAKE_IP6" ] || exit 1; printf '%s' "$FAKE_IP6"; exit ;;
esac
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then out="$2"; shift; fi
  shift
done
if [ -n "$out" ] && [ "$out" != /dev/null ]; then printf '\177ELFfixture' > "$out"; fi
''')
        self.mock("openssl", r'''
printf '%s\n' "$*" >> "$TFP_TEST_ROOT/openssl.log"
[ "${FAIL_VERIFY:-0}" != 1 ]
''')
        self.mock("certbot", r'''
if [ "${1:-}" = --help ]; then
  if [ "${OLD_CERTBOT:-0}" = 1 ] && [[ "$0" == */mock-bin/certbot ]]; then echo legacy; else echo '--ip-address --required-profile'; fi
  exit
fi
printf '%s\n' "$*" >> "$TFP_TEST_ROOT/certbot.log"
[ "${FAIL_ISSUE:-0}" != 1 ] || exit 1
if [ "${1:-}" = renew ]; then exit; fi
config=""; name=""
while [ "$#" -gt 0 ]; do
  case "$1" in --config-dir) config="$2"; shift ;; --cert-name) name="$2"; shift ;; esac
  shift
done
mkdir -p "$config/live/$name"
printf 'fixture certificate' > "$config/live/$name/fullchain.pem"
printf 'fixture key' > "$config/live/$name/privkey.pem"
''')
        self.mock("python3", r'''
if [ "${1:-}" = -m ] && [ "${2:-}" = venv ]; then
  mkdir -p "$3/bin"
  cp "$(command -v certbot)" "$3/bin/certbot"
  cp "$(command -v python3)" "$3/bin/python"
  exit
fi
if [ "${1:-}" = -m ] && [ "${2:-}" = pip ]; then
  printf '%s\n' "$*" > "$TFP_TEST_ROOT/pip.log"
  exit
fi
exec ''' + shlex.quote(shell_path(sys.executable)) + ' "$@"')
        self.mock("systemctl", r'''
printf '%s\n' "$*" >> "$TFP_TEST_ROOT/systemctl.log"
if [ "${FAIL_TIMER:-0}" = 1 ] && [[ "$*" == 'is-active --quiet tfp-cert-renew.timer' ]]; then exit 1; fi
if [[ "$*" == 'restart tfp-agent.service' ]]; then
  printf '{"kind":"identity"}' > "$TFP_TEST_ROOT/var/lib/tfp-agent/agent-state.json"
fi
''')

    def mock(self, name, body):
        path = self.bin / name
        path.write_text("#!/usr/bin/env bash\nset -euo pipefail\n" + body + "\n", encoding="utf-8", newline="\n")
        path.chmod(0o755)

    def run_script(self, name="agent-install.sh", extra=(), env=None):
        script = (ROOT / "internal/agentdist" / name).read_text(encoding="utf-8")
        for prefix in ("/etc/systemd/system", "/etc/tfp-agent", "/var/lib/tfp-agent", "/var/log/tfp-agent-acme", "/usr/local/bin/tfp-agent"):
            script = script.replace(prefix, shell_path(self.root) + prefix)
        staged = self.root / name
        staged.write_text(script, encoding="utf-8", newline="\n")
        args = [] if name == "agent-uninstall.sh" else ["-t", "fixture-key", "-u", "https://panel.example.com", "-m", "secure-direct", "-q", "public-ip", "-e", "fixture-exit-token", "-p", "secure-direct", "-O", "random-padding"]
        command = 'export PATH=' + shlex.quote(shell_path(self.bin)) + ':"$PATH"; exec bash ' + shlex.quote(shell_path(staged)) + " " + shlex.join(args + list(extra))
        return subprocess.run([BASH, "-c", command], capture_output=True, text=True, encoding="utf-8", errors="replace", env=dict(self.env, **(env or {})), timeout=30)

    def success(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_exit_modes_install_without_target_allowlist(self):
        for mode, transport in (("exit", "tls"), ("secure-direct", "secure-direct")):
            with self.subTest(mode=mode):
                self.success(self.run_script(extra=("-m", mode, "-p", transport)))
                service = (self.root / "etc/systemd/system/tfp-exit.service").read_text(encoding="utf-8")
                self.assertIn("-mode " + mode, service)
                self.assertNotIn("-allow", service)

    def test_legacy_target_option_is_ignored(self):
        self.success(self.run_script(extra=("-w", "tcp|127.0.0.1:8080,udp|127.0.0.1:5353")))
        service = (self.root / "etc/systemd/system/tfp-exit.service").read_text(encoding="utf-8")
        self.assertNotIn("-allow", service)
        self.assertNotIn("127.0.0.1:8080", service)

    def test_ip_install_and_renewal_without_server_name(self):
        self.success(self.run_script(extra=("-S", "stale.example.com")))
        issuance = (self.root / "certbot.log").read_text(encoding="utf-8")
        self.assertIn("--ip-address 8.8.8.8", issuance)
        self.assertIn("--required-profile shortlived", issuance)
        self.assertIn("--server https://acme-v02.api.letsencrypt.org/directory", issuance)
        self.assertNotIn("stale.example.com", issuance)
        self.assertIn("-checkip 8.8.8.8", (self.root / "openssl.log").read_text(encoding="utf-8"))
        timer = self.root / "etc/systemd/system/tfp-cert-renew.timer"
        self.assertIn("00,06,12,18:00:00", timer.read_text(encoding="utf-8"))
        self.assertIn("Persistent=true", timer.read_text(encoding="utf-8"))
        service = (self.root / "etc/systemd/system/tfp-cert-renew.service").read_text(encoding="utf-8")
        renew = next(line.removeprefix("ExecStart=") for line in service.splitlines() if line.startswith("ExecStart="))
        result = subprocess.run([BASH, "-c", renew], env=self.env, capture_output=True, text=True, timeout=10)
        self.success(result)
        self.assertIn("renew --non-interactive --cert-name tfp-exit-public-ip", (self.root / "certbot.log").read_text(encoding="utf-8"))

    def test_ipv6_fallback_and_endpoint(self):
        result = self.run_script(env={"FAKE_IP4": "invalid", "FAKE_IP6": "2606:4700:4700::1111"})
        self.success(result)
        self.assertIn("[2606:4700:4700::1111]:9443", result.stdout)
        self.assertIn("--ip-address 2606:4700:4700::1111", (self.root / "certbot.log").read_text(encoding="utf-8"))
        self.assertIn("-listen [::]:9443", (self.root / "etc/systemd/system/tfp-exit.service").read_text(encoding="utf-8"))

    def test_invalid_private_and_multicast_ips_are_rejected(self):
        for address in ("10.0.0.1", "127.0.0.1", "224.0.0.1", "invalid", "8.8.8.8\n--other-option"):
            with self.subTest(address=address):
                result = self.run_script(env={"FAKE_IP4": address})
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "certbot.log").exists())
                self.assertFalse((self.root / "etc/systemd/system/tfp-exit.service").exists())

    def test_old_certbot_is_replaced_by_isolated_install(self):
        self.success(self.run_script(env={"OLD_CERTBOT": "1"}))
        self.assertIn("certbot>=5.4,<6", (self.root / "pip.log").read_text(encoding="utf-8"))
        service = (self.root / "etc/systemd/system/tfp-cert-renew.service").read_text(encoding="utf-8")
        self.assertIn("/etc/tfp-agent/certbot/bin/certbot renew", service)

    def test_issuance_or_verification_failure_does_not_install_exit(self):
        for flag in ("FAIL_ISSUE", "FAIL_VERIFY"):
            with self.subTest(flag=flag):
                result = self.run_script(env={flag: "1"})
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "etc/systemd/system/tfp-exit.service").exists())
                self.assertFalse((self.root / "etc/systemd/system/tfp-cert-renew.timer").exists())

    def test_missing_service_name_still_rejected_for_existing_certificates(self):
        result = self.run_script(extra=("-q", "provided"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("-S", result.stderr)

    def test_inactive_renewal_timer_fails_installation(self):
        result = self.run_script(env={"FAIL_TIMER": "1"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("定时器", result.stderr)

    def test_reinstall_and_both_uninstall_paths_remove_renewal(self):
        self.success(self.run_script())
        self.success(self.run_script(extra=("-q", "provided", "-S", "8.8.8.8", "-C", shell_path(self.root / "etc/tfp-agent/acme/live/tfp-exit-public-ip/fullchain.pem"), "-K", shell_path(self.root / "etc/tfp-agent/acme/live/tfp-exit-public-ip/privkey.pem"))))
        self.assertFalse((self.root / "etc/systemd/system/tfp-cert-renew.timer").exists())
        for name, extra in (("agent-install.sh", ("-x",)), ("agent-uninstall.sh", ())):
            with self.subTest(name=name):
                self.success(self.run_script())
                self.success(self.run_script(name, extra))
                self.assertFalse((self.root / "etc/systemd/system/tfp-cert-renew.timer").exists())
                self.assertFalse((self.root / "etc/systemd/system/tfp-cert-renew.service").exists())
                self.assertTrue((self.root / "etc/tfp-agent/acme/live/tfp-exit-public-ip/fullchain.pem").exists())


if __name__ == "__main__":
    unittest.main()
