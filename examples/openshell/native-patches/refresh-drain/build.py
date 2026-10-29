#!/usr/bin/env python3
"""Prepare, test or build the pinned OpenShell refresh shutdown repair."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tarfile
import time
import urllib.request
import zipfile


HERE = Path(__file__).resolve().parent
PIN = json.loads((HERE / "source.json").read_text())
Z3 = json.loads((HERE / "z3.json").read_text())


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def tree_digest(root):
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root).as_posix()
        if path.is_symlink():
            record = [relative, "symlink", os.readlink(path)]
        elif path.is_file():
            record = [relative, "file", path.stat().st_mode & 0o777, sha256(path)]
        elif path.is_dir():
            continue
        else:
            raise ValueError(f"unsupported source entry: {relative}")
        digest.update(json.dumps(record, separators=(",", ":")).encode() + b"\n")
    return digest.hexdigest()


def write_receipt(path, value):
    with path.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def verify_patch():
    patch = HERE / "refresh-drain.patch"
    if sha256(patch) != PIN["patch_sha256"]:
        raise ValueError("patch differs from reviewed source.json")
    return patch


def clean_environment():
    environment = os.environ.copy()
    for name in list(environment):
        if name.startswith("GIT_") or name in {"RUSTFLAGS", "CARGO_ENCODED_RUSTFLAGS"}:
            del environment[name]
    environment.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                       GIT_CONFIG_NOSYSTEM="1")
    return environment


def z3_library(root, fetch=False):
    if platform.system().lower() + "-" + platform.machine().lower() != Z3["platform"]:
        raise ValueError("this pinned binary build recipe targets Apple Silicon macOS")
    library_dir = root / "z3-static"
    if library_dir.is_symlink():
        raise ValueError("Z3 directory must belong to this isolated build directory")
    archive = root / Z3["archive_name"]
    if fetch:
        if not archive.exists():
            urllib.request.urlretrieve(Z3["archive_url"], archive)
        if sha256(archive) != Z3["archive_sha256"]:
            raise ValueError("Z3 archive differs from its pinned official release")
        library_dir.mkdir(exist_ok=True)
        with zipfile.ZipFile(archive) as bundle:
            for member, filename in [(Z3["library_member"], "libz3.a"),
                                     (Z3["license_member"], "LICENSE.txt")]:
                destination = library_dir / filename
                if not destination.exists():
                    with bundle.open(member) as source, destination.open("xb") as output:
                        shutil.copyfileobj(source, output)
    library = library_dir / "libz3.a"
    if library.is_symlink() or not library.is_file() or sha256(library) != Z3["library_sha256"]:
        raise ValueError("pinned Z3 static library missing or changed; run fetch first")
    return library_dir


def prepare(root, archive_source):
    patch = verify_patch()
    root.mkdir(parents=True, exist_ok=False)
    archive = root / "source.tar.gz"
    if archive_source:
        shutil.copyfile(archive_source, archive)
    else:
        urllib.request.urlretrieve(PIN["archive_url"], archive)
    if sha256(archive) != PIN["archive_sha256"]:
        raise ValueError("source archive differs from pinned upstream commit")
    with tarfile.open(archive) as bundle:
        bundle.extractall(root, filter="data")
    source = root / ("OpenShell-" + PIN["commit"])
    if not source.is_dir() or any((p / ".git").exists() for p in [source, *source.parents]):
        raise ValueError("source archive has unexpected repository layout")
    environment = clean_environment()
    subprocess.run(["git", "apply", "--check", str(patch)], cwd=source, env=environment, check=True)
    subprocess.run(["git", "apply", str(patch)], cwd=source, env=environment, check=True)
    write_receipt(root / "prepared.json", {
        "schema_version": 1,
        "source_pin": PIN,
        "source_tree_sha256": tree_digest(source),
    })
    return source


def run(root, action):
    verify_patch()
    prepared = json.loads((root / "prepared.json").read_text())
    if prepared["source_pin"] != PIN:
        raise ValueError("prepared source pin differs from this patch bundle")
    source = root / ("OpenShell-" + PIN["commit"])
    if tree_digest(source) != prepared["source_tree_sha256"]:
        raise ValueError("prepared source changed; create a fresh build directory")
    target = root / "target"
    if target.is_symlink():
        raise ValueError("target must belong to this isolated build directory")
    library_dir = z3_library(root, fetch=action == "fetch")
    environment = clean_environment()
    environment.update({
        "CARGO_TARGET_DIR": str(target),
        "CARGO_BUILD_JOBS": "1",
        "CARGO_PROFILE_DEV_DEBUG": "0",
        "CARGO_PROFILE_TEST_DEBUG": "0",
        "OPENSHELL_GIT_VERSION": PIN["build_version"],
        "Z3_LIBRARY_PATH_OVERRIDE": str(library_dir),
        "Z3_SYS_Z3_VERSION": Z3["version"],
        "Z3_NO_PKG_CONFIG": "1",
    })
    command = ["cargo", "+" + PIN["rust_toolchain"]]
    if action == "fetch":
        command += ["fetch", "--locked"]
    elif action == "check":
        command += ["check", "--locked", "--offline", "-p", "openshell-server",
                    "--no-default-features", "--tests", "-j", "1"]
    elif action == "test":
        command += ["test", "--locked", "--offline", "-p", "openshell-server",
                    "--no-default-features", "--lib", "refresh_shutdown", "-j", "1",
                    "--", "--test-threads=1"]
    elif action == "regression":
        command += ["test", "--locked", "--offline", "-p", "openshell-server",
                    "--no-default-features", "--lib", "provider_refresh", "-j", "1",
                    "--", "--test-threads=1"]
    else:
        command += ["build", "--locked", "--offline", "-p", "openshell-gateway",
                    "--bin", "openshell-gateway", "--no-default-features",
                    "--features", "compute-driver-vm", "-j", "1"]
    stamp = str(time.time_ns())
    log = root / f"{action}-{stamp}.log"
    started = time.time()
    with log.open("x") as output:
        result = subprocess.run(command, cwd=source, env=environment,
                                stdout=output, stderr=subprocess.STDOUT)
    unchanged = tree_digest(source) == prepared["source_tree_sha256"]
    receipt = {
        "schema_version": 1, "action": action, "command": command,
        "exit_code": result.returncode, "source_unchanged": unchanged,
        "elapsed_seconds": round(time.time() - started, 3),
        "source_pin": PIN, "source_tree_sha256": prepared["source_tree_sha256"],
        "z3_pin": Z3,
        "log": str(log), "log_sha256": sha256(log),
    }
    if action == "build" and result.returncode == 0 and unchanged:
        binary = target / "debug" / "openshell-gateway"
        version_result = subprocess.run([str(binary), "--version"], env=environment,
                                        capture_output=True, text=True, timeout=10)
        version = version_result.stdout.strip()
        receipt.update(binary=str(binary), binary_sha256=sha256(binary), version=version)
        if version_result.returncode != 0 or PIN["build_version"] not in version:
            receipt["exit_code"] = 1
            receipt["error"] = "built gateway version does not identify this repair"
    destination = root / f"{action}-{stamp}.json"
    write_receipt(destination, receipt)
    print(destination)
    if receipt["exit_code"] != 0 or not unchanged:
        raise SystemExit("native operation failed; inspect its receipt and log")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["prepare", "fetch", "check", "test", "regression", "build"])
    parser.add_argument("--work-dir", required=True, type=Path)
    parser.add_argument("--archive", type=Path, help="prepare only: reuse the exact pinned archive")
    args = parser.parse_args()
    root = args.work_dir.absolute()
    if root.resolve() != root:
        parser.error("work-dir must be canonical and must not traverse symlinks")
    if args.archive and args.action != "prepare":
        parser.error("archive is only accepted for prepare")
    if args.action == "prepare":
        print(prepare(root, args.archive))
    else:
        run(root, args.action)


if __name__ == "__main__":
    main()
