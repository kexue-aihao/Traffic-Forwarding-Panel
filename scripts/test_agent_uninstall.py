"""Real systemd install -> Agent claim -> uninstall -> retry -> panel acknowledgement.

Run as root ONLY on an empty, disposable Linux CI runner. Refuses existing TFP
paths and removes only the paths it created. Binaries are supplied as arguments.
"""
import http.cookiejar
import http.server
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request


def command(*args, **kwargs):
    return subprocess.run(args, check=True, capture_output=True, **kwargs)


def eventually(check, seconds=75):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.25)
    raise AssertionError("timed out waiting for real Agent operation")


def main():
    assert sys.platform == "linux" and os.geteuid() == 0
    assert os.environ.get("TFP_DISPOSABLE_SYSTEMD_TEST") == "1"
    panel_binary, agent_binary = map(lambda v: str(Path(v).resolve()), sys.argv[1:])
    paths = [Path(p) for p in (
        "/usr/local/bin/tfp-agent", "/var/lib/tfp-agent", "/etc/tfp-agent",
        "/var/lib/tfp-agent-uninstall", "/etc/systemd/system/tfp-agent.service",
        "/etc/systemd/system/tfp-exit.service", "/etc/systemd/system/tfp-agent-uninstall.service",
    )]
    assert all(not p.exists() and not p.is_symlink() for p in paths), "existing installation; refusing test"
    panel = None
    proxy = None
    with tempfile.TemporaryDirectory(prefix="tfp-uninstall-test-") as tmp:
        work = Path(tmp)
        shutil.copyfile(agent_binary, work / "agent-linux-amd64")
        password = "disposable-systemd-test-password"
        command(panel_binary, "-dsn", str(work / "panel.db"), "-init-admin", "admin", input=(password + "\n").encode())
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            backend = "http://127.0.0.1:" + str(sock.getsockname()[1])
        rejected_result = threading.Event()
        allow_result = threading.Event()

        class Proxy(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                self.forward()

            def do_POST(self):
                self.forward()

            def forward(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                if self.path == "/api/v1/agent/control/result" and not allow_result.is_set():
                    rejected_result.set()
                    self.send_response(503)
                    self.end_headers()
                    return
                request = urllib.request.Request(backend + self.path, data=body if self.command == "POST" else None, method=self.command)
                for name in ("Content-Type", "Authorization", "Cookie", "X-Requested-With"):
                    if self.headers.get(name):
                        request.add_header(name, self.headers[name])
                try:
                    response = urllib.request.urlopen(request, timeout=20)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    payload = response.read()
                    self.send_response(response.status)
                    for name in ("Content-Type", "Set-Cookie"):
                        if response.headers.get(name):
                            self.send_header(name, response.headers[name])
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)

        try:
            proxy = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Proxy)
            threading.Thread(target=proxy.serve_forever, daemon=True).start()
            base = "http://127.0.0.1:" + str(proxy.server_port)
            log = (work / "panel.log").open("wb")
            panel = subprocess.Popen([panel_binary, "-dsn", str(work / "panel.db"), "-addr", backend.removeprefix("http://"), "-origin", base, "-agent-dir", tmp], stdout=log, stderr=log)
            opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

            def api(path, data=None):
                request = urllib.request.Request(base + "/api/v1" + path, data=json.dumps(data).encode() if data is not None else None)
                request.add_header("Content-Type", "application/json")
                request.add_header("X-Requested-With", "fetch")
                with opener.open(request, timeout=20) as response:
                    value = response.read()
                    return json.loads(value) if value else None

            eventually(lambda: api("/site"))
            api("/auth/login", {"username": "admin", "password": password})
            group = api("/groups", {"name": "disposable", "port_min": 22000, "port_max": 23000, "multiplier": "1"})
            enrollment = api("/nodes/enrollment", {"name": "systemd-uninstall", "group_ids": [group["id"]]})
            installer = Path(__file__).resolve().parents[1] / "internal/agentdist/agent-install.sh"
            command("bash", str(installer), "-u", base, "-t", enrollment["token"])
            sentinel = Path("/etc/tfp-agent/user-certificate.pem")
            sentinel.write_text("must survive managed uninstall")

            def ready_node():
                nodes = api("/nodes")["items"]
                return nodes[0] if nodes and {"shell-v1", "uninstall-v1"}.issubset(nodes[0].get("capabilities", [])) else None

            node = eventually(ready_node)
            probes = eventually(lambda: api("/probes")["items"])
            assert probes[0]["upload_total"] is not None
            access = api("/nodes/" + node["id"] + "/operation-access", {"password": password})
            operation = api("/nodes/" + node["id"] + "/uninstall", {"access_token": access["token"], "idempotency_key": "systemd-uninstall-test"})
            eventually(rejected_result.is_set)
            assert api("/nodes")["total"] == 1, "panel hid node without an acknowledgement"
            assert not Path("/usr/local/bin/tfp-agent").exists(), "worker did not remove binary"
            assert not Path("/etc/systemd/system/tfp-agent.service").exists()
            assert subprocess.run(["systemctl", "is-active", "--quiet", "tfp-agent.service"]).returncode != 0
            assert sentinel.read_text() == "must survive managed uninstall"
            allow_result.set()
            eventually(lambda: api("/nodes")["total"] == 0)
            eventually(lambda: not Path("/var/lib/tfp-agent-uninstall").exists())
            operations = api("/nodes/" + node["id"] + "/operations")["items"]
            assert operations[0]["id"] == operation["id"] and operations[0]["status"] == "succeeded"
            assert api("/probes")["items"] == []
            print("PASS: actual systemd Agent install, capabilities, totals, uninstall, failed acknowledgement retry, retained user certificate, panel removal", flush=True)
        except Exception:
            subprocess.run(["journalctl", "-u", "tfp-agent", "-u", "tfp-agent-uninstall", "--no-pager", "-n", "80"])
            raise
        finally:
            for unit in ("tfp-agent.service", "tfp-exit.service", "tfp-agent-uninstall.service"):
                subprocess.run(["systemctl", "disable", "--now", unit], capture_output=True)
            if panel:
                panel.terminate()
                panel.wait(timeout=15)
            if proxy:
                proxy.shutdown()
                proxy.server_close()
            for path in paths:
                if path.is_dir() and not path.is_symlink():
                    shutil.rmtree(path)
                else:
                    path.unlink(missing_ok=True)
            command("systemctl", "daemon-reload")


if __name__ == "__main__":
    main()
