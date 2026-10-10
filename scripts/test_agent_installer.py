"""Exercise distro dependency and service branches in the real installer.

Uses the same temporary filesystem as the ACME tests. No package manager,
system service, host file or public endpoint is changed.
"""
import shutil
import unittest

from test_agent_certificates import InstallerSandbox, shell_path


AGENT_ARGS = ["-t", "fixture-key", "-u", "https://panel.example.com"]
PACKAGE_MOCK = r'''
printf '%s %s\n' "${0##*/}" "$*" >> "$TFP_TEST_ROOT/packages.log"
[ "${FAIL_PACKAGES:-0}" != 1 ] || exit 1
if [ "${1:-}" = -q ]; then [ "${3:-}" = python3.11 ]; exit; fi
touch "$TFP_TEST_ROOT/dependencies-installed"
'''


class InstallerCompatibilityTests(InstallerSandbox):
    def test_managed_profiles_install_and_explicit_legacy_migration(self):
        source = self.root / "local-profiles.json"
        content = '{"local":{"allowed_listen":["0.0.0.0:9443"]}}'
        source.write_text(content, encoding="utf-8")
        for distro_id, related in (("debian", ""), ("ubuntu", "debian"), ("rocky", "rhel"), ("arch", "")):
            with self.subTest(distro=distro_id):
                self.distro(distro_id, related)
                self.success(self.run_script(args=AGENT_ARGS + ["-M", "-F", shell_path(source)]))
                installed = self.root / "etc/tfp-agent/service-profiles.json"
                self.assertEqual(installed.read_text(encoding="utf-8"), content)
                service = (self.root / "etc/systemd/system/tfp-agent.service").read_text(encoding="utf-8")
                self.assertIn("-service-profiles ", service)
                calls = (self.root / "systemctl.log").read_text(encoding="utf-8")
                self.assertIn("disable --now tfp-exit.service", calls)

    def test_managed_profiles_reject_implicit_migration_and_missing_file(self):
        source = self.root / "local-profiles.json"
        source.write_text("{}", encoding="utf-8")
        result = self.run_script(args=AGENT_ARGS + ["-F", shell_path(source)])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("旧出口服务仍占用端口", result.stderr)
        self.assertFalse((self.root / "usr/local/bin/tfp-agent").exists())
        result = self.run_script(args=AGENT_ARGS + ["-M"])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("-M 需要同时指定 -F", result.stderr)
        result = self.run_script(args=AGENT_ARGS + ["-F", shell_path(self.root / "missing.json")])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("无法读取本地 service profile", result.stderr)

    def distro(self, distro_id, related=""):
        (self.root / "etc/os-release").write_text(
            f'ID={distro_id}\nID_LIKE="{related}"\nPRETTY_NAME="{distro_id} fixture"\n',
            encoding="utf-8",
        )

    def packages(self):
        return (self.root / "packages.log").read_text(encoding="utf-8")

    def test_distro_families_install_ca_and_start_agent(self):
        for manager in ("apt-get", "dnf", "yum", "pacman", "zypper"):
            self.mock(manager, PACKAGE_MOCK)
        for distro_id, related, expected in (
            ("debian", "", "apt-get install -y"),
            ("ubuntu", "debian", "apt-get install -y"),
            ("linuxmint", "ubuntu debian", "apt-get install -y"),
            ("rhel", "fedora", "dnf install -y"),
            ("rocky", "rhel centos fedora", "dnf install -y"),
            ("almalinux", "rhel centos fedora", "dnf install -y"),
            ("centos", "rhel fedora", "dnf install -y"),
            ("fedora", "", "dnf install -y"),
            ("arch", "", "pacman -S --needed --noconfirm"),
            ("manjaro", "arch", "pacman -S --needed --noconfirm"),
        ):
            with self.subTest(distro=distro_id):
                self.distro(distro_id, related)
                (self.root / "packages.log").unlink(missing_ok=True)
                result = self.run_script(args=AGENT_ARGS, env={"SSL_CERT_FILE": "", "CURL_CA_BUNDLE": ""})
                self.success(result)
                self.assertIn(expected + " ca-certificates", self.packages())
                self.assertNotIn(" -Sy ", self.packages())
                self.assertNotIn("-Syu", self.packages())
                service = (self.root / "etc/systemd/system/tfp-agent.service").read_text(encoding="utf-8")
                self.assertIn("-name fixture-node", service)
                self.assertNotIn("fixture-key", service)
                self.assertIn("接入完成", result.stdout)

    def test_yum_fallback_without_dnf(self):
        self.distro("centos", "rhel")
        self.mock("yum", PACKAGE_MOCK)
        prelude = 'command() { if [ "$*" = "-v dnf" ]; then return 1; fi; builtin command "$@"; }\n'
        self.success(self.run_script(args=AGENT_ARGS, env={"SSL_CERT_FILE": "", "CURL_CA_BUNDLE": ""}, prelude=prelude))
        self.assertIn("yum install -y ca-certificates", self.packages())

    def test_missing_download_and_core_tools_are_installed(self):
        self.mock("apt-get", PACKAGE_MOCK)
        prelude = r'''
command() {
  if [ "${1:-}" = -v ] && [ ! -f "$TFP_TEST_ROOT/dependencies-installed" ]; then
    case "$2" in curl|grep|od) return 1 ;; esac
  fi
  builtin command "$@"
}
'''
        self.success(self.run_script(args=AGENT_ARGS, prelude=prelude))
        self.assertIn("apt-get install -y curl coreutils grep", self.packages())

    def test_complete_agent_install_does_not_need_python_or_packages(self):
        for name in ("python3", "apt-get", "dnf", "yum", "pacman"):
            self.mock(name, "exit 95")
        self.success(self.run_script(args=AGENT_ARGS))
        self.assertFalse((self.root / "packages.log").exists())
        self.assertFalse((self.root / "pip.log").exists())

    def test_provided_certificate_and_native_udp_on_each_family(self):
        cert, key = self.root / "cert.pem", self.root / "key.pem"
        cert.write_text("fixture", encoding="utf-8")
        key.write_text("fixture", encoding="utf-8")
        self.mock("python3", "exit 95")
        args = AGENT_ARGS + ["-m", "exit", "-S", "exit.example.com", "-e", "fixture-exit-token", "-C", shell_path(cert), "-K", shell_path(key), "-D", "[::]:9443"]
        for distro_id, related in (("debian", ""), ("ubuntu", "debian"), ("rocky", "rhel"), ("arch", "")):
            with self.subTest(distro=distro_id):
                self.distro(distro_id, related)
                self.success(self.run_script(args=args))
                service = (self.root / "etc/systemd/system/tfp-exit.service").read_text(encoding="utf-8")
                self.assertIn("-udp-listen '[::]:9443'", service)
                self.assertNotIn("fixture-exit-token", service)
        self.assertFalse((self.root / "pip.log").exists())

    def test_no_running_systemd_fails_before_installing(self):
        result = self.run_script(args=AGENT_ARGS, env={"NO_SYSTEMD": "1"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("systemd 未运行", result.stderr)
        self.assertFalse((self.root / "usr/local/bin/tfp-agent").exists())
        self.assertFalse((self.root / "curl.log").exists())

    def test_exit_installs_openssl_before_certificate_validation(self):
        self.mock("apt-get", PACKAGE_MOCK)
        cert, key = self.root / "cert.pem", self.root / "key.pem"
        cert.write_text("fixture", encoding="utf-8")
        key.write_text("fixture", encoding="utf-8")
        prelude = r'''
command() {
  if [ "$*" = '-v openssl' ] && [ ! -f "$TFP_TEST_ROOT/dependencies-installed" ]; then return 1; fi
  builtin command "$@"
}
'''
        args = AGENT_ARGS + ["-m", "exit", "-S", "exit.example.com", "-e", "fixture-exit-token", "-C", shell_path(cert), "-K", shell_path(key)]
        self.success(self.run_script(args=args, prelude=prelude))
        self.assertIn("apt-get install -y openssl", self.packages())
        self.assertIn("-checkhost exit.example.com", (self.root / "openssl.log").read_text(encoding="utf-8"))

    def test_arch_package_error_is_actionable(self):
        self.distro("arch")
        self.mock("pacman", PACKAGE_MOCK)
        result = self.run_script(args=AGENT_ARGS, env={"SSL_CERT_FILE": "", "CURL_CA_BUNDLE": "", "FAIL_PACKAGES": "1"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("pacman -Syu", result.stderr)
        self.assertFalse((self.root / "usr/local/bin/tfp-agent").exists())

    def test_kernel_architectures_and_invalid_override(self):
        for machine, arch in (("x86_64", "amd64"), ("aarch64", "arm64"), ("armv7l", "arm"), ("i686", "386")):
            with self.subTest(machine=machine):
                (self.root / "curl.log").unlink(missing_ok=True)
                self.success(self.run_script(args=AGENT_ARGS, env={"FAKE_ARCH": machine}))
                self.assertIn("/download/agent/linux/" + arch, (self.root / "curl.log").read_text(encoding="utf-8"))
        for extra, env in ((("-a", "riscv64"), {}), ((), {"FAKE_ARCH": "unknown"})):
            result = self.run_script(args=AGENT_ARGS, extra=extra, env=env)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("架构", result.stderr)

    def test_selinux_labels_are_restored_before_agent_start(self):
        self.distro("rhel", "fedora")
        self.success(self.run_script(args=AGENT_ARGS))
        labels = (self.root / "restorecon.log").read_text(encoding="utf-8")
        for path in ("usr/local/bin/tfp-agent", "etc/tfp-agent/agent.env", "etc/systemd/system/tfp-agent.service", "var/lib/tfp-agent"):
            self.assertIn(path, labels)
        self.mock("restorecon", "exit 1")
        (self.root / "systemctl.log").unlink()
        result = self.run_script(args=AGENT_ARGS)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SELinux", result.stderr)
        self.assertNotIn("restart", (self.root / "systemctl.log").read_text(encoding="utf-8"))

    def test_enforcing_selinux_installs_missing_label_tools(self):
        self.distro("rocky", "rhel")
        self.mock("dnf", PACKAGE_MOCK)
        self.mock("getenforce", "echo Enforcing")
        prelude = r'''
command() {
  if [ "$*" = '-v restorecon' ] && [ ! -f "$TFP_TEST_ROOT/dependencies-installed" ]; then return 1; fi
  builtin command "$@"
}
'''
        self.success(self.run_script(args=AGENT_ARGS, prelude=prelude))
        self.assertIn("dnf install -y policycoreutils", self.packages())

    def test_private_ca_is_used_for_panel_probe_and_all_downloads(self):
        self.success(self.run_script(args=AGENT_ARGS, extra=("-c", "https://ca.example.com/root.pem"), env={"PRIVATE_CA": "1"}))
        calls = (self.root / "curl.log").read_text(encoding="utf-8").splitlines()
        panel_calls = [line for line in calls if "https://panel.example.com" in line]
        self.assertEqual(len(panel_calls), 3)
        self.assertTrue(all("--cacert" in line for line in panel_calls))
        self.assertTrue((self.root / "etc/tfp-agent/ca.pem").exists())

    def test_red_hat_legacy_python_uses_versioned_repository_package(self):
        modern_python = (self.bin / "python3").read_text(encoding="utf-8")
        for manager in ("dnf", "yum"):
            with self.subTest(manager=manager):
                self.distro("rocky", "rhel")
                self.mock(manager, PACKAGE_MOCK)
                self.mock("python3.11", modern_python)
                self.mock("python3", "exit 1")
                (self.root / "dependencies-installed").unlink(missing_ok=True)
                (self.root / "packages.log").unlink(missing_ok=True)
                prelude = r'''
command() {
  if [ "${1:-}" = -v ]; then
    case "$2" in
      python3.11) [ -f "$TFP_TEST_ROOT/dependencies-installed" ] || return 1 ;;
      python3.10|python3.12|python3.13|python3.14) return 1 ;;
    esac
    if [ "$2" = dnf ] && [ "$TFP_TEST_MANAGER" = yum ]; then return 1; fi
  fi
  builtin command "$@"
}
'''
                self.success(self.run_script(env={"OLD_CERTBOT": "1", "TFP_TEST_MANAGER": manager}, prelude=prelude))
                self.assertIn(manager + " install -y python3.11 python3.11-pip openssl", self.packages())
                self.assertIn("--ip-address 8.8.8.8", (self.root / "certbot.log").read_text(encoding="utf-8"))

    def test_arch_python_packages_and_debian_venv_repair(self):
        modern_python = (self.bin / "python3").read_text(encoding="utf-8")
        for distro_id, related, manager, expected in (
            ("arch", "", "pacman", "python python-pip openssl"),
            ("debian", "", "apt-get", "python3 python3-venv openssl"),
        ):
            with self.subTest(distro=distro_id):
                self.distro(distro_id, related)
                self.mock(manager, PACKAGE_MOCK)
                (self.root / "dependencies-installed").unlink(missing_ok=True)
                (self.root / "packages.log").unlink(missing_ok=True)
                shutil.rmtree(self.root / "etc/tfp-agent/certbot", ignore_errors=True)
                prefix = r'''
if [ ! -f "$TFP_TEST_ROOT/dependencies-installed" ]; then
  if [ "${1:-}" = -m ] && [ "${2:-}" = venv ]; then exit 1; fi
  if [ "$TFP_TEST_DISTRO" = arch ] && [ "${1:-}" = -c ]; then exit 1; fi
fi
'''
                self.mock("python3", prefix + modern_python)
                prelude = 'command() { if [ "${1:-}" = -v ]; then case "$2" in python3.*) return 1 ;; esac; fi; builtin command "$@"; }\n'
                self.success(self.run_script(env={"OLD_CERTBOT": "1", "TFP_TEST_DISTRO": distro_id}, prelude=prelude))
                self.assertIn(expected, self.packages())

    def test_unsupported_python_fails_before_acme_or_agent_install(self):
        self.distro("debian")
        self.mock("apt-get", PACKAGE_MOCK)
        self.mock("python3", "exit 1")
        prelude = 'command() { if [ "${1:-}" = -v ]; then case "$2" in python3.*) return 1 ;; esac; fi; builtin command "$@"; }\n'
        result = self.run_script(prelude=prelude)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Python 3.10", result.stderr)
        self.assertFalse((self.root / "certbot.log").exists())
        self.assertFalse((self.root / "usr/local/bin/tfp-agent").exists())


if __name__ == "__main__":
    unittest.main()
