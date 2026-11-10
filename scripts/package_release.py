#!/usr/bin/env python3
"""Package and smoke-test the native executable; used by GitHub Actions."""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import zipfile


def run(*args, expected=0, env=None):
    result = subprocess.run(args, capture_output=True, text=True, env=env)
    if result.returncode != expected:
        raise RuntimeError(f"{args[0]} exited {result.returncode}: {result.stderr}")
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    version = parser.parse_args().version
    if version != "dev" and not re.fullmatch(r"v\d+\.\d+\.\d+", version):
        parser.error("version must be dev or vMAJOR.MINOR.PATCH")
    root = Path(__file__).resolve().parent.parent
    os.chdir(root)
    goos, goarch = run("go", "env", "GOOS", "GOARCH").split()
    if goos not in {"darwin", "linux", "windows"} or goarch not in {"amd64", "arm64"}:
        raise RuntimeError("unsupported release target")
    binary = "concierge-dialer.exe" if goos == "windows" else "concierge-dialer"
    commit = run("git", "rev-parse", "HEAD").strip()
    dirty = bool(run("git", "status", "--porcelain").strip())
    if version != "dev" and dirty:
        raise RuntimeError("release builds require a clean checkout")
    metadata = {"version": version, "commit": commit, "dirty": dirty, "goos": goos, "goarch": goarch}
    payload = {"bin/" + binary: (root / "bin" / binary).read_bytes()}
    for name in ["README.md", "LICENSE"]:
        payload[name] = (root / name).read_bytes()
    for path in sorted((root / "skills" / "concierge-dialer").rglob("*")):
        if path.is_symlink():
            raise RuntimeError(f"symlink in skill: {path}")
        if path.is_file():
            payload[path.relative_to(root).as_posix()] = path.read_bytes()
    go_root = Path(run("go", "env", "GOROOT").strip())
    go_license = next(path for path in [go_root / "LICENSE", go_root.parent / "LICENSE"] if path.is_file())
    notices = ["Go runtime and standard library\n\n" + go_license.read_text() + "\n"]
    dependencies = sorted(set(run("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}", "./cmd/concierge-dialer").split()))
    modules = run("go", "list", "-m", "-f", "{{.Path}}|{{.Version}}|{{.Dir}}", *dependencies)
    for line in modules.splitlines():
        if not line.strip():
            continue
        module, module_version, directory = line.split("|", 2)
        candidates = [Path(directory) / name for name in ["LICENSE", "LICENSE.txt", "LICENSE.md"]]
        license_path = next((path for path in candidates if path.is_file()), None)
        if not directory or license_path is None:
            raise RuntimeError(f"missing dependency license: {module}")
        notices.append(f"{module} {module_version}\n\n{license_path.read_text()}\n")
    payload["skills/concierge-dialer/THIRD_PARTY_LICENSES.txt"] = "\n---\n\n".join(notices).encode()
    payload["BUILD.json"] = (json.dumps(metadata, indent=2) + "\n").encode()
    name = f"bellhopi_{version}_{goos}_{goarch}"
    output = root / "dist"
    output.mkdir(exist_ok=True)
    archive = output / (name + (".zip" if goos == "windows" else ".tar.gz"))
    if archive.exists():
        raise RuntimeError(f"refusing to replace {archive}")
    if goos == "windows":
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as bundle:
            for relative, data in sorted(payload.items()):
                bundle.writestr(name + "/" + relative, data)
        with zipfile.ZipFile(archive) as bundle:
            unpacked = {item[len(name) + 1:]: bundle.read(item) for item in bundle.namelist()}
    else:
        with tarfile.open(archive, "w:gz") as bundle:
            for relative, data in sorted(payload.items()):
                entry = tarfile.TarInfo(name + "/" + relative)
                entry.size = len(data)
                entry.mode = 0o755 if relative.startswith("bin/") else 0o644
                bundle.addfile(entry, io.BytesIO(data))
        with tarfile.open(archive) as bundle:
            unpacked = {item.name[len(name) + 1:]: bundle.extractfile(item).read() for item in bundle.getmembers()}
    assert unpacked == payload, "archive contents differ from inputs"
    environment = dict(os.environ)
    for key in ["OPENAI_API_KEY", "TELNYX_API_KEY", "TELNYX_PUBLIC_KEY", "TELNYX_CONNECTION_ID", "TELNYX_FROM_NUMBER", "PUBLIC_BASE_URL", "CONCIERGE_API_TOKEN"]:
        environment.pop(key, None)
    with tempfile.TemporaryDirectory(prefix="bellhopi-package-") as temporary:
        temporary = Path(temporary)
        extracted = temporary / name
        for relative, data in unpacked.items():
            destination = extracted / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(data)
        executable = extracted / "bin" / binary
        executable.chmod(0o700)
        assert "commands" in json.loads(run(str(executable), "help", env=environment))
        home = temporary / "installed"
        run(str(executable), "install", "--target", "both", "--source", str(extracted / "skills" / "concierge-dialer"), "--home", str(home), env=environment)
        for client in [".agents", ".claude"]:
            skill = home / client / "skills" / "concierge-dialer"
            assert (skill / "bin" / binary).read_bytes() == payload["bin/" + binary]
            assert (skill / "LICENSE").read_bytes() == payload["LICENSE"]
            assert (skill / "THIRD_PARTY_LICENSES.txt").is_file()
            assert "commands" in json.loads(run(str(skill / "bin" / binary), "help", env=environment))
        config = temporary / "private" / "config.json"
        run(str(executable), "--config", str(config), "init", env=environment)
        doctor = json.loads(run(str(executable), "--config", str(config), "doctor", expected=1, env=environment))
        assert doctor["configured"] is False and doctor["live_verified"] is False
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    (output / (archive.name + ".sha256")).write_bytes(f"{digest}  {archive.name}\n".encode())
    print(json.dumps({**metadata, "archive": archive.name, "sha256": digest, "native_smoke": "passed", "provider_calls": 0}))


if __name__ == "__main__":
    main()
