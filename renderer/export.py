#!/usr/bin/env python3
"""Turn the fork's native parse events into an unevaluated statement tree."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile


def parse_events(events):
    files, current, stack = [], None, []
    for event in events:
        kind = event['kind']
        if kind == 'file_begin':
            if current is not None:
                raise ValueError('Nested file_begin')
            current = {'kind': 'file', 'file': event['file'], 'body': []}
            body = current['body']
            continue
        if kind == 'file_end':
            if stack:
                raise ValueError(f"Unclosed conditional at {stack[-1][1]['file']}:{stack[-1][1]['line']}")
            files.append(current)
            current = None
            continue
        if current is None:
            raise ValueError('Statement outside a file')
        if kind == 'directive':
            directive = event['directive']
            if directive in ('ifdef', 'ifndef', 'ifeq', 'ifneq'):
                node = {'kind': 'conditional', 'file': event['file'], 'line': event['line'],
                        'branches': [dict(event, body=[])]}
                body.append(node)
                stack.append((body, node))
                body = node['branches'][0]['body']
                continue
            if directive == 'else':
                if not stack:
                    raise ValueError(f"Unexpected else at {event['file']}:{event['line']}")
                node = stack[-1][1]
                if node['branches'][-1]['directive'] == 'else' and not node['branches'][-1]['arguments']:
                    raise ValueError(f"Duplicate else at {event['file']}:{event['line']}")
                node['branches'].append(dict(event, body=[]))
                body = node['branches'][-1]['body']
                continue
            if directive == 'endif':
                if not stack:
                    raise ValueError(f"Unexpected endif at {event['file']}:{event['line']}")
                body, node = stack.pop()
                node['end'] = event
                continue
        body.append(event)
    if current is not None:
        raise ValueError('Missing file_end')
    return {'format': 'gnu-make-syntax-v1', 'make_version': '4.4.1', 'files': files}


def native_events(make, paths, expressions=False, variables=None):
    env = {'LC_ALL': 'C', 'PATH': ''}
    with tempfile.TemporaryDirectory(prefix='make-ast-') as directory:
        output = Path(directory) / 'events.jsonl'
        env['LINUX_BZL_MAKE_AST'] = str(output)
        if expressions:
            env['LINUX_BZL_MAKE_EXPRESSIONS'] = '1'
        # Batch native invocations to stay below the system's argument limit.
        events = []
        for start in range(0, len(paths), 128):
            command = [str(make.resolve()), '_make', '-rR', '--no-print-directory']
            for path in paths[start:start + 128]:
                command.extend(['-f', str(path.resolve())])
            command.extend(name + '=' + value for name, value in (variables or {}).items())
            result = subprocess.run(command, env=env, capture_output=True, text=True)
            if result.returncode:
                raise RuntimeError(f'GNU Make export failed:\n{result.stderr}')
            events.extend(json.loads(line) for line in output.read_text().splitlines())
        return events


def render_file(node):
    """Replay exported syntax for differential tests, without adding semantics."""
    lines = []
    def emit(body):
        for statement in body:
            if statement['kind'] == 'conditional':
                for branch in statement['branches']:
                    lines.append(branch['text'])
                    emit(branch['body'])
                lines.append(statement['end']['text'])
            elif statement['kind'] == 'define':
                operator = {'simple': ':=', 'recursive': '=', 'expand': ':::=', 'append': '+=',
                            'conditional': '?=', 'shell': '!='}[statement['flavor']]
                modifiers = ('override ' if statement['override'] else '') + ('private ' if statement['private'] else '')
                if statement['export'] == 1:
                    modifiers += 'export '
                elif statement['export'] == 2:
                    modifiers += 'unexport '
                lines.extend([modifiers + 'define ' + statement['name'] + ' ' + operator,
                              statement['value'], 'endef'])
            else:
                lines.append(statement['text'])
    emit(node['body'])
    return '\n'.join(lines) + '\n'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--make', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--events-out', type=Path)
    parser.add_argument('--expressions', action='store_true', help='Export native expression operations; CONFIG_* references stay symbolic')
    parser.add_argument('--var', action='append', default=[], metavar='NAME=VALUE', help='Known Make input; CONFIG_* values are never substituted')
    parser.add_argument('--linux', type=Path, help='Export every Makefile/Kbuild/.mk file in this checkout')
    parser.add_argument('files', type=Path, nargs='*')
    args = parser.parse_args()
    paths = list(args.files)
    if args.linux:
        paths.extend(p for p in args.linux.rglob('*') if p.is_file() and '.git' not in p.parts and
                     (p.name in ('Kbuild', 'Makefile') or p.name.startswith('Makefile.') or p.suffix == '.mk'))
    paths = sorted(set(p.resolve() for p in paths))
    if not paths:
        parser.error('Supply files or --linux')
    variables = dict(value.split('=', 1) for value in args.var)
    if any(not name.isidentifier() for name in variables):
        parser.error('--var requires plain variable names')
    events = native_events(args.make, paths, args.expressions, variables)
    tree = parse_events(events)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(tree, indent=2) + '\n')
    if args.events_out:
        args.events_out.write_text(''.join(json.dumps(e) + '\n' for e in events))
    print(f'{len(paths)} files; {len(events)} native parse events -> {args.out}')


if __name__ == '__main__':
    main()
