#!/usr/bin/env python3
"""Prepare pinned native sources or run offline, model-free contract controls."""
import argparse
import hashlib
import json
import os
import subprocess
import sys
import tarfile
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
PIN = json.loads((HERE / "source.json").read_text())


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def fingerprint(root):
    rows = []
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            raise ValueError("source contains symlink")
        if path.is_file():
            rows.append([path.relative_to(root).as_posix(), path.stat().st_mode & 0o777,
                         digest(path)])
    return hashlib.sha256(json.dumps(rows, separators=(",", ":")).encode()).hexdigest()


def save(path, value):
    with path.open("x") as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write("\n")


def prepare(root, archive):
    if digest(HERE / "noodle-owner.patch") != PIN["patch_sha256"]:
        raise ValueError("patch differs from source pin")
    root.mkdir(parents=True, exist_ok=False)
    target = root / "source.tar.gz"
    if archive:
        target.write_bytes(archive.read_bytes())
    else:
        urllib.request.urlretrieve(PIN["archive_url"], target)
    if digest(target) != PIN["archive_sha256"]:
        raise ValueError("upstream archive differs from source pin")
    prefix = "harness2-" + PIN["commit"]
    with tarfile.open(target) as bundle:
        seen = set()
        for member in bundle.getmembers():
            name = Path(member.name)
            if (name.is_absolute() or ".." in name.parts or not name.parts
                    or name.parts[0] != prefix or member.name in seen
                    or not (member.isfile() or member.isdir())):
                raise ValueError("upstream archive contains unsupported entry")
            seen.add(member.name)
        bundle.extractall(root, filter="data")
    source = root / prefix
    if digest(source / "LICENSE") != PIN["license_sha256"]:
        raise ValueError("upstream license differs")
    environment = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    environment.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                       GIT_CONFIG_NOSYSTEM="1")
    for args in [["--check"], []]:
        subprocess.run(["git", "apply", *args, str(HERE / "noodle-owner.patch")],
                       cwd=source, env=environment, check=True)
    save(root / "prepared.json", {"source_pin": PIN, "source_tree_sha256": fingerprint(source)})
    return source


def controls(root):
    prepared = json.loads((root / "prepared.json").read_text())
    source = root / ("harness2-" + PIN["commit"])
    if (prepared["source_pin"] != PIN or digest(HERE / "noodle-owner.patch") != PIN["patch_sha256"]
            or fingerprint(source) != prepared["source_tree_sha256"]):
        raise ValueError("prepared source or patch drifted")
    environment = {"PATH": os.defpath, "HOME": str(root), "PYTHONDONTWRITEBYTECODE": "1",
                   "PYTHONPATH": os.pathsep.join([str(source), str(HERE)]),
                   "HARNESS2_HOME": str(root / "unused-home")}
    command = [sys.executable, "-B", "-m", "unittest", "discover", "-s", str(HERE),
               "-p", "test_*.py", "-v"]
    stamp = time.time_ns()
    log = root / f"controls-{stamp}.log"
    started = time.monotonic()
    with log.open("x") as output:
        result = subprocess.run(command, cwd=root, env=environment,
                                stdout=output, stderr=subprocess.STDOUT)
    receipt = {"schema_version": 1, "action": "offline-native-controls", "source_pin": PIN,
               "source_tree_sha256": prepared["source_tree_sha256"],
               "source_unchanged": fingerprint(source) == prepared["source_tree_sha256"],
               "exit_code": result.returncode, "elapsed_seconds": time.monotonic() - started,
               "command": command, "log": str(log), "log_sha256": digest(log),
               "model_calls": 0, "vm_started": False, "formal_owner_bridge": "offline contract only; real-owner acceptance not performed",
               "integration_sources": {p.name: digest(p) for p in sorted(HERE.glob("*.py"))},
               "integration_fixtures": {str(p.relative_to(HERE)): digest(p)
                   for p in sorted((HERE / "fixtures").glob("*")) if p.is_file()}}
    destination = root / f"controls-{stamp}.json"
    save(destination, receipt)
    print(destination)
    if result.returncode or not receipt["source_unchanged"]:
        raise SystemExit("native controls failed; retained log and receipt")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["prepare", "test"])
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--archive", type=Path)
    args = parser.parse_args()
    if not (3, 12) <= sys.version_info[:2] < (3, 14):
        parser.error("pinned upstream requires Python 3.12 or 3.13")
    root = args.work_dir.absolute()
    if root.resolve() != root:
        parser.error("work-dir traverses a symlink")
    if args.action == "prepare":
        print(prepare(root, args.archive))
    elif args.archive:
        parser.error("archive is prepare-only")
    else:
        controls(root)


if __name__ == "__main__":
    main()
