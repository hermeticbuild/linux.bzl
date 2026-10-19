"""Shared traversal for unevaluated native Make syntax."""
from pathlib import Path

def relative(path, root):
    return Path(path).resolve().relative_to(root).as_posix()


def runtime_program(value: object, root: Path) -> object:
    """Retain executable syntax and source-relative diagnostic provenance."""
    if isinstance(value, dict):
        result = {}
        for key, child in value.items():
            if 'kind' in value and key in {'text', 'words', 'end'}:
                continue
            if key == 'file' and isinstance(child, str) and Path(child).is_absolute():
                result[key] = Path(child).relative_to(root).as_posix()
            else:
                result[key] = runtime_program(child, root)
        return result
    if isinstance(value, list):
        return [runtime_program(child, root) for child in value]
    return value


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
