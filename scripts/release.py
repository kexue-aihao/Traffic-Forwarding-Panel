"""Build Linux release archives from a clean, committed checkout (Python 3.10+)."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[1]
MODULE = "github.com/kexue-aihao/Traffic-Forwarding-Panel"


def command(*args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, help="new output directory; defaults to .local/releases/vVERSION")
    args = parser.parse_args()
    version = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?", version):
        parser.error("VERSION must contain a release version without the v prefix")
    if command("git", "status", "--porcelain"):
        parser.error("commit all source changes before building release artifacts")
    package = json.loads((ROOT / "web/package.json").read_text(encoding="utf-8"))
    lock = json.loads((ROOT / "web/package-lock.json").read_text(encoding="utf-8"))
    if any(v != version for v in (package["version"], lock["version"], lock["packages"][""]["version"])):
        parser.error("VERSION and web package versions must match")
    commit = command("git", "rev-parse", "HEAD")
    epoch = int(command("git", "show", "-s", "--format=%ct", "HEAD"))
    output = (args.output or ROOT / ".local/releases" / ("v" + version)).resolve()
    output.mkdir(parents=True, exist_ok=False)
    docs = command("git", "ls-files", "README.md", "docs", "examples", "internal/openapi/openapi.json").splitlines()
    toolchain = command("go", "version")
    checksums = []
    for arch, elf_machine in (("amd64", 62), ("arm64", 183)):
        name = f"traffic-forwarding-panel_{version}_linux_{arch}"
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
        with tempfile.TemporaryDirectory(prefix="tfp-release-") as temp:
            work = Path(temp)
            binaries = {}
            for app, symbol in (("panel", "main.Version"), ("agent", MODULE + "/internal/agent.Version")):
                binary = work / app
                subprocess.run([
                    "go", "build", "-trimpath", "-buildvcs=true",
                    "-ldflags", f"-s -w -X {symbol}={version}",
                    "-o", str(binary), "./cmd/" + app,
                ], cwd=ROOT, env=env, check=True)
                with binary.open("rb") as f:
                    header = f.read(20)
                if header[:4] != b"\x7fELF" or header[4:6] != b"\x02\x01" or int.from_bytes(header[18:20], "little") != elf_machine:
                    raise RuntimeError(f"unexpected binary architecture: {app}/{arch}")
                build_info = command("go", "version", "-m", str(binary))
                if f"vcs.revision={commit}" not in build_info or "vcs.modified=false" not in build_info:
                    raise RuntimeError("binary VCS metadata does not match the clean release commit")
                binaries[app] = binary
            info = json.dumps({
                "version": version, "commit": commit, "target": f"linux/{arch}",
                "toolchain": toolchain, "commit_timestamp": epoch, "cgo_enabled": False,
            }, indent=2).encode("utf-8") + b"\n"
            archive = output / (name + ".tar.gz")
            with archive.open("xb") as raw, gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=epoch) as gz, tarfile.open(fileobj=gz, mode="w") as tar:
                def add(relative, data, mode):
                    entry = tarfile.TarInfo(name + "/" + relative)
                    entry.size, entry.mode, entry.mtime = len(data), mode, epoch
                    tar.addfile(entry, io.BytesIO(data))

                for app, path in binaries.items():
                    add(app, path.read_bytes(), 0o755)
                for path in docs:
                    add(path, (ROOT / path).read_bytes(), 0o644)
                add("BUILDINFO.json", info, 0o644)
            with tarfile.open(archive) as tar:
                for app in binaries:
                    entry = tar.getmember(name + "/" + app)
                    if entry.mode != 0o755:
                        raise RuntimeError("release binary is not executable")
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            checksums.append(f"{digest}  {archive.name}\n")
            print(f"Built {archive.name} ({archive.stat().st_size} bytes)", flush=True)
    (output / "SHA256SUMS").write_text("".join(checksums), encoding="utf-8", newline="\n")
    print(f"Release commit: {commit}\nOutput: {output}", flush=True)


if __name__ == "__main__":
    main()
