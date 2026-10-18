#!/usr/bin/env python3
"""Export file-local Linux Kconfig syntax using the native grammar and lexer."""
import json
import os
from pathlib import Path
import subprocess
import sys


HERE = Path(__file__).resolve().parent


def export_syntax(linux):
    executable = Path(sys.executable)
    env = {'LC_ALL': 'C', 'PATH': ''}
    env.update(srctree=str(linux), SRCARCH='__KCONFIG_SRCARCH', ARCH='__KCONFIG_ARCH',
               SUBARCH='__KCONFIG_SUBARCH', HEADER_ARCH='__KCONFIG_HEADER_ARCH',
               KERNELVERSION='prototype', CC='__selected_cc__', LD='__selected_ld__',
               CC_VERSION_TEXT='__KCONFIG_CC_VERSION_TEXT', PAHOLE_VERSION='0')
    files, warnings = {}, set()
    for path in sorted(linux.rglob('Kconfig*')):
        relative = path.relative_to(linux).as_posix()
        if not path.is_file() or (relative.startswith('Documentation/') and relative != 'Documentation/Kconfig') or relative.startswith(('scripts/kconfig/', 'tools/')) or '.git' in path.parts:
            continue
        result = subprocess.run([executable, "_kconfig", relative], cwd=linux, env=env, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError(f'Native Kconfig syntax export failed for {relative}:\n{result.stderr}')
        warnings.update(line for line in result.stderr.splitlines() if line)
        files[relative] = json.loads(result.stdout)
    for warning in sorted(warnings):
        print(warning, file=sys.stderr)
    return files
