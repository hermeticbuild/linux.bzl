"""Measure native Kconfig and context-specific Kbuild queries as Bazel actions."""
import concurrent.futures
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
    for start in range(0, len(encoded), 180):
        (output / ('%08d=' % (start // 180) + encoded[start:start + 180])).touch()
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


def compiler_environment(spec, execution_root):
    roles = {name: str(execution_root / path) for name, path in spec['roles'].items()}
    arch = spec['architecture']
    environment = dict(absolute_environment(spec.get('compile_env', {}), execution_root), LC_ALL='C', LANG='C')
    environment.update(spec.get('tool_environment', {}))
    environment.update(roles)
    environment.update(
        srctree=str(Path(spec['source']).resolve()), ARCH=arch['arch'], SRCARCH=arch['srcarch'],
        SUBARCH=arch['subarch'], HEADER_ARCH=arch['header_arch'], KERNELVERSION='prototype',
        HOSTCC=str(execution_root / spec['host_roles']['CC']), RUSTC='false', PAHOLE='false',
        PAHOLE_VERSION='0', PATH=environment.get('PATH', os.environ['PATH']),
        CLANG_FLAGS=shlex.join(absolute_arguments(spec['compile_flags'], execution_root)),
    )
    environment['CC_VERSION_TEXT'] = subprocess.check_output([roles['CC'], '--version'], env=environment, text=True).splitlines()[0]
    return environment


def kconfig(spec, output):
    execution_root = Path.cwd()
    root = Path(spec['source']).resolve()
    baseline = json.loads(Path(spec['baseline']).read_text())
    sources = {key: json.loads(value) if isinstance(value, str) else value for key, value in baseline.items()}
    reachable, pending = set(), ['Kconfig']
    while pending:
        name = pending.pop()
        if name in reachable:
            continue
        reachable.add(name)
        for node in sources[name]:
            if node['kind'] == 'source':
                path = node['path']
                arch = spec['architecture']
                for key, value in {'SRCARCH': arch['srcarch'], 'ARCH': arch['arch'], 'SUBARCH': arch['subarch'], 'HEADER_ARCH': arch['header_arch']}.items():
                    path = path.replace('__KCONFIG_' + key, value)
                pending.append(path)
    environment = compiler_environment(spec, execution_root)
    binary = str(Path(spec['syntax']).resolve())

    def export(name):
        result = subprocess.run([binary, name], cwd=root, env=environment, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError(name + '\n' + result.stderr)
        nodes = json.loads(result.stdout)
        if len(nodes) != len(sources[name]):
            return [{'file': name, 'nodes': nodes}]
        return [{'file': name, 'index': i, 'node': node} for i, node in enumerate(nodes) if node != sources[name][i]]

    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
        patches = [patch for group in pool.map(export, sorted(reachable)) for patch in group]
    host = str(execution_root / spec['host_roles']['CC'])
    host_flags = absolute_arguments(spec['host_compile_flags'], execution_root)
    host_capabilities = {}
    host_environment = dict(environment, **absolute_environment(spec.get('host_compile_env', {}), execution_root))
    for program in json.loads(Path(spec['host_programs']).read_text()):
        command = program.replace('$(pound)', '#').replace('$(HOSTCC)', shlex.quote(host)).replace('$(HOSTCFLAGS)', shlex.join(host_flags)).replace('$(HOSTPKG_CONFIG)', 'pkg-config').replace('$$', '$')
        result = subprocess.run(['/bin/sh', '-c', command], env=host_environment, text=True, capture_output=True, timeout=30)
        if result.returncode not in (0, 1):
            raise RuntimeError(command + '\n' + result.stderr)
        host_capabilities[program] = result.stdout.strip()
    result = subprocess.run([host, *host_flags, '-Werror', '-x', 'c', '-', '-c', '-o', '/dev/null'], input='#include <libelf.h>\nvoid *p = elf_getshdrnum;\n', env=host_environment, text=True, capture_output=True)
    host_capabilities['__libelf_modern_api__'] = result.returncode == 0
    publish(output, dict(patches=patches, capabilities={}, host_capabilities=host_capabilities, history=[], files=len(reachable), compiler=environment['CC_VERSION_TEXT'], toolchain=spec))


def probe_command(request, work, execution_root):
    """Build the native helper command using its complete evaluated context."""
    kind, argument = request['kind'], request['argument']
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
    if kind == 'try-run':
        return ['/bin/sh', '-c', request['command']]
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
            environment.update(LC_ALL='C', TMP=str(work / 'tmp'), TMPDIR=str(work), PATH=environment.get('PATH', os.environ['PATH']))
            result = subprocess.run(command, cwd=directory, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=30, env=environment)
            if result.returncode not in (0, 1):
                raise RuntimeError(f'{key}: unexpected exit {result.returncode}\n{result.stderr}')
            answers[key] = result.returncode == 0
            records.append(dict(key=key, argv=[arg.replace(str(work), '__probe_tmp__') for arg in command], status=result.returncode, stderr=result.stderr.replace(str(work), '__probe_tmp__')))
    previous['capabilities'] = answers
    previous['history'].append(records)
    publish(output, previous)


if __name__ == '__main__':
    inputs = json.loads(Path(sys.argv[2]).read_text())
    (kconfig if sys.argv[1] == 'kconfig' else probes)(inputs, sys.argv[3])
