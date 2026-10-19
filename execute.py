"""Execute one kernel action with explicit virtual-to-artifact input mappings."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile


def archive_metadata(info):
    """Transport contents and executable bits, independent of filesystem metadata."""
    info.uid = info.gid = info.mtime = 0
    info.uname = info.gname = ''
    info.mode = 0o755 if info.mode & 0o111 else 0o644
    info.pax_headers = {}
    return info


def execute(spec, scanner=None):
    real = Path.cwd()
    aliases = spec.get('aliases', {})
    if spec.get('common_inputs'):
        common = real / aliases.get(spec['common_inputs'], spec['common_inputs'])
        spec = dict(spec, inputs=spec['inputs'] + json.loads(common.read_text()))
    with tempfile.TemporaryDirectory(prefix='kernel-action-') as temporary:
        work = Path(temporary)
        directories = set(spec['directory_inputs'])
        for name in sorted(set(spec['inputs']), key=lambda path: (len(Path(path).parts), path)):
            path = Path(name)
            if path.is_absolute():
                continue
            dest = work / path
            if dest.exists() or dest.is_symlink():
                continue
            dest.parent.mkdir(parents=True, exist_ok=True)
            source = real / aliases.get(name, name)
            if name in directories:
                dest.mkdir()
                with tarfile.open(source) as archive:
                    archive.extractall(dest, filter='data')
            else:
                dest.symlink_to(source)
        for name in spec['outputs']:
            (work / name).parent.mkdir(parents=True, exist_ok=True)
        executable = spec['executable']
        if not executable.startswith('/'):
            executable = str(real / executable)
        arguments = scanner(spec, work) if scanner else spec['arguments']
        environment = dict(spec['environment'], KBUILD_EXEC_ROOT=str(work))
        environment.update({name: str(work / path) for name, path in spec.get('path_environment', {}).items()})
        subprocess.run([executable, *arguments], cwd=work, env=environment, check=True)
        if scanner:
            return scanner.dependencies(work)
        for name in spec['outputs']:
            source = work / name
            dest = real / spec.get('output_aliases', {}).get(name, name)
            dest.parent.mkdir(parents=True, exist_ok=True)
            if name in spec['directory_outputs']:
                with tarfile.open(dest, 'w') as archive:
                    for child in sorted(source.rglob('*')):
                        if child.is_file():
                            archive.add(child, arcname=str(child.relative_to(source)), filter=archive_metadata)
            else:
                shutil.copy2(source, dest)


if __name__ == '__main__':
    execute(json.loads(Path(sys.argv[1]).read_text()))
