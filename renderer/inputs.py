"""Write symbolic kernel input data for the measured runtime."""
import json
from catalog import catalog


def write_inputs(output, tree, linux, syntax, program, link_script, fragments):
    encoded = {name: json.dumps(body) for name, body in syntax.items()}
    actions, diagnostics = catalog(tree, linux, 'x86')
    actions = {name: dict(action, deps=sorted(action['deps'])) for name, action in actions.items()}
    base = {node['symbol']: 'n' for body in syntax.values() for node in body
            if node['symbol'] and node['type'] in ('bool', 'boolean', 'tristate')
            and any(prop['kind'] in ('prompt', 'menu') and prop.get('text') for prop in node['properties'])}
    requested = dict(EXPERT='y', **{'64BIT': 'y'}, PRINTK='y', TTY='y', SERIAL_8250='y',
                     SERIAL_8250_CONSOLE='y', BLK_DEV_INITRD='y', RD_GZIP='y', BINFMT_ELF='y',
                     HYPERVISOR_GUEST='y', PVH='y', UNWINDER_FRAME_POINTER='y', CC_OPTIMIZE_FOR_SIZE='y',
                     OBJTOOL='n', UNWINDER_ORC='n', STACK_VALIDATION='n', DEBUG_INFO_NONE='y')
    profile = dict(base=base, requested=requested)
    programs = set()

    def walk(value):
        if isinstance(value, dict):
            if value.get('op') == 'function' and value.get('name') == 'kbuild_host_fallback':
                terms = value['arguments'][0]
                if any(term.get('op') != 'literal' for term in terms):
                    raise ValueError('Non-literal host capability program')
                programs.add(''.join(term['value'] for term in terms))
            for child in value.values():
                walk(child)
        elif isinstance(value, list):
            for child in value:
                walk(child)
    walk(program)
    (output / 'rules').mkdir(parents=True, exist_ok=True)
    (output / 'native').mkdir()
    for directory in [output, output / 'rules', output / 'native']:
        (directory / 'BUILD.bazel').write_text('package(default_visibility=["//visibility:public"])\nexports_files(glob(["**"], exclude=["BUILD.bazel"]))\n')
    (output / 'MODULE.bazel').write_text('module(name="kernel_inputs")\n')
    for filename, value in [('baseline.json', encoded), ('profile.json', profile),
                            ('host-programs.json', sorted(programs)), ('diagnostics.json', diagnostics)]:
        (output / filename).write_text(json.dumps(value))
    for filename, name, value in [('sources', 'SOURCES', encoded), ('data', 'PROFILE', profile),
                                   ('catalog', 'ACTIONS', actions), ('flag_program', 'FLAG_PROGRAM', program),
                                   ('qualification', 'FRAGMENTS', fragments)]:
        (output / 'rules' / (filename+'.bzl')).write_text(name+' = json.decode('+repr(json.dumps(value))+')\n')
    (output / 'rules/link-vmlinux.sh').write_text(link_script)
    (output / 'provenance.json').write_text(json.dumps(dict(kconfig_files=len(syntax), actions=len(actions)), indent=2)+'\n')
