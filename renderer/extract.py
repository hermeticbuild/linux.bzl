#!/usr/bin/env python3
"""Lower native kernel Make syntax into execution-time interpreter programs."""
import re
import tempfile
from pathlib import Path

import programs as backend
from export import native_events, parse_events
from syntax import walk, relative
from kconfig_syntax import export_syntax
from text_transforms import lower_text_transforms
from compiler_queries import lower_compiler_queries
from shell_outputs import output_redirections
from execution_paths import lower_execution_paths


def make_text(terms):
    result = []
    for term in terms:
        if term['op'] == 'literal':
            result.append(term['value'].replace('$', '$$'))
        elif term['op'] == 'config':
            result.append('$(' + term['name'] + ')')
        elif term['op'] == 'reference':
            result.append('$(' + make_text(term['name']) + ')')
        elif term['op'] == 'function':
            result.append('$(' + term['name'] + ' ' + ','.join(make_text(a) for a in term['arguments']) + ')')
        else:
            raise ValueError(term)
    return ''.join(result)


def fragment_provenance(value, filename):
    if isinstance(value, dict):
        return {k: filename if k == 'file' else fragment_provenance(v, filename)
                for k, v in value.items()}
    if isinstance(value, list):
        return [fragment_provenance(v, filename) for v in value]
    return value


def lower_eval_template(terms, make, definitions):
    """Parse fixed eval template syntax natively, retaining its symbolic loops."""
    significant = [t for t in terms if t['op'] != 'literal' or t['value'].strip()]
    if len(significant) == 1 and significant[0].get('name') == 'foreach':
        args = significant[0]['arguments']
        return [{'kind': 'eval_foreach', 'name_expression': args[0], 'words_expression': args[1],
                 'body': lower_eval_template(args[2], make, definitions)}]
    # eval removes one dollar-escaping layer before the native parser sees the
    # template. Runtime references remain references; no config is expanded.
    inputs, pieces = {}, []
    for term in terms:
        if term['op'] == 'literal':
            pieces.append(term['value'])
        else:
            name = '__KBUILD_EVAL_INPUT_' + str(len(inputs))
            inputs[name] = [term]
            pieces.append('$(' + name + ')')
    text = ''.join(pieces)
    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / 'eval-template.mk'
        source.write_text(text + '\n')
        nodes = parse_events(native_events(make, [source], expressions=True))['files'][0]['body']
        nodes = fragment_provenance(nodes, '<eval-template>')
    entries = [(n, 'text' if n['kind'] == 'expression' else 'value')
               for n, _ in walk(nodes) if n['kind'] in ('expression', 'recipe') and 'value_expression' not in n]
    if entries:
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / 'eval-expressions.mk'
            source.write_text(''.join('define __eval_expr_%d =\n%s\nendef\n' % (i, n[field])
                                      for i, (n, field) in enumerate(entries)))
            expressions = {n['name']: n['value_expression'] for n in native_events(make, [source], expressions=True)
                           if n['kind'] == 'define'}
        for i, (node, _) in enumerate(entries):
            node['value_expression'] = expressions['__eval_expr_' + str(i)]
    nodes = lower_body(nodes, make, definitions)
    return [{'kind': 'eval_template', 'inputs': inputs, 'body': nodes}]


def lower_statement(terms, make, definitions):
    terms = [t for t in terms if t['op'] != 'literal' or t['value'].strip()]
    if len(terms) != 1 or terms[0]['op'] != 'function':
        return None
    term = terms[0]
    args = term['arguments']
    if term['name'] == 'shell' and make_text(args[0]).strip().startswith('mkdir -p '):
        return {'kind': 'declared_directories', 'expression': args[0]}
    if term['name'] == 'foreach':
        body = lower_statement(args[2], make, definitions)
        return {'kind': 'foreach_eval', 'name_expression': args[0], 'words_expression': args[1], 'body': [body]} if body else None
    if term['name'] == 'eval':
        inner = args[0]
        if len(inner) == 1 and inner[0].get('name') == 'call':
            call = inner[0]['arguments']
            name = make_text(call[0]).strip()
            definition = definitions.get(name)
            if definition:
                return {'kind': 'eval_call', 'arguments': call[1:],
                        'body': lower_eval_template(definition['value_expression'], make, definitions)}
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / 'eval.mk'
            source.write_text(make_text(args[0]) + '\n')
            nodes = [n for n in native_events(make, [source], expressions=True) if not n['kind'].startswith('file_')]
        if len(nodes) == 1 and nodes[0]['kind'] == 'assignment':
            return dict(nodes[0], kind='eval_assignment', file='<eval>')
    return None


