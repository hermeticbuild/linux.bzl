"""Check example requests against the values produced by execution-time Kconfig."""
import json
from pathlib import Path
import sys

values = json.loads((Path(sys.argv[1]) / 'config-report.json').read_text())['values']
for name, expected in json.loads(sys.argv[2]).items():
    actual = values.get(name, 'n')
    assert actual == expected, f'{name}: expected {expected!r}, resolved {actual!r}'
