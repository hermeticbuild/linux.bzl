#!/usr/bin/env python3
"""Extract symbolic Make programs and compiler queries without resolving them."""
from pathlib import Path
import tempfile

from export import native_events
from syntax import relative, walk


PROBES = {'cc-option', 'cc-option-yn', 'cc-disable-warning', 'as-option', 'as-instr', 'ld-option', 'try-run', '__cc-option', 'rustc-option', '__rustc-option', 'rustc-option-yn'}
VERSIONS = {'gcc-min-version': 'GCC_VERSION', 'clang-min-version': 'CLANG_VERSION', 'rustc-min-version': 'RUSTC_VERSION'}
ROOTS = {'KBUILD_CFLAGS', 'KBUILD_AFLAGS', 'KBUILD_CPPFLAGS', 'KBUILD_LDFLAGS', 'KBUILD_HOSTCFLAGS',
         'KBUILD_HOSTCXXFLAGS', 'KBUILD_HOSTLDFLAGS', 'KBUILD_HOSTLDLIBS', 'NOSTDINC_FLAGS',
         'LINUXINCLUDE', 'c_flags', 'a_flags', 'hostc_flags', 'hostcxx_flags', 'ld_flags',
         'ccflags-y', 'asflags-y', 'subdir-ccflags-y', 'subdir-asflags-y'}


def literal(text):
    return [{'op': 'literal', 'value': text}]


def const(terms):
    return ''.join(t['value'] for t in terms) if all(t['op'] == 'literal' for t in terms) else None


def refs(value):
    if isinstance(value, list):
        return set().union(*(refs(v) for v in value)) if value else set()
    if not isinstance(value, dict):
        return set()
    result = set()
    if value.get('directive') in ('ifdef', 'ifndef') and 'operands' in value:
        # The operand names a variable; even a literal name reads its value.
        name = const(value['operands'][0])
        if name is not None:
            result.add(name.strip())
    if value.get('op') == 'reference':
        name = const(value['name'])
        if name is not None:
            result.add(name)
        else:
            prefix = value['name'][0].get('value', '')
            result.add(prefix + '*')
    elif value.get('op') == 'function' and value['name'] == 'call':
        name = const(value['arguments'][0])
        if name and name.strip() not in PROBES | VERSIONS.keys():
            result.add(name.strip())
    for v in value.values():
        result.update(refs(v))
    return result


def matches(name, wanted):
    return any(name == n or (n.endswith('*') and name.startswith(n[:-1])) or
               (name.endswith('*') and n.startswith(name[:-1])) for n in wanted)


def selected(body, wanted):
    out = []
    for n in body:
        if n['kind'] == 'conditional':
            branches = [dict(b, body=selected(b['body'], wanted)) for b in n['branches']]
            if any(b['body'] for b in branches):
                out.append(dict(n, branches=branches))
        elif n['kind'] == 'flag_includes':
            programs = {p: selected(body, wanted) for p, body in n['files'].items()}
            out.append(dict(n, files=programs))
        elif n['kind'] in ('assignment', 'define'):
            name = const(n['name_expression'])
            if name is None:
                name = n['name_expression'][0].get('value', '') + '*'
            if matches(name, wanted):
                out.append(n)
    return out


def export_directives(body):
    """Retain export ownership and only the guards that control it."""
    result = []
    for node in body:
        if node['kind'] == 'conditional':
            branches = [dict(branch, body=export_directives(branch['body']))
                        for branch in node['branches']]
            if any(branch['body'] for branch in branches):
                result.append(dict(node, branches=branches))
        elif node['kind'] == 'directive' and node.get('directive') in ('export', 'unexport'):
            result.append(node)
        elif node['kind'] in ('assignment', 'define') and node.get('export') in (1, 2):
            result.append({'kind': 'directive', 'directive': 'export' if node['export'] == 1 else 'unexport',
                           'arguments': node['name'], 'arguments_expression': node['name_expression']})
    return result


