"""Materialize the declared Linux source tree for the local toolchain prototype."""
import json
from pathlib import Path
import shutil
import sys

spec=json.loads(Path(sys.argv[1]).read_text())
root=Path(sys.argv[2]);root.mkdir(parents=True,exist_ok=True)
for logical,source in spec['sources'].items():
    destination=root/logical
    destination.parent.mkdir(parents=True,exist_ok=True)
    shutil.copy2(source,destination)
