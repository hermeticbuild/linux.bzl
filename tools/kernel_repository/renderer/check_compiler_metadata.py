"""Validate advertised facts using Linux's unmodified version-query scripts."""
import json
from pathlib import Path
import subprocess
import sys

spec = json.loads(Path(sys.argv[1]).read_text())
root = Path(spec['source'])

def query(script, args):
    return subprocess.check_output([spec['shell'], str(root / 'scripts' / script), *args], text=True).strip()

actual = {
    'compiler': query('cc-version.sh', [spec['tools']['CC'], *spec['flags']]),
    'assembler': query('as-version.sh', [spec['tools']['CC'], *spec['flags'], *spec['assembler_flags']]),
    'linker': query('ld-version.sh', [spec['tools']['LD']]),
}
assert actual == spec['expected'], {'advertised': spec['expected'], 'native': actual}
include = subprocess.check_output([spec['tools']['CC'], *spec['flags'], '-print-file-name=include'], text=True).strip()
assert Path(include).resolve() == Path(spec['builtin_include']).resolve(), (include, spec['builtin_include'])
Path(sys.argv[2]).write_text(json.dumps({'native': actual, 'builtin_include': include, 'status': 'passed'}, indent=2))
