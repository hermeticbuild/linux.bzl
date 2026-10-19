"""Measure context-specific native Kbuild queries as Bazel actions."""
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile


def publish(output, value):
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    encoded = json.dumps(value, ensure_ascii=True, separators=(',', ':')).encode().hex()
    print(f'Payload: {len(encoded) // 2} JSON bytes; {(len(encoded) + 179) // 180} chunks', file=sys.stderr, flush=True)
    for start in range(0, len(encoded), 180):
        with (output / ('%08d=' % (start // 180) + encoded[start:start + 180])).open('xb'):
            pass
    (output / 'report.json').write_text(json.dumps(value, indent=2))


def absolute_arguments(arguments, execution_root):
    """Preserve provider argument order while relocating declared tool paths."""
    result = []
    path_value = False
    for argument in arguments:
        if path_value and argument == "-Xclang":
            result.append(argument)
        elif path_value:
            result.append(str(execution_root / argument) if not os.path.isabs(argument) else argument)
            path_value = False
        elif argument in ('-I', '-isystem', '-iquote', '-include', '-imacros', '-resource-dir', '--sysroot', '-B', '-internal-isystem', '-internal-externc-isystem', '-internal-iframework'):
            result.append(argument)
            path_value = True
        elif argument.startswith(('-I', '-B')) and len(argument) > 2:
            result.append(argument[:2] + str(execution_root / argument[2:]))
        elif argument.startswith(('-resource-dir=', '--sysroot=')):
            name, value = argument.split('=', 1)
            result.append(name + '=' + str(execution_root / value))
        else:
            result.append(argument)
    return result


def absolute_environment(environment, execution_root):
    result = dict(environment)
    for name in ['PATH', 'LD_LIBRARY_PATH', 'DYLD_LIBRARY_PATH', 'LIBRARY_PATH', 'COMPILER_PATH', 'CPATH', 'C_INCLUDE_PATH']:
        if name in result:
            result[name] = ':'.join(str(execution_root / path) if path and not os.path.isabs(path) else path for path in result[name].split(':'))
    return result


def probe_command(request, work, execution_root):
    """Build the native helper command using its complete evaluated context."""
    kind = request['kind']
    if kind in {'try-run', 'host-shell'}:
        return [str(execution_root / request['environment']['CONFIG_SHELL']), '-c', request['command']]
    argument = request['argument']
    compiler = shlex.split(request['compiler'])
    compiler[0] = str(execution_root / compiler[0])
    flags = shlex.split(request['flags'])
    source, obj = work / 'test.c', work / 'test.o'
    source.write_text('')
    if kind == 'ld-option':
        return [*compiler, *flags, *shlex.split(argument), '-v']
    if kind == 'as-instr':
        source = work / 'test.S'
        source.write_text(argument.replace('\\n', '\n').replace('\\$', '$') + '\n')
        return [*compiler, '-Werror', *flags, '-Wa,--fatal-warnings', '-c', '-x', 'assembler-with-cpp', str(source), '-o', str(obj)]
    if kind == 'as-option':
        return [*compiler, '-Werror', *flags, *shlex.split(argument), '-c', '-x', 'assembler-with-cpp', '/dev/null', '-o', str(obj)]
    if kind.startswith('rustc'):
        raise RuntimeError('Rust probes require a declared Rust toolchain')
    options = ['-Wno-' + argument] if kind == 'cc-disable-warning' else shlex.split(argument)
    options = [flag.replace('-Wno-', '-W', 1) if flag.startswith('-Wno-') else flag for flag in options]
    return [*compiler, '-Werror', *flags, *options, '-c', '-x', 'c', '/dev/null', '-o', str(obj)]


def probes(spec, output):
    previous = json.loads((Path(spec['previous']) / 'report.json').read_text())
    if not spec['requests']:
        previous['converged'] = True
        publish(output, previous)
        return
    answers, records = dict(previous['capabilities']), []
    execution_root = Path.cwd()
    for key, request in sorted(spec['requests'].items()):
        with tempfile.TemporaryDirectory() as temporary:
            work = Path(temporary)
            logical_directory = request['directory'].removeprefix('__object__').lstrip('/')
            if Path(request['directory']).is_absolute() or '..' in Path(logical_directory).parts:
                raise ValueError('Probe working directory escapes the object tree: ' + request['directory'])
            directory = work / 'object' / logical_directory
            directory.mkdir(parents=True)
            for namespace in ('external', 'bazel-out'):
                if (execution_root / namespace).exists():
                    (directory / namespace).symlink_to(execution_root / namespace)
            (directory / '__source__').symlink_to(execution_root / request['source'])
            (directory / '__object__').symlink_to(directory)
            (directory / 'include/generated').mkdir(parents=True)
            (directory / 'include/generated/autoconf.h').write_text(spec.get('header', ''))
            (directory / '.config').write_text(spec.get('config', ''))
            command = probe_command(request, work, execution_root)
            environment = absolute_environment(request['environment'], execution_root)
            infrastructure_error = work / 'infrastructure-error'
            environment.update(LC_ALL='C', TMP=str(work / 'tmp'), TMPDIR=str(work), KBUILD_EXEC_ROOT=str(execution_root), KBUILD_PROBE_ERROR=str(infrastructure_error))
            result = subprocess.run(command, cwd=directory, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=30, env=environment)
            if infrastructure_error.exists():
                raise RuntimeError('Kbuild probe infrastructure failed: ' + infrastructure_error.read_text())
            if request['kind'] != 'host-shell' and result.returncode not in (0, 1):
                raise RuntimeError(f'{key}: unexpected exit {result.returncode}\n{result.stderr}')
            answers[key] = result.stdout.rstrip('\n').replace('\n', ' ') if request['kind'] == 'host-shell' else result.returncode == 0
            records.append(dict(key=key, argv=[arg.replace(str(work), '__probe_tmp__') for arg in command], status=result.returncode, stdout=result.stdout.replace(str(work), '__probe_tmp__'), stderr=result.stderr.replace(str(work), '__probe_tmp__')))
    previous['capabilities'] = answers
    previous['history'].append(records)
    publish(output, previous)


if __name__ == '__main__':
    inputs = json.loads(Path(sys.argv[2]).read_text())
    if sys.argv[1] == 'seed':
        publish(sys.argv[3], dict(capabilities={}, history=[]))
    elif sys.argv[1] == 'kbuild':
        probes(inputs, sys.argv[3])
    else:
        raise ValueError(f'Unsupported probe operation: {sys.argv[1]!r}')
