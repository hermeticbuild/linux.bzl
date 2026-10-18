"""Execute one planned kernel action in an isolated overlay of declared inputs.

Directory intermediates are tar artifacts. This avoids the allowlisted subtree
API while preserving individual compiler, generator, archive and link actions.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile

spec=json.loads(Path(sys.argv[1]).read_text())
real=Path.cwd()
with tempfile.TemporaryDirectory(prefix='kernel-action-') as temporary:
    work=Path(temporary)
    directories=set(spec['directory_inputs'])
    for name in sorted(set(spec['inputs']),key=lambda p:(len(Path(p).parts),p)):
        path=Path(name)
        if path.is_absolute():continue
        dest=work/path
        if dest.exists() or dest.is_symlink():continue
        dest.parent.mkdir(parents=True,exist_ok=True)
        source=real/path
        if name in directories:
            dest.mkdir()
            with tarfile.open(source) as archive:archive.extractall(dest,filter='data')
        else:
            dest.symlink_to(source)
    for name in spec['outputs']:
        target=work/name
        target.parent.mkdir(parents=True,exist_ok=True)
    executable=spec['executable']
    if not executable.startswith('/'):executable=str(real/executable)
    env=dict(spec['environment'])
    subprocess.run([executable,*spec['arguments']],cwd=work,env=env,check=True)
    for name in spec['outputs']:
        source=work/name;dest=real/name
        dest.parent.mkdir(parents=True,exist_ok=True)
        if name in spec['directory_outputs']:
            with tarfile.open(dest,'w') as archive:
                for child in sorted(source.rglob('*')):
                    if child.is_file():archive.add(child,arcname=str(child.relative_to(source)))
        else:
            shutil.copy2(source,dest)
