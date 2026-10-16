"""Copy declared sources and bind script shebangs to declared interpreters."""
import json
from pathlib import Path
import shlex
import shutil
import sys

manifest = json.loads(Path(sys.argv[1]).read_text())
root = Path(sys.argv[2])
root.mkdir(parents=True, exist_ok=True)
commands = manifest['commands']
for logical, source in manifest['sources'].items():
    destination = root / logical
    destination.parent.mkdir(parents=True, exist_ok=True)
    with open(source, 'rb') as stream:
        first = stream.readline(512)
    if not first.startswith(b'#!'):
        shutil.copy2(source, destination)
        continue
    fields = shlex.split(first[2:].decode().strip())
    name = Path(fields.pop(0)).name
    if name == 'env':
        if fields and fields[0] == '-S':
            fields.pop(0)
        name = fields.pop(0) if fields else ''
    interpreter = commands.get(name)
    with open(destination, 'wb') as output:
        if interpreter:
            # Linux resolves a relative shebang interpreter against the action
            # working directory. These are declared execroot-relative paths,
            # independent of sandbox directory names.
            line = '#!' + interpreter + (' ' + ' '.join(fields) if fields else '') + '\n'
            if len(line.encode()) > 255:
                raise ValueError(f'Shebang exceeds Linux limit: {logical}')
            output.write(line.encode())
            with open(source, 'rb') as stream:
                stream.readline()
                shutil.copyfileobj(stream, output)
        else:
            # Never fall through to an installed host interpreter. Optional
            # scripts remain usable once their runtime is explicitly supplied.
            output.write(('#!' + commands['sh'] + '\n' +
                          'printf "%s\\n" ' + shlex.quote(f'Undeclared interpreter {name!r} required by {logical}') +
                          ' >&2\nexit 127\n').encode())
    shutil.copystat(source, destination)
