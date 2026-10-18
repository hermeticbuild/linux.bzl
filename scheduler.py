"""Preprocess compiler dependencies, execute narrow-input jobs, and export results."""
import concurrent.futures
import json
import os
from pathlib import Path
import posixpath
import shlex
import shutil
import subprocess
import sys
import tempfile

from execute import execute


def publish(output, value):
    root = Path(output)
    root.mkdir(parents=True, exist_ok=True)
    text = json.dumps(value, ensure_ascii=True, separators=(',', ':')).encode().hex()
    for start in range(0, len(text), 180):
        (root / ('%08d=' % (start // 180) + text[start:start + 180])).touch()
    (root / 'report.json').write_text(json.dumps(value, indent=2))


class DependencyScan:
    def __init__(self, request):
        self.request = request
        self.depfile = None

    def __call__(self, spec, work):
        start = self.request['scan']['argument_start']
        self.depfile = work / '.compiler-dependencies.d'
        flags, skip = [], False
        for flag in spec['arguments'][start:]:
            if skip:
                skip = False
            elif flag in ('-o', '-MF', '-MT', '-MQ'):
                skip = True
            elif flag in ('-c', '-S', '-E', '-MD', '-MMD', '-M', '-MM', '-MP') or flag.startswith(('-Wp,-MD,', '-Wp,-MMD,')):
                continue
            else:
                flags.append(flag)
        return spec['arguments'][:start] + flags + ['-M', '-MF', str(self.depfile), '-MT', 'kernel_dependency_scan']

    def dependencies(self, work):
        text = self.depfile.read_text().replace('\\\n', '')
        if ':' not in text:
            raise ValueError('Compiler did not emit a valid dependency file')
        paths = shlex.split(text.split(':', 1)[1])
        prefixes = [self.request['source_prefix'], self.request['original_prefix']]
        logical = set()
        for path in paths:
            path = posixpath.normpath(path)
            if path.startswith(str(work) + '/'):
                path = path[len(str(work)) + 1:]
            for prefix in prefixes:
                needle = prefix.rstrip('/') + '/'
                if path.startswith(needle):
                    relative = path[len(needle):]
                    if '..' in Path(relative).parts or Path(relative).is_absolute():
                        raise ValueError('Invalid source dependency: ' + relative)
                    logical.add(relative)
        return sorted(logical)


def scan(request):
    spec = json.loads(Path(request['manifest']).read_text())
    spec['aliases'] = dict(request['aliases'])
    spec['aliases'][request['original_prefix']] = request['source_prefix']
    spec['inputs'] = spec['inputs'] + [request['original_prefix']]
    return request['id'], execute(spec, DependencyScan(request))


def run_job(mapping):
    spec = json.loads(Path(mapping['manifest']).read_text())
    spec['aliases'] = mapping['aliases']
    spec['inputs'] = list(mapping['aliases']) + mapping['source_inputs']
    spec['output_aliases'] = mapping['outputs']
    execute(spec)


def main(arguments):
    if arguments[0] == 'scan':
        requests = json.loads(Path(arguments[1]).read_text())
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            answers = dict(pool.map(scan, requests))
        publish(arguments[2], answers)
    elif arguments[0] == 'execute':
        run_job(json.loads(Path(arguments[1]).read_text()))
    elif arguments[0] == 'copy':
        dest = Path(arguments[2])
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(arguments[1], dest)
    else:
        raise ValueError('Unknown scheduler operation ' + arguments[0])


if __name__ == '__main__':
    main(sys.argv[1:])
