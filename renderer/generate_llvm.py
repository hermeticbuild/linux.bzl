#!/usr/bin/env python3
"""Generate an analysis-resolved LLVM prototype. Make runs only in this tool."""
import argparse
import json
import os
from pathlib import Path
import shutil
import tempfile

from export import native_events, parse_events
from generate import relative, walk
from generate_actions import generate_actions


HERE = Path(__file__).resolve().parent
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


def flags_program(tree, linux, defer_arch=False):
    bodies = {relative(f['file'], linux): f['body'] for f in tree['files']}
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
                    out.append({'kind': 'flag_includes', 'files': {p: expand(bodies[p], stack + (p,)) for p in bodies
                                if p.startswith('scripts/Makefile.') and p.rsplit('/', 1)[-1] in
                                ['Makefile.warn', 'Makefile.debug', 'Makefile.btf', 'Makefile.kasan', 'Makefile.kcsan',
                                 'Makefile.kmsan', 'Makefile.ubsan', 'Makefile.kcov', 'Makefile.randstruct',
                                 'Makefile.kstack_erase', 'Makefile.autofdo', 'Makefile.propeller', 'Makefile.context-analysis', 'Makefile.gcc-plugins']}})
                elif arg in bodies and arg not in stack and arg != 'scripts/Makefile.compiler':
                    out.extend(expand(bodies[arg], stack + (arg,)))
            else:
                out.append(n)
        return out
    # Derive setup boundaries from native assignment events instead of copying
    # flag values or configuring Linux during BUILD generation.
    nodes = list(walk(bodies['Makefile']))
    low = next(n['line'] for n, _ in nodes if n.get('name') == 'KBUILD_USERHOSTCFLAGS')
    high = next(n['line'] for n, _ in nodes if n.get('name') == 'KBUILD_LDFLAGS' and '-z noexecstack' in n.get('value', ''))
    top = expand(bounded(bodies['Makefile'], low, high))
    # Compiler helper definitions are symbolic analysis intrinsics. Unknown
    # capabilities warn and use the native helper's failure branch.
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
            # toolchain at analysis time, never the generator's compiler.
            return {'op': 'function', 'name': 'kbuild_probe', 'arguments': [literal(name), *args[1:]], 'probe_context': context}
        if name in VERSIONS:
            return {'op': 'function', 'name': 'kbuild_min_version', 'arguments': [
                [{'op': 'config', 'name': 'CONFIG_' + VERSIONS[name]}], args[1]]}
    return value


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--ast', type=Path, required=True)
    p.add_argument('--linux', type=Path, default=Path('_tmp/linux'))
    p.add_argument('--syntax', type=Path, help='File-local native Kconfig syntax; source edges resolve in analysis')
    p.add_argument('--make', type=Path, required=True)
    p.add_argument('--out', type=Path, default=Path('_tmp/kernel-llvm/repo'))
    p.add_argument('--setup-name', default='toolchain')
    p.add_argument('--default-config', default='//configs:baseline')
    p.add_argument('--config-scope')
    a = p.parse_args(argv)
    tree = json.loads(a.ast.read_text())
    extras = sorted(a.linux.rglob('Makefile.*') if a.syntax else a.linux.glob('scripts/Makefile.*')) + [a.linux / 'scripts/Kbuild.include']
    if not a.syntax:
        extras.append(a.linux / 'arch/x86/Makefile_32.cpu')
    existing = {Path(f['file']).resolve() for f in tree['files']}
    tree['files'].extend(parse_events(native_events(a.make, [p for p in extras if p.resolve() not in existing], expressions=True))['files'])
    linux, out = a.linux.resolve(), a.out.resolve()
    if not a.syntax:
        p.error('--syntax is required; the repository tool exports native file-local syntax')
    generate_actions(tree, linux, out, a.make.resolve(), syntax=a.syntax,
                     setup_name=a.setup_name, default_config=a.default_config, config_scope=a.config_scope)
    global_program, directory, helpers, bodies = flags_program(tree, linux, defer_arch=bool(a.syntax))
    capabilities = {}
    commands = {p: slice_program(body, {'cmd_asn1_compiler', 'cmd_bison', 'cmd_flex', 'cmd_gzip', 'cmd_elfconfig', 'cmd_build_OID_registry', 'cmd_syshdr', 'cmd_systbl', 'filechk_offsets', 'filechk_gen_featuremasks', 'filechk_gentimeconst', 'filechk_cat', 'out', 'uapi'}) for p, body in bodies.items()}
    target_assignments = [n for f in tree['files'] for n, _ in walk(f['body']) if n['kind'] == 'target_assignment']
    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / 'expressions.mk'
        source.write_text(''.join(f'x{i}_{field} = {n[field]}\n' for i, n in enumerate(target_assignments) for field in ('targets', 'name', 'value')))
        expressions = {n['name']: n['value_expression'] for n in native_events(a.make, [source], expressions=True) if n['kind'] == 'assignment'}
    for i, n in enumerate(target_assignments):
        for field in ('targets', 'name', 'value'):
            n[field + '_expression'] = expressions[f'x{i}_{field}']
    targets = {p: [n for n, _ in walk(body) if n['kind'] == 'target_assignment'] for p, body in bodies.items()}
    data = lower_probes({'global': global_program, 'directories': directory, 'helpers': helpers, 'commands': commands, 'target_assignments': targets})
    data['capabilities'] = capabilities
    (out / 'rules/flag_program.bzl').write_text('FLAG_PROGRAM = json.decode(' + repr(json.dumps(data)) + ')\n')
    for name, dest in [('llvm_actions.bzl', 'actions.bzl'), ('llvm_toolchain.bzl', 'toolchain.bzl'),
                       ('llvm_resolved.bzl', 'resolved.bzl'), ('llvm_commands.bzl', 'commands.bzl'),
                       ('llvm_expressions.bzl', 'expressions.bzl'), ('compiler_inputs.bzl', 'compiler_inputs.bzl'), ('materialize.py', 'materialize.py')]:
        shutil.copyfile(HERE / name, out / 'rules' / dest)
    (out / 'rules/materialize.py').chmod(0o755)
    (out / 'MODULE.bazel').write_text('module(name="kernel_llvm_actions")\nbazel_dep(name="bazel_skylib",version="1.9.0")\nbazel_dep(name="rules_cc",version="0.2.25")\nbazel_dep(name="platforms",version="1.0.0")\nbazel_dep(name="llvm",version="0.8.24")\nregister_toolchains("@llvm//toolchain:all")\n')
    print(json.dumps({'flag_statements': len(data['global']), 'compiler_probes': len(capabilities), 'workspace': str(out)}))


if __name__ == '__main__':
    main()
