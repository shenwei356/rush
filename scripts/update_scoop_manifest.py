#!/usr/bin/env python3
"""Update the Scoop manifest from already published Windows release archives."""

import gzip
import hashlib
import json
import pathlib
import re
import sys
import tarfile


ARCHIVES = {
    "32bit": "rush_windows_386.exe.tar.gz",
    "64bit": "rush_windows_amd64.exe.tar.gz",
    "arm64": "rush_windows_arm64.exe.tar.gz",
}


def version_parts(value):
    if not re.fullmatch(r"v?\d+\.\d+\.\d+", value):
        raise ValueError(f"expected a stable vMAJOR.MINOR.PATCH release, got {value!r}")
    return tuple(map(int, value.removeprefix("v").split(".")))


def update(tag, assets_dir, manifest_path):
    new_version = version_parts(tag)
    if not tag.startswith("v"):
        raise ValueError(f"release tag must start with v: {tag!r}")

    manifest = json.loads(manifest_path.read_text())
    if new_version <= version_parts(manifest["version"]):
        return False

    for arch, filename in ARCHIVES.items():
        archive_path = assets_dir / filename
        with gzip.open(archive_path, "rb") as payload:
            with tarfile.open(fileobj=payload, mode="r|") as archive:
                members = archive.getmembers()
                if len(members) != 1 or members[0].name != "rush.exe" or not members[0].isfile():
                    raise ValueError(f"{filename} must contain only rush.exe at the archive root")
                if members[0].size == 0:
                    raise ValueError(f"{filename} contains an empty rush.exe")
            # Tar ends before the gzip trailer. Read to EOF to verify the checksum and size.
            while payload.read(1024 * 1024):
                pass
        with archive_path.open("rb") as archive_file:
            digest = hashlib.file_digest(archive_file, "sha256").hexdigest()
        manifest["architecture"][arch]["url"] = (
            f"https://github.com/shenwei356/rush/releases/download/{tag}/{filename}"
        )
        manifest["architecture"][arch]["hash"] = digest

    manifest["version"] = tag.removeprefix("v")
    manifest_path.write_text(json.dumps(manifest, indent=4) + "\n")
    return True


if __name__ == "__main__":
    if len(sys.argv) not in (3, 4):
        raise SystemExit("usage: update_scoop_manifest.py TAG ASSETS_DIR [MANIFEST]")
    manifest_path = pathlib.Path(sys.argv[3]) if len(sys.argv) == 4 else pathlib.Path("bucket/rush.json")
    changed = update(sys.argv[1], pathlib.Path(sys.argv[2]), manifest_path)
    print("updated" if changed else "already up to date")