def slice_program(body, wanted):
    wanted = set(wanted)
    for _ in range(100):
        result = selected(body, wanted)
        dependencies = refs(result)
        extra = {n for n in dependencies if not n.startswith('CONFIG_')} - wanted
        if not extra:
            return result
        wanted.update(extra)
    raise ValueError('Flag dependency slice did not converge')


def bounded(body, low, high):
    out = []
    for n in body:
        if n['kind'] == 'conditional' and n['line'] < low:
            # Top-level build orchestration encloses the compiler setup. The
            # generator selects that scope; CONFIG conditionals inside remain.
            for b in n['branches']:
                out.extend(bounded(b['body'], low, high))
        elif low <= n['line'] <= high:
            out.append(n)
    return out


def phase_body(body, anchor):
    """Select an established invocation while retaining its value guards."""
    location = (anchor['file'], anchor['line'])
    matches = [node for node, _ in walk(body)
               if (node['file'], node['line']) == location]
    if len(matches) != 1:
        raise ValueError(f'Expected one native phase anchor at {location}, got {matches}')
    result = []
    for node in body:
        if node['kind'] == 'conditional':
            branches = [branch for branch in node['branches']
                        if any((child['file'], child['line']) == location
                               for child, _ in walk(branch['body']))]
            if branches:
                result.extend(phase_body(branches[0]['body'], anchor))
                continue
        result.append(node)
    return result


def flags_program(tree, linux, defer_arch=False):
    bodies = {relative(f['file'], linux): f['body'] for f in tree['files']}
    flag_includes = set()
    for node, _ in walk(bodies['Makefile']):
        if node['kind'] == 'assignment' and node.get('name', '').startswith('include-'):
            paths = const(node['value_expression'])
            if paths is None:
                raise ValueError(f"Flag include paths must be source literals: {node['value']}")
            flag_includes.update(paths.split())
    def expand(body, stack=()):
        out = []
        for n in body:
            if n['kind'] == 'conditional':
                out.append(dict(n, branches=[dict(b, body=expand(b['body'], stack)) for b in n['branches']]))
            elif n['kind'] == 'directive' and n['directive'] in ('include', '-include'):
                arg = n['arguments'].strip().replace('$(srctree)/', '')
                if defer_arch and '$(SRCARCH)' in arg:
                    out.append(n)
                    continue
                arg = arg.replace('$(SRCARCH)', 'x86')
                if arg == '$(addprefix , $(include-y))':
                    # Retain the configuration-dependent include list as a
                    # program operation evaluated by the analysis interpreter.
                    out.append({'kind': 'flag_includes', 'files': {p: expand(bodies[p], stack + (p,))
                                for p in sorted(flag_includes) if p in bodies}})
                elif arg in bodies and arg not in stack and arg != 'scripts/Makefile.compiler':
                    out.extend(expand(bodies[arg], stack + (arg,)))
            else:
                out.append(n)
        return out
    # Derive setup boundaries from native assignment events instead of copying
    # flag values or configuring Linux during BUILD generation.
    nodes = list(walk(bodies['Makefile']))
    compiler_flags = [n['line'] for n, _ in nodes
                      if n.get('name', '').startswith('KBUILD_') and n.get('name', '').endswith('CFLAGS')]
    if not compiler_flags:
        raise ValueError('Top Makefile has no KBUILD compiler flag definitions')
    low = min(compiler_flags)
    high = next(n['line'] for n, _ in nodes if n.get('name') == 'KBUILD_LDFLAGS' and '-z noexecstack' in n.get('value', ''))
    top = expand(bounded(bodies['Makefile'], low, high))
    # Compiler helper definitions stay symbolic until the selected compiler
    # executes their probes in the configured build context.
    common = bodies['scripts/Kbuild.include']
    # Directory and helper references also pull their global flag definitions
    # into the setup program (sanitizer/module flags, for example).
    directory = {p: slice_program(body, ROOTS | {'HOST_EXTRACFLAGS', 'HOST_EXTRACXXFLAGS', 'HOSTLDLIBS_*',
                    'CFLAGS_*', 'AFLAGS_*', 'HOSTCFLAGS_*', 'HOSTCXXFLAGS_*', 'CFLAGS_REMOVE_*', 'ccflags-remove-y'}) for p, body in bodies.items()
                 if p.rsplit('/', 1)[-1] in ('Makefile', 'Kbuild') and p != 'Makefile'}
    helpers = slice_program(bodies['scripts/Makefile.lib'] + bodies['scripts/Makefile.host'] + common,
                            ROOTS | {'filechk_offsets', 'cmd_asn1_compiler', 'cmd_bison', 'cmd_flex', 'cmd_gzip'})
    dependencies = refs(helpers) | refs(list(directory.values()))
    needed = ROOTS | {'include-y'} | {n for n in dependencies if n.startswith(('KBUILD_CFLAGS', 'KBUILD_AFLAGS', 'KBUILD_CPPFLAGS', 'KBUILD_LDFLAGS', 'KBUILD_HOST', 'CFLAGS_', 'AFLAGS_', 'LDFLAGS_', 'CC_FLAGS_'))}
    global_program = slice_program(common + top, needed)
    return global_program, directory, helpers, bodies


