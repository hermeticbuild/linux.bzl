#!/usr/bin/env python3
"""Render GNU Make syntax AST as unevaluated Bazel Kbuild data targets."""
import argparse
from collections import defaultdict
import json
from pathlib import Path, PurePosixPath
import re
import shutil

HERE = Path(__file__).resolve().parent
CONSTRUCTORS = {'assignment': 'assignment', 'define': 'define', 'conditional': 'conditional',
                'rule': 'make_rule', 'target_assignment': 'target_assignment', 'directive': 'directive',
                'recipe': 'recipe', 'expression': 'expression', 'undefine': 'undefine'}


def literal(value):
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, bool):
        return str(value)
    if value is None:
        return 'None'
    if isinstance(value, (list, tuple)):
        return '[' + ', '.join(literal(v) for v in value) + ']'
    if isinstance(value, dict):
        return '{' + ', '.join(literal(k) + ': ' + literal(v) for k, v in value.items()) + '}'
    return str(value)


def render_value(value, helpers):
    if isinstance(value, dict) and value.get('kind') in CONSTRUCTORS:
        helper = CONSTRUCTORS[value['kind']]
        helpers.add(helper)
        return helper + '(' + ', '.join(k + ' = ' + render_value(v, helpers)
                                       for k, v in value.items() if k not in ('kind', 'file')) + ')'
    if isinstance(value, list):
        return '[' + ', '.join(render_value(v, helpers) for v in value) + ']'
    if isinstance(value, dict):
        return '{' + ', '.join(literal(k) + ': ' + render_value(v, helpers) for k, v in value.items()) + '}'
    return literal(value)


def call(kind, name, **attrs):
    return kind + '(\n    name = ' + literal(name) + ',\n' + ''.join(
        '    ' + key + ' = ' + literal(value).replace('\n', '\n    ') + ',\n'
        for key, value in attrs.items() if value is not None) + ')\n'


def relative(path, root):
    return Path(path).resolve().relative_to(root).as_posix()


def package(directory):
    return 'tree' if directory == '.' else 'tree/' + directory


def file_name(path):
    # Hex encoding avoids collisions such as Makefile.foo versus Makefile_foo.
    name = Path(path).name
    return {'Kbuild': 'kbuild', 'Makefile': 'makefile'}.get(name, '_make_' + name.encode().hex())


def file_label(path):
    return '//' + package(PurePosixPath(path).parent.as_posix()) + ':' + file_name(path)


def object_label(path):
    p = PurePosixPath(path)
    return '//' + package(p.parent.as_posix()) + ':' + p.name


def clean_path(base, token):
    if token.startswith(('/', '-')) or not re.fullmatch(r'[A-Za-z0-9_./+-]+', token):
        return None
    parts = PurePosixPath(base, token).parts
    if '..' in parts:
        return None
    return PurePosixPath(*parts).as_posix()


def walk(statements, guards=()):
    for node in statements:
        if node['kind'] == 'conditional':
            previous = []
            for branch in node['branches']:
                guard = {'directive': branch['directive'], 'arguments': branch['arguments'],
                         'previous_branches': list(previous)}
                yield from walk(branch['body'], guards + (guard,))
                previous.append({'directive': branch['directive'], 'arguments': branch['arguments']})
        else:
            yield node, guards