def lower_body(body, make, definitions=None):
    definitions = definitions or {n["name"]: n for n, _ in walk(body) if n["kind"] == "define"}
    result = []
    for node in body:
        if node['kind'] == 'conditional':
            node = dict(node, branches=[dict(b, body=lower_body(b['body'], make, definitions)) for b in node['branches']])
        elif node['kind'] == 'expression':
            lowered = lower_statement(node['value_expression'], make, definitions)
            if lowered:
                node = dict(node, **lowered)
        result.append(node)
    return result


_original_selected = backend.selected
_INCLUDE_DIRECTIVES = False
_INCLUDE_HEADER_RULES = False

def image_selected(body, wanted):
    result = []
    for node in body:
        if node['kind'] == 'foreach_eval':
            children = image_selected(node['body'], wanted)
            if children:
                result.append(dict(node, body=children))
        elif _INCLUDE_HEADER_RULES and node['kind'] in ('rule', 'recipe'):
            result.append(node)
        elif node['kind'] == 'directive' and (node['directive'] in ('export', 'unexport') or (_INCLUDE_DIRECTIVES and node['directive'] in ('include', '-include', 'sinclude'))):
            result.append(node)
        elif node['kind'] == 'target_assignment':
            name = backend.const(node['name_expression'])
            if name and backend.matches(name, wanted):
                result.append(node)
        elif node['kind'] == 'eval_assignment':
            result.extend([dict(n, kind='eval_assignment') for n in _original_selected([dict(node, kind='assignment')], wanted)])
        else:
            result.extend(_original_selected([node], wanted))
    return result


