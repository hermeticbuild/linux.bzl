"""Shared capability queries for native export and analysis providers."""
import json
from pathlib import Path

HERE = Path(__file__).resolve().parent


def native_capabilities():
    lines = []
    for fact in json.loads((HERE / 'compiler_capabilities.json').read_text()):
        if fact.get('kbuild'):
            continue
        lines.append('if (!strcmp(name, %s) && argc == 1 && !strcmp(argv[0], %s)) return prototype_input(%s);' % tuple(json.dumps(fact[k]) for k in ('probe', 'argument', 'input')))
    return '\n'.join(lines)