def generate(tree, linux, out):
    if tree.get('format') != 'gnu-make-syntax-v1':
        raise ValueError('Expected gnu-make-syntax-v1 AST')
    linux, out = linux.resolve(), out.resolve()
    for anchor in ('Makefile', 'Kbuild'):
        if (linux / anchor).is_file():
            linux = (linux / anchor).resolve().parent
            break
    if out == linux or linux in out.parents or out in linux.parents:
        raise ValueError('Output must be outside the Linux source tree')
    marker = out / '.make-ast-targets'
    if out.exists() and (out.is_symlink() or not marker.is_file()):
        raise ValueError(f'Refusing to replace unmarked output directory: {out}')
    # Validate input paths before writing anything.
    files = {relative(f['file'], linux): f for f in tree['files']}
    objects, directories, diagnostics = {}, {}, []
    packages = defaultdict(list)
    for path, source in sorted(files.items()):
        base = PurePosixPath(path).parent.as_posix()
        owner = package(base)
        helpers = {'kbuild_file'}
        statements = [render_value(n, helpers) for n in source['body']]
        declaration = ('kbuild_file(\n    name = ' + literal(file_name(path)) + ',\n'
                       '    path = ' + literal(path) + ',\n    src = ' + literal('//:source/' + path) + ',\n'
                       '    statements = [\n' + ''.join('        ' + s + ',\n' for s in statements) + '    ],\n)\n')
        packages[owner].append((helpers, declaration))
        for node, guards in walk(source['body']):
            if node['kind'] != 'assignment':
                if node['kind'] in ('expression', 'directive', 'rule', 'target_assignment', 'define'):
                    diagnostics.append({'file': path, 'line': node['line'], 'kind': node['kind'],
                                        'reason': 'Preserved as syntax; not lowered to an object/directory edge'})
                continue
            name, value = node['name'], node['value']
            occurrence = {'file': path, 'line': node['line'], 'variable': name,
                          'flavor': node['flavor'], 'guards': list(guards)}
            # Discover literal paths only; all dynamic values remain in the file AST.
            tokens = node.get('words', [] if '$' in value else value.split())
            if '$' in value:
                diagnostics.append({'file': path, 'line': node['line'], 'kind': 'dynamic_value',
                                    'reason': 'Make expression preserved', 'variable': name, 'value': value})
            members = []
            for token in tokens:
                candidate = clean_path(base, token)
                if candidate is None:
                    continue
                if token.endswith('.o'):
                    entry = objects.setdefault(candidate, {'definitions': set(), 'occurrences': [], 'members': set()})
                    entry['definitions'].add(file_label(path))
                    entry['occurrences'].append(occurrence)
                    members.append(candidate)
                elif token.endswith('/'):
                    # Only Kbuild directory-list assignments create descent edges.
                    if not (name.startswith(('obj-', 'subdir-'))):
                        continue
                    entry = directories.setdefault(candidate, {'definitions': set(), 'occurrences': []})
                    entry['definitions'].add(file_label(path))
                    entry['occurrences'].append(occurrence)
            match = re.fullmatch(r'(.+)-(?:y|m|objs|\$\(CONFIG_[A-Za-z0-9_]+\))', name)
            if match and match[1] not in ('obj', 'lib', 'subdir', 'always', 'extra', 'targets') and members:
                composite = clean_path(base, match[1] + '.o')
                if composite:
                    entry = objects.setdefault(composite, {'definitions': set(), 'occurrences': [], 'members': set()})
                    entry['definitions'].add(file_label(path))
                    entry['occurrences'].append(occurrence)
                    entry['members'].update(object_label(member) for member in members)
    for path, data in sorted(objects.items()):
        p = PurePosixPath(path)
        sources = [str(p.with_suffix(suffix)) for suffix in ('.c', '.S', '.s') if (linux / p.with_suffix(suffix)).is_file()]
        declaration = call('kbuild_object', p.name, path=path, definitions=sorted(data['definitions']),
                           srcs=['//:source/' + src for src in sources], members=sorted(data['members']),
                           occurrences=data['occurrences'])
        packages[package(p.parent.as_posix())].append(({'kbuild_object'}, declaration))
        if not sources and not data['members']:
            diagnostics.append({'object': path, 'reason': 'No source/composite members found; may be generated or dynamic'})
    for path, data in sorted(directories.items()):
        p = PurePosixPath(path)
        child = next((file_label(str(p / name)) for name in ('Kbuild', 'Makefile') if str(p / name) in files), None)
        declaration = call('kbuild_directory', '_dir_' + p.name, path=path,
                           definitions=sorted(data['definitions']), child=child, occurrences=data['occurrences'])
        packages[package(p.parent.as_posix())].append(({'kbuild_directory'}, declaration))
        if child is None:
            diagnostics.append({'directory': path, 'reason': 'Child Kbuild/Makefile absent from AST'})
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)
    marker.write_text('Generated by kernel-repository\n')
    (out / 'MODULE.bazel').write_text('module(name = "kbuild_ast_targets")\n')
    (out / '.bazelversion').write_text('9.1.0\n')
    (out / 'source').symlink_to(linux, target_is_directory=True)
    (out / 'rules').mkdir()
    shutil.copyfile(HERE / 'kbuild.bzl', out / 'rules/kbuild.bzl')
    (out / 'rules/BUILD.bazel').write_text('exports_files(["kbuild.bzl"])\n')
    for owner, declarations in sorted(packages.items()):
        directory = out / owner
        directory.mkdir(parents=True)
        helpers = sorted(set().union(*(h for h, _ in declarations)))
        header = 'load("//rules:kbuild.bzl", ' + ', '.join(literal(h) for h in helpers) + ')\n'
        (directory / 'BUILD.bazel').write_text(header + 'package(default_visibility = ["//visibility:public"])\n\n'
                                               + '\n'.join(text for _, text in declarations))
    directory_labels = ['//' + package(PurePosixPath(p).parent.as_posix()) + ':_dir_' + PurePosixPath(p).name
                        for p in sorted(directories)]
    (out / 'BUILD.bazel').write_text('load("//rules:kbuild.bzl", "kbuild_graph")\n'
        'exports_files(glob(["source/**"], exclude=["source/.git/**"]))\n' +
        call('kbuild_graph', 'graph', makefiles=[file_label(p) for p in sorted(files)],
             objects=[object_label(p) for p in sorted(objects)], directories=directory_labels))
    report = {'makefiles': len(files), 'objects': len(objects), 'directories': len(directories),
              'packages': len(packages), 'diagnostics': diagnostics}
    (out / 'generation.json').write_text(json.dumps(report, indent=2) + '\n')
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--ast', type=Path, required=True)
    parser.add_argument('--linux', type=Path, default=Path('_tmp/linux'))
    parser.add_argument('--out', type=Path, default=Path('_tmp/kbuild-ast-targets/repo'))
    args = parser.parse_args()
    report = generate(json.loads(args.ast.read_text()), args.linux, args.out)
    print(json.dumps({k: v for k, v in report.items() if k != 'diagnostics'}))
    print(f'{len(report["diagnostics"])} diagnostics; workspace: {args.out.resolve()}')
