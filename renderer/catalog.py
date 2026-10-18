#!/usr/bin/env python3
"""Render the symbolic source catalogue and candidate per-object targets.

The execution-time runtime evaluates the preserved Make programs;
GNU Make is used only by the generation frontend's native syntax exporter.
"""
from collections import defaultdict
import json
import posixpath
from pathlib import PurePosixPath

from syntax import relative, walk


def static_expression(terms, environment):
    if terms is None:
        return None
    result = []
    for term in terms:
        if term['op'] == 'literal':
            result.append(term['value'])
        elif term['op'] == 'reference':
            name = static_expression(term['name'], environment)
            value = environment.get(name)
            if value is None:
                return None
            result.append(value)
        else:
            return None
    return ''.join(result)


def artifact_path(value):
    value = posixpath.normpath(value)
    if value.startswith(('/', '../')) or any(c in value for c in '$%:;|*?[]\\'):
        return None
    return value


def catalog(tree, linux, arch):
    actions, prerequisites, diagnostics = {}, defaultdict(set), []
    # Lists of references to artifact outputs, never evaluated commands.
    def add(path, directory, outs=None, deps=(), kind='generate', headers=False, driver='build'):
        if path in actions:
            return
        actions[path] = {'target': path, 'directory': directory, 'outs': outs or [path],
                         'deps': set(deps), 'kind': kind, 'needs_headers': headers, 'driver': driver}

    for source in tree['files']:
        filename = relative(source['file'], linux)
        if PurePosixPath(filename).name not in ('Kbuild', 'Makefile'):
            continue
        if filename.startswith('arch/') and PurePosixPath(filename).parts[1] != arch:
            continue
        directory = str(PurePosixPath(filename).parent)
        # Kbuild takes precedence. Other source files are still in the full AST.
        if filename.endswith('/Makefile') and (linux / directory / 'Kbuild').exists():
            continue
        env = {'obj': directory, 'objtree': '.', 'src': '__source__/' + directory,
               'srctree': '__source__', 'srcroot': '__source__', 'SRCARCH': arch,
               'ARCH': arch, 'KCONFIG_CONFIG': '.config'}
        nodes = list(walk(source['body']))
        hostprogs = set()
        # Resolve only simple path/literal assignments; no CONFIG or Make function
        # execution here. Reassignments/conditionals that differ become unknown.
        assigned = {}
        for node, guards in nodes:
            if node['kind'] != 'assignment':
                continue
            name = static_expression(node.get('name_expression'), env)
            value = static_expression(node.get('value_expression'), env)
            if name and name not in env:
                if not guards and node['flavor'] in ('simple', 'recursive', 'conditional'):
                    env[name] = value
                elif not guards and node['flavor'] == 'append' and env.get(name) is not None and value is not None:
                    env[name] += ' ' + value
            if name:
                assigned.setdefault(name, []).append((node, guards))
            if node['name'].startswith('hostprogs'):
                for token in node.get('words', []):
                    if artifact_path(token) and '/' not in token and '$' not in token:
                        hostprogs.add(token)
        # Host tools: native Makefile.host determines flags and command syntax.
        for host in hostprogs:
            path = posixpath.normpath(directory + '/' + host)
            members = []
            dynamic = False
            for suffix, extension in (('-objs', '.c'), ('-cxxobjs', '.cc')):
                for node, guards in assigned.get(host + suffix, []):
                    if guards or '$' in node['value'] or node['flavor'] not in ('simple', 'recursive', 'append'):
                        dynamic = True
                        continue
                    for obj in node.get('words', node['value'].split()):
                        member = artifact_path(directory + '/' + obj)
                        if member is None or not obj.endswith('.o'):
                            dynamic = True
                            continue
                        add(member, directory, deps=['scripts/basic/fixdep'], kind='host')
                        members.append(member)
            if dynamic or host + '-rust' in assigned:
                diagnostics.append({'target': path, 'reason': 'Dynamic/Rust host tool graph not lowered'})
                continue
            if not members and not (linux / (path + '.c')).is_file():
                diagnostics.append({'target': path, 'reason': 'No single C source or concrete host object list'})
                continue
            add(path, directory, deps=members or ([] if path == 'scripts/basic/fixdep' else ['scripts/basic/fixdep']), kind='host')
        # Rules: native expression nodes expose paths, native Make remains the
        # authority for pattern rules, target variables, recipe ownership, etc.
        current = None
        for node, guards in nodes:
            if node['kind'] == 'rule':
                targets = static_expression(node.get('targets_expression'), env)
                tail = static_expression(node.get('tail_expression'), env)
                current = None
                if targets is None or tail is None or any(c in targets + tail for c in '%;:|*?[]'):
                    diagnostics.append({'file': filename, 'line': node['line'],
                                        'reason': 'Dynamic/pattern/static-pattern/inline rule retained in metadata'})
                    continue
                target_paths = [artifact_path(t) for t in targets.split()]
                deps = [artifact_path(d) for d in tail.split() if d != 'FORCE']
                if None in target_paths or None in deps:
                    continue
                for target in target_paths:
                    prerequisites[target].update(deps)
                current = (target_paths, node['separator'] in ('&:', '&::'))
            elif node['kind'] == 'recipe' and current:
                # Multiple ordinary targets do not imply a shared output set.
                # Only '&:' groups guarantee shared outputs; native special
                # multi-output pattern generators are instantiated below.
                target_paths, grouped = current
                for target in target_paths[:1] if grouped else target_paths:
                    if '/' in target and not target.startswith('__source__/') and target not in (directory + '/',):
                        add(target, directory, outs=target_paths if grouped else None)
            elif node['kind'] not in ('recipe', 'target_assignment'):
                current = None
        # Instantiate kernel-provided multi-output patterns from actual inputs.
        for source_path in sorted((linux / directory).glob('*.asn1')):
            stem = source_path.relative_to(linux).as_posix()
            add(stem + '.c', directory, outs=[stem + '.c', stem + '.h'], deps=['scripts/asn1_compiler'])
        for source_path in sorted((linux / directory).glob('*.y')):
            stem = source_path.relative_to(linux).with_suffix('.tab').as_posix()
            add(stem + '.c', directory, outs=[stem + '.c', stem + '.h'])
        for source_path in sorted((linux / directory).glob('*.l')):
            stem = source_path.relative_to(linux).with_suffix('.lex').as_posix()
            add(stem + '.c', directory)

    # Generic header wrappers each have an independent native recipe/action.
    bodies = {relative(f['file'], linux): f['body'] for f in tree['files']}
    common_headers = []
    for uapi in ('', 'uapi/'):
        generic = 'include/' + uapi + 'asm-generic'
        arch_src = 'arch/' + arch + '/include/' + uapi + 'asm'
        arch_out = 'arch/' + arch + '/include/generated/' + uapi + 'asm'
        def header_list(path, variable):
            return {word for n, _ in walk(bodies.get(path, []))
                    if n['kind'] == 'assignment' and n['name'] == variable
                    for word in n.get('words', []) if '$' not in word}
        generated = header_list(arch_src + '/Kbuild', 'generated-y')
        headers = header_list(arch_src + '/Kbuild', 'generic-y')
        headers |= {h for h in header_list(generic + '/Kbuild', 'mandatory-y')
                    if h not in generated and not (linux / arch_src / h).is_file()}
        for header in sorted(headers):
            path = arch_out + '/' + header
            add(path, arch_out, driver='asm_headers')
            common_headers.append(path)
    common_headers.extend(path for path in actions if path.startswith('arch/' + arch + '/include/generated/') and
                          ('/unistd_' in path or path.startswith('arch/' + arch + '/include/generated/asm/syscalls_')))
    # Architecture-level generated headers live in arch/Makefile, not Kbuild.
    for path in ('arch/' + arch + '/include/generated/asm/cpufeaturemasks.h',):
        if arch == 'x86':
            add(path, '.', driver='arch')
            common_headers.append(path)
    # Compile implicit intermediate objects/assembly required by generators.
    # This discovers modpost's empty.o and devicetable-offsets.s, as well as
    # bounds.s and asm-offsets.s, without running a configured Make at generation.
    for target, deps in list(prerequisites.items()):
        for dep in deps:
            if dep.endswith(('.o', '.s')) and (linux / PurePosixPath(dep).with_suffix('.c')).is_file():
                owner = str(PurePosixPath(dep).parent)
                if dep in ('kernel/bounds.s', 'arch/' + arch + '/kernel/asm-offsets.s', 'kernel/sched/rq-offsets.s'):
                    owner = '.'
                add(dep, owner, deps=['scripts/basic/fixdep'], kind='compile', headers=True)
    producers = {out: target for target, action in actions.items() for out in action['outs']}
    # Explicit header/source edges on .o rules connect consumers to generators.
    for target, deps in list(prerequisites.items()):
        if target.endswith('.o') and any(dep in producers for dep in deps):
            source = linux / PurePosixPath(target).with_suffix('.c')
            generated_c = str(PurePosixPath(target).with_suffix('.c'))
            if source.is_file() or generated_c in producers:
                add(target, str(PurePosixPath(target).parent), deps=['scripts/basic/fixdep'], kind='compile', headers=True)
                producers[target] = target
                if generated_c in producers:
                    prerequisites[target].add(generated_c)
    for target, action in actions.items():
        if action['needs_headers']:
            action['deps'].update(common_headers)
            if target != 'kernel/bounds.s':
                action['deps'].update(['include/generated/bounds.h', 'include/generated/timeconst.h'])
            if target not in ('kernel/bounds.s', 'arch/' + arch + '/kernel/asm-offsets.s'):
                action['deps'].add('include/generated/asm-offsets.h')
            if target not in ('kernel/bounds.s', 'arch/' + arch + '/kernel/asm-offsets.s', 'kernel/sched/rq-offsets.s'):
                action['deps'].add('include/generated/rq-offsets.h')
    blocked = {}
    for target, action in actions.items():
        for output in action['outs']:
            for dep in prerequisites[output]:
                if dep in ('.config', 'include/config/auto.conf') or dep.startswith('__source__/') or (linux / dep).is_file():
                    continue
                if dep in producers and producers[dep] != target:
                    action['deps'].add(producers[dep])
                elif dep not in action['outs']:
                    blocked.setdefault(target, []).append(dep)
        for dep in list(action['deps']):
            if dep not in actions:
                blocked.setdefault(target, []).append(dep)
    # Reject unresolved chains and cycles rather than falling back to make-all.
    visiting, done = set(), set()
    def check(target):
        if target in done:
            return target not in blocked
        if target in visiting:
            blocked.setdefault(target, []).append('dependency cycle')
            return False
        visiting.add(target)
        for dep in actions[target]['deps']:
            if dep not in actions or not check(dep):
                blocked.setdefault(target, []).append(dep)
        visiting.remove(target)
        done.add(target)
        return target not in blocked
    for target in actions:
        check(target)
    for target, deps in blocked.items():
        diagnostics.append({'target': target, 'reason': 'Unresolved prerequisite chain', 'prerequisites': sorted(set(deps))})
    return {k: v for k, v in actions.items() if k not in blocked}, diagnostics
