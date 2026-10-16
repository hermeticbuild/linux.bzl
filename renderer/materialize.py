#!/usr/bin/env python3
"""Arrange Bazel-declared generated prerequisites for compiler include lookup."""
import json
from pathlib import Path
import shutil
import sys

out = Path(sys.argv[2])
out.mkdir(parents=True, exist_ok=True)
for name, source in json.loads(Path(sys.argv[1]).read_text()).items():
    path = Path(name)
    if path.is_absolute() or '..' in path.parts:
        raise ValueError(name)
    dest = out / path
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, dest)