def extract(tree, linux, make):
    global _INCLUDE_DIRECTIVES, _INCLUDE_HEADER_RULES
    syntax = export_syntax(linux)
    setup_files = {relative(source["file"], linux) for source in tree["files"]} | {"scripts/Kbuild.include"}
    extras = [p for p in sorted(linux.rglob('Makefile*')) if p.is_file()] + sorted(linux.rglob('Kbuild.*')) + sorted(linux.rglob('Platform')) + [linux / 'scripts/Kbuild.include', linux / 'tools/objtool/Build', *sorted(linux.glob('tools/objtool/arch/*/Build')), linux / 'tools/lib/subcmd/Build']
    existing = {Path(f['file']).resolve() for f in tree['files']}
    tree['files'].extend(parse_events(native_events(make, [p for p in extras if p.resolve() not in existing], expressions=True))['files'])
    entries = []
    for f in tree['files']:
        for n, _ in walk(f['body']):
            if n['kind'] == 'directive' and n['directive'] in ('include', '-include', 'sinclude', 'export', 'unexport'):
                entries.append((n, 'arguments', 'arguments_expression'))
            elif n['kind'] in ('expression', 'recipe'):
                entries.append((n, 'value' if n['kind'] == 'recipe' else 'text', 'value_expression'))
            elif n['kind'] == 'target_assignment':
                entries.extend((n, key, key + '_expression') for key in ('targets', 'name', 'value'))
    with tempfile.TemporaryDirectory() as tmp:
        source = Path(tmp) / 'expressions.mk'
        source.write_text(''.join(f'x{i} = {n[field]}\n' for i, (n, field, _) in enumerate(entries)))
        expressions = {n['name']: n['value_expression'] for n in native_events(make, [source], expressions=True) if n['kind'] == 'assignment'}
    for i, (n, _, field) in enumerate(entries):
        n[field] = expressions['x' + str(i)]
    bodies = {relative(f['file'], linux): lower_execution_paths(lower_compiler_queries(lower_text_transforms(backend.lower_probes(lower_body(f['body'], make))))) for f in tree['files']}
    # The native build frontend supplies composite-object and archive semantics.
    build = bodies['scripts/Makefile.build']
    flat = [n for n, _ in walk(build)]
    low = next(n['line'] for n in flat if n.get('name') == 'obj-m' and 'filter-out' in n.get('value', ''))
    high = next(n['line'] for n in flat if n.get('name') == 'subdir-modorder')
    graph = backend.slice_program(backend.bounded(build, low, high), {'real-obj-y', 'lib-y', 'always-y', 'extra-y', 'subdir-ym', 'multi-obj-y'})
    data = backend.initial_program(tree, linux, make)
    for key in ["commands", "target_assignments"]:
        data[key] = {name: body for name, body in data[key].items() if name in setup_files}
    backend.selected = image_selected
    def flag_body(body):
        result = []
        for n in body:
            if n['kind'] == 'conditional':
                result.append(dict(n, branches=[dict(b, body=flag_body(b['body'])) for b in n['branches']]))
            elif n['kind'] not in ('rule', 'recipe'):
                result.append(n)
        return result
    data['image_flag_programs'] = {p: flag_body(body) for p, body in bodies.items()}
    data['directories'] = {p: body for p, body in data['image_flag_programs'].items() if p.rsplit('/', 1)[-1] in ('Makefile', 'Kbuild') and p != 'Makefile'}
    data['image_programs'] = bodies
    data['image_compile_commands'] = sorted({n['name'][4:] for body in bodies.values() for n, _ in walk(body) if n['kind'] in ('assignment', 'define') and n.get('name', '').startswith('cmd_') and backend.refs(n['value_expression']) & {'c_flags', 'a_flags'}})
    data['image_postlink_outputs'] = {p: [expr for n, _ in walk(body) if n['kind'] in ('assignment', 'define') and n.get('name', '').startswith('cmd_') for expr in output_redirections(n['value_expression'])] for p, body in bodies.items() if p.endswith('/Makefile.postlink')}
    data['image_dtbs'] = bodies['scripts/Makefile.dtbs']
    data['graph_helpers'] = graph
    data['image_arch'] = [{'kind': 'directive', 'directive': 'include', 'arguments_expression': [
        {'op': 'literal', 'value': 'arch/'}, {'op': 'reference', 'name': [{'op': 'literal', 'value': 'SRCARCH'}]}, {'op': 'literal', 'value': '/Makefile'}]}]
    top = bodies['Makefile']
    top_nodes = [n for n, _ in walk(top)]
    first = next(n['line'] for n in top_nodes if n.get('name') == 'core-y')
    last = next(n['line'] for n in top_nodes if n.get('name') == 'libs-y')
    data['image_roots_init'] = backend.bounded(top, first, last)
    data['image_roots'] = backend.slice_program(backend.bounded(top, next(n['line'] for n in top_nodes if n.get('name') == 'ARCH_CORE'), next(n['line'] for n in top_nodes if n.get('name') == 'KBUILD_LDS')), {'ARCH_CORE', 'ARCH_LIB', 'ARCH_DRIVERS', 'KBUILD_LDS', 'KBUILD_VMLINUX_OBJS', 'KBUILD_VMLINUX_LIBS'})
    def export_directives(body):
        result = []
        for node in body:
            if node['kind'] == 'conditional':
                result.append(dict(node, branches=[dict(b, body=export_directives(b['body'])) for b in node['branches']]))
            elif node['kind'] == 'directive' and node.get('directive') in ('export', 'unexport'):
                result.append(node)
            elif node['kind'] in ('assignment', 'define') and node.get('export') in (1, 2):
                result.append({'kind': 'directive', 'directive': 'export' if node['export'] == 1 else 'unexport', 'arguments': node['name'], 'arguments_expression': node['name_expression']})
        return result
    data['image_exports'] = export_directives(top)
    data['image_version'] = backend.slice_program(top, {'VERSION', 'PATCHLEVEL', 'SUBLEVEL', 'EXTRAVERSION'})
    first = next(n['line'] for n in flat if n.get('name') == 'obj-y')
    last = next(n['line'] for n in flat if n.get('name') == 'subdir-rustflags-y')
    data['graph_init'] = backend.bounded(build, first, last)
    data['image_archives'] = {
        'builtin': next(n['value_expression'] for n, _ in walk(bodies['scripts/Makefile.build']) if n.get('name') == 'cmd_ar_builtin'),
        'library': next(n['value_expression'] for n, _ in walk(bodies['scripts/Makefile.lib']) if n.get('name') == 'cmd_ar'),
    }
    data['image_command_helpers'] = backend.slice_program(
        bodies['scripts/Kbuild.include'] + bodies['scripts/Makefile.lib'] + bodies['scripts/Makefile.host'] + bodies['scripts/Makefile.build'] + backend.selected(backend.bounded(top, next(n['line'] for n in top_nodes if n.get('name') == 'uts_len'), next(n['line'] for n in top_nodes if n.get('name') == 'filechk_compile.h')), {'filechk_*', 'uts_len'}),
        {'cmd_*', 'filechk_*'})
    data['image_utilities'] = [n for n in top_nodes if n.get('name') in {'AWK', 'PERL', 'PYTHON3', 'BASH', 'KGZIP', 'KBZIP2', 'KLZMA', 'LZOP', 'LZ4', 'XZ', 'ZSTD', 'LEX', 'YACC', 'CONFIG_SHELL'}]
    def host_fallback(value):
        if isinstance(value, list):
            return [host_fallback(v) for v in value]
        if isinstance(value, dict):
            if value.get('op') == 'function' and value.get('name') == 'shell':
                if 'libelf --cflags' in make_text(value['arguments'][0]):
                    return {'op': 'literal', 'value': ''}  # Supplied by the CcInfo dependency.
                if 'grep elf_getshdr' in make_text(value['arguments'][0]):
                    return {'op': 'function', 'name': 'kbuild_libelf_api', 'arguments': []}
                return {'op': 'function', 'name': 'kbuild_host_fallback', 'arguments': [[{'op': 'literal', 'value': make_text(value['arguments'][0])}]]}
            return {k: host_fallback(v) for k, v in value.items()}
        return value
    data['image_host_objtool'] = host_fallback(backend.slice_program(bodies['tools/objtool/Makefile'], {'OBJTOOL_CFLAGS', 'BUILD_ORC', 'BUILD_KLP'}))
    data['image_host_build'] = backend.slice_program(bodies['tools/objtool/Build'], {'objtool-y'}) + [n for n in bodies['tools/objtool/Build'] if n['kind'] == 'rule']
    data['image_host_arch'] = bodies
    data['image_host_subcmd'] = backend.slice_program(bodies['tools/lib/subcmd/Makefile'] + bodies['tools/lib/subcmd/Build'], {'libsubcmd-y', 'CFLAGS'})
    _INCLUDE_DIRECTIVES = True
    data['image_link_global'] = backend.slice_program(backend.bounded(top, min(n['line'] for n in top_nodes if n.get('name') == 'LDFLAGS_vmlinux'), max(n['line'] for n in top_nodes if n.get('name') == 'LDFLAGS_vmlinux')), {'LDFLAGS_vmlinux'})
    data['image_link_programs'] = {p: backend.slice_program(body, {'LDFLAGS_vmlinux'}) for p, body in bodies.items()}
    backend.ROOTS |= {'core-y', 'libs-y', 'drivers-y', 'drivers-m'}
    global_program, _, helpers, _ = backend.flags_program(tree, linux, defer_arch=True)
    data['global'] = lower_compiler_queries(lower_text_transforms(backend.lower_probes(global_program)))
    def export_names(body):
        names = set()
        for node, _ in walk(body):
            if node['kind'] == 'directive' and node.get('directive') == 'export':
                text = backend.const(node.get('arguments_expression', backend.literal(node['arguments'])))
                if text is not None:
                    names.update(text.split())
            elif node['kind'] in ('assignment', 'define') and node.get('export') == 1:
                name = backend.const(node['name_expression'])
                if name is not None:
                    names.add(name)
        return names
    setup_roots = backend.ROOTS | backend.refs(data["image_command_helpers"]) | backend.refs(global_program) | backend.refs(helpers) | {'BITS', 'UTS_MACHINE', 'REALMODE_CFLAGS', 'CLANG_FLAGS', 'libs-y', 'core-y', 'drivers-y', 'LDFLAGS_vmlinux'}
    def slice_includes(roots):
        # An included file may define a variable referenced by its caller.
        # Compute the closure across files before pruning individual bodies.
        wanted = set(roots) | set().union(*(export_names(body) for body in bodies.values()))
        for _ in range(100):
            selected = {p: backend.selected(body, wanted) for p, body in bodies.items()}
            extra = {name for body in selected.values() for name in backend.refs(body)
                     if not name.startswith('CONFIG_')} - wanted
            if not extra:
                return selected
            wanted.update(extra)
        raise ValueError('Cross-file Make dependency slice did not converge')
    data['image_setup_programs'] = slice_includes(setup_roots)
    data['image_graph_programs'] = slice_includes({'libs-y', 'core-y', 'drivers-y', 'BITS', 'UTS_MACHINE', 'REALMODE_CFLAGS'})
    _INCLUDE_HEADER_RULES = True
    header_body, active = [], False
    for node in bodies['scripts/Makefile.asm-headers']:
        if node['kind'] == 'rule':
            active = '%.h' in node['targets']
        if node['kind'] not in ('rule', 'recipe') or active:
            header_body.append(node)
    data['image_asm_headers'] = backend.slice_program(header_body, {'generic-y', 'syscall-y', 'generated-y', 'filechk_wrap', 'cmd_syshdr', 'cmd_systbl'})
    _INCLUDE_HEADER_RULES = False
    _INCLUDE_DIRECTIVES = False
    data['image_link'] = {}
    for driver, roots in {'vmlinux_a': {'cmd_ar_builtin_fixup', 'cmd_copy'}, 'vmlinux_o': {'cmd_ld_vmlinux.o'}, 'vmlinux': {'cmd_strip_relocs', 'LDFLAGS_vmlinux'}}.items():
        data['image_link'][driver] = backend.slice_program(bodies['scripts/Makefile.lib'] + bodies['scripts/Makefile.' + driver], roots)
    modpost = bodies['scripts/Makefile.modpost']
    nodes = [n for n, _ in walk(modpost)]
    high = next(n['line'] for n in nodes if n.get('name') == 'modpost-deps')
    data['image_link']['modpost'] = backend.slice_program(backend.bounded(modpost, 1, high), {'MODPOST', 'modpost-args'})
    data['image_link']['modpost'].extend(backend.selected(modpost, {'cmd_modpost'}))
    # The timestamp object is a separate Bazel action. Remove just its recursive
    # Make dispatch; the native script still performs the native final link.
    link_script = (linux / 'scripts/link-vmlinux.sh').read_text()
    original = '${MAKE} -f "${srctree}/scripts/Makefile.build" obj=init init/version-timestamp.o'
    if link_script.count(original) != 1:
        raise ValueError('Unsupported link-vmlinux.sh: timestamp Make boundary changed')
    link_script = link_script.replace(original, ': # init/version-timestamp.o is a declared Bazel prerequisite')
    exported = {name for n in top_nodes if n['kind'] == 'directive' and n.get('directive') == 'export' for name in n['arguments'].split()}
    referenced = set(re.findall(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)', link_script))
    data['image_common'] = bodies['scripts/Kbuild.include'] + bodies['scripts/Makefile.compiler']
    data['image_link_exports'] = sorted({left or right for left, right in referenced})
    # These helpers run after the local Kbuild file. Do not replay the earlier
    # directory-variable initialization and erase its ccflags/asflags.
    local_include = next(n['line'] for n, _ in walk(build) if n['kind'] == 'directive' and n.get('arguments') == '$(kbuild-file)')
    data['image_object_helpers'] = backend.slice_program(bodies['scripts/Makefile.lib'] + backend.bounded(build, local_include + 1, max(n['line'] for n, _ in walk(build))), {'cmd_objtool', 'objtool-enabled'})
    data['image_cpp'] = [n for n in top_nodes if n.get('name') == 'CPP']
    def implicit_rules(body):
        result = []
        active = False
        for n in body:
            if n['kind'] == 'conditional':
                branches = [dict(b, body=implicit_rules(b['body'])) for b in n['branches']]
                if any(b['body'] for b in branches):
                    result.append(dict(n, branches=branches))
            elif n['kind'] == 'rule':
                active = '%' in n['targets']
                if active:
                    result.append(dict(n, implicit=True))
            elif n['kind'] == 'recipe' and active:
                result.append(n)
            else:
                active = False
        return result
    data['image_implicit_rules'] = implicit_rules(bodies['scripts/Makefile.build']) + implicit_rules(bodies['scripts/Makefile.lib'])
    return syntax, data, link_script
