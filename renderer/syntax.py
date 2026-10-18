"""Shared traversal for unevaluated native Make syntax."""
from pathlib import Path

def relative(path, root):
    return Path(path).resolve().relative_to(root).as_posix()


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