def lower_probes(value):
    if isinstance(value, list):
        return [lower_probes(v) for v in value]
    if not isinstance(value, dict):
        return value
    if value.get('kind') == 'assignment' and value.get('name') == 'CC_VERSION_TEXT':
        return dict(value, value_expression=[{'op': 'config', 'name': 'CONFIG_CC_VERSION_TEXT'}])
    value = {k: lower_probes(v) for k, v in value.items()}
    if value.get('op') == 'function' and value['name'] == 'call':
        args = value['arguments']
        name = const(args[0])
        name = name.strip() if name else None
        if name in PROBES:
            context = {}
            if name in ('__cc-option', '__rustc-option'):
                context = {'compiler': args[1], 'flags': args[2]}
                name = 'cc-option' if name == '__cc-option' else 'rustc-option'
                args = [literal(name), *args[3:]]
            # Keep the probe symbolic. Its answer belongs to the selected
            # toolchain during execution, never the generator's compiler.
            return {'op': 'function', 'name': 'kbuild_probe', 'arguments': [literal(name), *args[1:]], 'probe_context': context}
        if name in VERSIONS:
            return {'op': 'function', 'name': 'kbuild_min_version', 'arguments': [
                [{'op': 'config', 'name': 'CONFIG_' + VERSIONS[name]}], args[1]]}
    return value


def initial_program(tree, linux, make):
    global_program, _, helpers, bodies = flags_program(tree, linux, defer_arch=True)
    capabilities = {}
    commands = {p: slice_program(body, {'cmd_asn1_compiler', 'cmd_bison', 'cmd_flex', 'cmd_gzip', 'cmd_elfconfig', 'cmd_build_OID_registry', 'cmd_syshdr', 'cmd_systbl', 'filechk_offsets', 'filechk_gen_featuremasks', 'filechk_gentimeconst', 'filechk_cat', 'out', 'uapi'}) for p, body in bodies.items()}
    target_assignments = [n for f in tree['files'] for n, _ in walk(f['body']) if n['kind'] == 'target_assignment']
    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / 'expressions.mk'
        source.write_text(''.join(f'x{i}_{field} = {n[field]}\n' for i, n in enumerate(target_assignments) for field in ('targets', 'name', 'value')))
        expressions = {n['name']: n['value_expression'] for n in native_events(make, [source], expressions=True) if n['kind'] == 'assignment'}
    for i, n in enumerate(target_assignments):
        for field in ('targets', 'name', 'value'):
            n[field + '_expression'] = expressions[f'x{i}_{field}']
    targets = {p: [n for n, _ in walk(body) if n['kind'] == 'target_assignment'] for p, body in bodies.items()}
    data = lower_probes({'global': global_program, 'helpers': helpers, 'commands': commands, 'target_assignments': targets})
    data['capabilities'] = capabilities
    return data
