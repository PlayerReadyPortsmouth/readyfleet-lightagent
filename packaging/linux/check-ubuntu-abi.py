#!/usr/bin/env python3
"""Reject prebuilt payloads requiring a newer libc than Ubuntu 24.04 provides."""
import pathlib
import re
import subprocess
import sys


def check(root: pathlib.Path) -> None:
    baseline = (2, 39)
    for relative in ('readyfleet-light', 'readyfleet-light-installer', 'engine/sunshine', 'engine/solarbeam-bridge'):
        with root.joinpath(relative).open('rb') as stream:
            header = stream.read(20)
        if header[:6] != b'\x7fELF\x02\x01' or header[18:20] != b'\x3e\x00':
            raise ValueError(f'{relative}: expected prebuilt x86-64 ELF executable')
    for path in sorted(root.rglob('*')):
        if not path.is_file() or path.is_symlink():
            continue
        with path.open('rb') as stream:
            if stream.read(4) != b'\x7fELF':
                continue
        result = subprocess.run(['readelf', '--version-info', str(path)], check=True, text=True, capture_output=True)
        versions = [tuple(map(int, match.split('.'))) for match in re.findall(r'\bGLIBC_([0-9]+(?:\.[0-9]+)+)', result.stdout)]
        required = max(versions, default=(0,))
        if required > baseline:
            version = '.'.join(map(str, required))
            raise ValueError(f'{path.relative_to(root)} requires GLIBC_{version}; Ubuntu 24.04 supports at most GLIBC_2.39. Rebuild on Ubuntu 24.04; repackaging is insufficient.')
    print('Ubuntu 24.04 GLIBC baseline check passed (desktop capture still requires host validation).')


if __name__ == '__main__':
    try:
        if len(sys.argv) != 2:
            raise ValueError('usage: check-ubuntu-abi.py EXTRACTED_BUNDLE_DIRECTORY')
        check(pathlib.Path(sys.argv[1]))
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
