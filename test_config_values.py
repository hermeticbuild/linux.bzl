"""Check example requests against the values produced by execution-time Kconfig."""
import argparse
import json
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('config', type=Path)
parser.add_argument('expected', type=json.loads)
parser.add_argument('--unwritten', action='append', default=[])
parser.add_argument('--type', action='append', default=[])
parser.add_argument('--compiler-suffix')
parser.add_argument('--make-string', action='append', default=[])
args = parser.parse_args()
report = json.loads((args.config / 'config-report.json').read_text())
values = report['values']
for name, expected in args.expected.items():
    actual = values.get(name)
    assert actual == expected, f'{name}: expected {expected!r}, resolved {actual!r}'
written = (args.config / '.config').read_text()
for name in args.unwritten:
    assert not any(line.startswith('CONFIG_' + name + '=') or line == '# CONFIG_' + name + ' is not set' for line in written.splitlines()), (name, written)
for item in args.type:
    name, expected = item.split('=', 1)
    assert report['symbols'][name]['type'] == expected, (name, report['symbols'][name], expected)
if args.compiler_suffix:
    assert report['compiler']['cc'].endswith(args.compiler_suffix), report['compiler']
payload = json.loads((args.config / 'values/report.json').read_text())
for item in args.make_string:
    name, representation = item.split('=', 1)
    value = values[name]
    assert report['symbols'][name]['type'] == 'string', report['symbols'][name]
    match representation:
        case 'quoted':
            expected = json.dumps(value)
        case 'raw':
            expected = value
        case _:
            raise ValueError(f'Unknown native Make string representation: {representation}')
    actual = payload['make_values']['CONFIG_' + name]
    assert actual == expected, (name, actual, expected)
