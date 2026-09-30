#!/usr/bin/env python3
"""Extract only a bounded, regular Sessions app payload before signing keys exist."""
import os
from pathlib import Path, PurePosixPath
import stat
import sys
import zipfile


def unpack(archive, destination):
    root = Path(destination)
    if root.exists():
        raise ValueError("app extraction destination must not exist")
    with zipfile.ZipFile(archive) as bundle:
        if len(bundle.infolist()) > 10000:
            raise ValueError("app archive exceeds 10000 entries")
        names = set()
        total = 0
        for member in bundle.infolist():
            name = PurePosixPath(member.filename)
            if name.is_absolute() or ".." in name.parts or "\\" in member.filename or not name.parts or name.parts[0] != "Sessions.app":
                raise ValueError("app archive contains an unexpected path")
            if member.filename in names:
                raise ValueError("app archive contains a duplicate path")
            names.add(member.filename)
            mode = member.external_attr >> 16
            kind = stat.S_IFMT(mode)
            if kind not in (0, stat.S_IFDIR, stat.S_IFREG):
                raise ValueError("app archive contains a symlink or special file")
            total += member.file_size
            if total > 512 * 1024 * 1024:
                raise ValueError("app archive exceeds 512 MiB")
        root.mkdir(mode=0o700)
        for member in bundle.infolist():
            target = root.joinpath(*PurePosixPath(member.filename).parts)
            if member.is_dir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            with bundle.open(member) as source, target.open("xb") as output:
                while block := source.read(1024 * 1024):
                    output.write(block)
            os.chmod(target, 0o755 if (member.external_attr >> 16) & 0o111 else 0o644)


if __name__ == "__main__":
    try:
        if len(sys.argv) != 3:
            raise ValueError("usage: unpack-release-app.py ARCHIVE NEW_DESTINATION")
        unpack(sys.argv[1], sys.argv[2])
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        print(f"Cannot unpack release app: {error}", file=sys.stderr)
        sys.exit(1)
