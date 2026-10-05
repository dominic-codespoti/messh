"""Build all supported targets and validate the complete release asset set."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import tarfile
import time
import zipfile

from identity import TARGETS, identity, require, validate_identity


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def asset_names(value, goos, goarch):
    root = f"messh-{value['version']}-{goos}-{goarch}"
    suffix = ".zip" if goos == "windows" else ".tar.gz"
    executable = "messh.exe" if goos == "windows" else "messh"
    return root + suffix, root + "/" + executable


def verify_archive(path, executable):
    root = executable.split("/")[0]
    seen = set()
    executable_found = False

    def member(name, regular, directory, size, mode):
        nonlocal executable_found
        normalized = name.rstrip("/")
        parts = PurePosixPath(normalized).parts
        require(normalized not in seen, f"Duplicate archive member: {name}")
        seen.add(normalized)
        require(parts and parts[0] == root and all(p not in (".", "..") for p in parts)
                and not name.startswith("/") and "\\" not in name,
                f"Unsafe archive member: {name}")
        require(regular or directory, f"Non-regular archive member: {name}")
        if normalized == executable:
            require(regular and size > 0, "Archive executable must be a nonempty file")
            if not executable.endswith(".exe"):
                require(mode & 0o111, "Archive executable is not executable")
            executable_found = True

    if path.name.endswith(".zip"):
        with zipfile.ZipFile(path) as archive:
            for entry in archive.infolist():
                mode = entry.external_attr >> 16
                kind = mode & 0o170000
                member(entry.filename, not entry.is_dir() and kind in (0, 0o100000),
                       entry.is_dir() and kind in (0, 0o040000), entry.file_size, mode)
            require(archive.testzip() is None, "Archive contains corrupt data")
    else:
        with tarfile.open(path, "r:gz") as archive:
            for entry in archive:
                member(entry.name, entry.isfile(), entry.isdir(), entry.size, entry.mode)
    require(executable_found, "Archive is missing the manifest executable")


def verify_assets(dist, expected=None):
    value = json.loads((dist / "update.json").read_text(encoding="utf-8"))
    require(type(value.get("schema")) is int and value["schema"] == 1,
            "Unsupported update manifest schema")
    validate_identity(value)
    if expected is not None:
        require(all(value.get(key) == item for key, item in expected.items()),
                "Manifest does not describe this workflow build")
    require(isinstance(value.get("assets"), list) and len(value["assets"]) == len(TARGETS),
            "Manifest must describe all three supported targets")
    expected_files = {"update.json", "SHA256SUMS"}
    targets = set()
    for asset in value["assets"]:
        target = (asset.get("os"), asset.get("arch"))
        require(target in TARGETS and target not in targets, "Invalid or duplicate target")
        targets.add(target)
        name, executable = asset_names(value, *target)
        require(asset.get("name") == name and asset.get("executable") == executable,
                "Manifest archive name or executable does not match the build")
        path = dist / name
        require(path.is_file() and not path.is_symlink(), f"Missing archive: {name}")
        require(type(asset.get("size")) is int and asset["size"] > 0
                and path.stat().st_size == asset["size"], f"Archive size mismatch: {name}")
        require(asset.get("sha256") == digest(path), f"Archive hash mismatch: {name}")
        verify_archive(path, executable)
        expected_files.add(name)
    require({path.name for path in dist.iterdir()} == expected_files,
            "Release asset directory contains missing or unexpected files")
    checksums = {}
    for line in (dist / "SHA256SUMS").read_text(encoding="ascii").splitlines():
        checksum, separator, name = line.partition("  ")
        require(separator and name not in checksums, "Invalid or duplicate checksum entry")
        checksums[name] = checksum
    require(set(checksums) == expected_files - {"SHA256SUMS"},
            "Checksums must cover exactly the manifest and all archives")
    for name, checksum in checksums.items():
        require(digest(dist / name) == checksum, f"Checksum mismatch: {name}")
    return value


def archive_tree(stage, destination, epoch):
    paths = [stage, *sorted(stage.rglob("*"))]
    if destination.name.endswith(".zip"):
        stamp = time.gmtime(max(epoch, 315532800))[:6]
        with zipfile.ZipFile(destination, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            for path in paths:
                name = path.relative_to(stage.parent).as_posix()
                if path.is_dir():
                    name += "/"
                entry = zipfile.ZipInfo(name, stamp)
                entry.create_system = 3
                entry.external_attr = (0o040755 if path.is_dir() else 0o100644) << 16
                entry.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(entry, b"" if path.is_dir() else path.read_bytes())
    else:
        with destination.open("wb") as raw:
            with gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=epoch) as compressed:
                with tarfile.open(fileobj=compressed, mode="w") as archive:
                    for path in paths:
                        entry = archive.gettarinfo(str(path), path.relative_to(stage.parent).as_posix())
                        entry.uid = entry.gid = 0
                        entry.uname = entry.gname = ""
                        entry.mtime = epoch
                        entry.mode = 0o755 if entry.isdir() or path == stage / "messh" or entry.mode & 0o111 else 0o644
                        if entry.isfile():
                            with path.open("rb") as source:
                                archive.addfile(entry, source)
                        else:
                            archive.addfile(entry)


def build(dist, staging):
    value = identity(os.environ)
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    require(actual == value["commit"], "Checkout differs from the workflow commit")
    epoch = int(subprocess.check_output(["git", "log", "-1", "--format=%ct"], text=True))
    dist.mkdir(parents=True, exist_ok=False)
    staging.mkdir(parents=True, exist_ok=False)
    assets = []
    flags = "-s -w " + " ".join(
        f"-X messh/internal/buildinfo.{key.title()}={value[key]}"
        for key in ("version", "commit", "channel", "build")
    )
    for goos, goarch in TARGETS:
        name, executable = asset_names(value, goos, goarch)
        stage = staging / executable.split("/")[0]
        stage.mkdir()
        output = staging / executable
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-ldflags=" + flags,
                        "-o", str(output), "./cmd/messh"], env=env, check=True)
        output.chmod(0o755)
        for source in ("README.md", "LICENSE"):
            shutil.copyfile(source, stage / source)
        destination = dist / name
        archive_tree(stage, destination, epoch)
        assets.append(dict(os=goos, arch=goarch, name=name, size=destination.stat().st_size,
                           sha256=digest(destination), executable=executable))
    manifest = dict(schema=1, **value, assets=assets)
    (dist / "update.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    checksums = "".join(f"{digest(path)}  {path.name}\n" for path in sorted(dist.iterdir()))
    (dist / "SHA256SUMS").write_text(checksums, encoding="ascii")
    verify_assets(dist, value)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("build", "verify"))
    parser.add_argument("--dist", type=Path, default=Path("dist"))
    parser.add_argument("--staging", type=Path, default=Path(".release"))
    args = parser.parse_args()
    if args.command == "build":
        build(args.dist, args.staging)
    else:
        verify_assets(args.dist, identity(os.environ))
