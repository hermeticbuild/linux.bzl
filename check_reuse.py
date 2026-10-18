#!/usr/bin/env python3
"""Measure real object action reuse, restoring the edited source on every exit."""
import argparse
import json
from pathlib import Path
import subprocess
import uuid


def records(stream):
    decoder = json.JSONDecoder()
    buffer = ''
    read_size = 65536
    while True:
        chunk = stream.read(read_size)
        buffer += chunk
        while buffer.strip():
            buffer = buffer.lstrip()
            try:
                value, end = decoder.raw_decode(buffer)
            except json.JSONDecodeError:
                if not chunk:
                    raise
                read_size = min(read_size * 2, 16 * 1024 * 1024)
                break
            yield value
            buffer = buffer[end:]
            read_size = 65536
        if not chunk:
            return


def executed_objects(decoder, log, inventory):
    paths, executed = {}, set()
    command = [decoder, 'print', '--compact_execution_log=' + str(log), '--raw=true', '--sort=false']
    with subprocess.Popen(command, stdout=subprocess.PIPE, text=True) as process:
        for entry in records(process.stdout):
            for kind in ['file', 'directory', 'unresolvedSymlink']:
                if kind in entry:
                    paths[int(entry['id'])] = entry[kind]['path']
            spawn = entry.get('spawn')
            if not spawn or spawn.get('cacheHit') or not spawn.get('runner'):
                continue
            if spawn.get('mnemonic') != 'KernelArtifact':
                continue
            for output in spawn.get('outputs', []):
                path = paths.get(int(output.get('outputId', 0)), '')
                marker = '/artifacts/'
                if marker in path:
                    relative = path.split(marker, 1)[1]
                    if relative in inventory:
                        executed.add(inventory[relative])
        if process.wait():
            raise RuntimeError('Compact execution log decoder failed')
    return sorted(executed)


def inventory(metadata, artifact_prefix):
    jobs = json.loads((metadata / 'report.json').read_text())
    result = {}
    for job in jobs:
        if job['scan'] is None:
            continue
        for path in job['outputs']:
            if not path.endswith('.o'):
                continue
            relative = path.split('.graph/', 1)[1]
            if '.raw/' in relative:
                result[relative] = relative.split('.raw/', 1)[1]
            elif relative.startswith(artifact_prefix):
                result[relative] = relative[len(artifact_prefix):]
    return result



def affected_targets(graph, changed):
    """Follow actual producer edges, including generated sources after linking."""
    affected = {changed}
    members = graph['members']
    for _ in range(len(members) + 1):
        new = {row['target'] for row in members if affected.intersection(row['dependencies'])}
        if new.issubset(affected):
            return affected
        affected.update(new)
    raise ValueError('Artifact dependency closure did not converge')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--decoder', required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--changed', default='init/main.o')
    parser.add_argument('--control', default='kernel/fork.o')
    parser.add_argument('--target', default='//:kernel')
    parser.add_argument('--metadata', type=Path, default=Path('bazel-bin/kernel.schedule64'))
    parser.add_argument('--artifact-prefix', default='kernel.artifacts/')
    parser.add_argument('--graph', type=Path)
    parser.add_argument('--out', type=Path, default=Path('validation/reuse'))
    parser.add_argument('--bazel-option', action='append', default=[])
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    original = args.source.read_bytes()
    phases = {}
    marker = None
    try:
        for phase in ['baseline', 'unchanged', 'edited']:
            if phase == 'edited':
                if args.source.read_bytes() != original:
                    raise RuntimeError('Source changed concurrently before the reuse edit')
                marker = ('\n/* Bazel reuse check ' + uuid.uuid4().hex + ' */\n').encode()
                args.source.write_bytes(original + marker)
            log = (args.out / (phase + '.execution.pb.zst')).resolve()
            subprocess.run(['bazel', 'build', args.target, '--execution_log_compact_file=' + str(log), *args.bazel_option], check=True)
            objects = inventory(args.metadata, args.artifact_prefix)
            if args.changed not in objects.values() or args.control not in objects.values():
                raise AssertionError('Changed and control objects must both be in the compiler inventory')
            phases[phase] = executed_objects(args.decoder, log, objects)
    finally:
        if marker is not None:
            current = args.source.read_bytes()
            if marker in current:
                args.source.write_bytes(current.replace(marker, b"", 1))
    graph = args.graph or (Path('bazel-bin/kernel.tree/kernel.json') if args.target == '//:kernel' else None)
    allowed = affected_targets(json.loads(graph.read_text()), args.changed) if graph else {args.changed}
    if args.control in allowed:
        raise AssertionError('The control must be independent of the changed object')
    unrelated = [name for name in phases['edited'] if name not in allowed]
    result = dict(inventory=len(set(objects.values())), executed=phases, unrelated=unrelated, affected_compilers=sorted(allowed.intersection(objects.values())),
                  passed=not phases['unchanged'] and args.changed in phases['edited'] and not unrelated)
    (args.out / 'summary.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))
    if not result['passed']:
        raise SystemExit('Object actions did not preserve selective source reuse')


if __name__ == '__main__':
    main()
