"""Write symbolic kernel input data for the measured runtime."""
from catalog import catalog
import json
from syntax import runtime_program


def write_inputs(output, tree, linux, program, link_script):
    actions, diagnostics = catalog(tree, linux, 'x86')
    actions = {name: dict(action, deps=sorted(action['deps'])) for name, action in actions.items()}
    data = dict(actions=actions, program=runtime_program(program, linux), link_script=link_script)
    output.mkdir()
    encoded = json.dumps(data, separators=(',', ':'))
    # This module is loaded once by the source-local callback. Native config and
    # measured probe results remain execution inputs rather than static values.
    (output / 'program.bzl').write_text('KBUILD = json.decode(' + repr(encoded) + ')\n')
    (output / 'report.json').write_text(json.dumps(dict(data, diagnostics=diagnostics), indent=2) + '\n')
