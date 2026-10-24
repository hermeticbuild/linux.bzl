"""Compiler probes retain native helper context, including diagnostic behavior."""
import json
import argparse
from pathlib import Path
import subprocess
import tempfile
import unittest
from urllib.parse import unquote
import sys

parser = argparse.ArgumentParser()
parser.add_argument('--compiler', required=True, type=Path)
parser.add_argument('--shell', required=True, type=Path)
tools, remaining = parser.parse_known_args()
COMPILER, SHELL = str(tools.compiler.absolute()), str(tools.shell.absolute())
sys.argv[1:] = remaining

from probe_runtime import absolute_arguments, logical_probe_output, probe_command, probes, publish_answers, stage_output_directories


class ProbeTest(unittest.TestCase):
    def request(self, flags='', kind='cc-option', argument='-fno-common'):
        return dict(kind=kind, argument=argument, flags=flags, compiler=COMPILER, command='', environment={'CONFIG_SHELL': SHELL}, directory='.', source='.')

    def test_accumulated_flags_change_the_actual_answer(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            valid = self.request(flags='-DSELECTED=1')
            invalid = self.request(flags='-fthis-flag-must-not-exist')
            self.assertEqual(subprocess.run(probe_command(valid, work, Path.cwd()), capture_output=True).returncode, 0)
            self.assertNotEqual(subprocess.run(probe_command(invalid, work, Path.cwd()), capture_output=True).returncode, 0)

    def test_kernel_flags_cannot_be_replayed_into_early_m16_probe(self):
        with tempfile.TemporaryDirectory() as temporary:
            work = Path(temporary)
            early = subprocess.run(probe_command(self.request(flags='-fno-PIE', argument='-m16'), work, Path.cwd()), capture_output=True, text=True)
            late = subprocess.run(probe_command(self.request(flags='-fno-PIE -m64 -mcmodel=kernel', argument='-m16'), work, Path.cwd()), capture_output=True, text=True)
            self.assertEqual(early.returncode, 0, early.stderr)
            self.assertNotEqual(late.returncode, 0, late.stderr)
            self.assertIn("unsupported argument 'kernel'", late.stderr)

    def test_native_script_compiler_environment_preserves_target(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'kallsyms.o'
            command = 'export CC="$1"; shift; ${CC} -x assembler -c /dev/null -o "$1"'
            result = subprocess.run([
                SHELL, '-c', command, 'kallsyms',
                f'{COMPILER} -target aarch64-linux-musl', str(output),
            ], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            header = output.read_bytes()[:20]
            self.assertEqual(header[:6], b'\x7fELF\x02\x01')
            self.assertEqual(int.from_bytes(header[18:20], 'little'), 183)

    def test_native_flag_order_and_negative_warning_check(self):
        with tempfile.TemporaryDirectory() as tmp:
            command = probe_command(self.request('-m64 -DVALUE=1', argument='-Wno-this-warning-must-not-exist'), Path(tmp), Path.cwd())
            self.assertLess(command.index('-m64'), command.index('-DVALUE=1'))
            self.assertIn('-Wthis-warning-must-not-exist', command)
            self.assertNotIn('-Wno-this-warning-must-not-exist', command)

    def test_assembler_and_linker_receive_accumulated_flags(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            assembly = probe_command(self.request('-m32', 'as-instr', 'nop'), work, Path.cwd())
            self.assertIn('-m32', assembly)
            self.assertIn('assembler-with-cpp', assembly)
            self.assertIn('-Wa,--fatal-warnings', assembly)
            linker = probe_command(self.request('-m elf_i386', 'ld-option', '--orphan-handling=warn'), work, Path.cwd())
            self.assertEqual(linker[1:], ['-m', 'elf_i386', '--orphan-handling=warn', '-v'])

    def test_provider_paths_relocate_without_reordering(self):
        args = [
            '--target=aarch64-linux-gnu', '-resource-dir', 'external/clang/lib/clang/23',
            '-isystem', 'external/libc/include', '-DVALUE=1', '-Xclang',
            '-internal-isystem', '-Xclang', 'external/compiler-rt/include',
            '-Lexternal/glibc/lib', '-lpthread', '-ldl', '-L', 'external/unwind/lib',
            '-lunwind', '-L/absolute/joined', '-L', '/absolute/separate', '-lc',
        ]
        self.assertEqual(absolute_arguments(args, Path('/exec')), [
            '--target=aarch64-linux-gnu', '-resource-dir', '/exec/external/clang/lib/clang/23',
            '-isystem', '/exec/external/libc/include', '-DVALUE=1', '-Xclang',
            '-internal-isystem', '-Xclang', '/exec/external/compiler-rt/include',
            '-L/exec/external/glibc/lib', '-lpthread', '-ldl', '-L', '/exec/external/unwind/lib',
            '-lunwind', '-L/absolute/joined', '-L', '/absolute/separate', '-lc',
        ])

    def test_try_run_uses_directory_environment_and_temporary_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / 'source'
            source.mkdir()
            (source / 'marker').touch()
            previous = root / 'previous'
            previous.mkdir()
            (previous / 'report.json').write_text(json.dumps(dict(capabilities={}, history=[])))
            request = self.request(kind='try-run')
            request.update(
                source=str(source), directory='nested', environment={'CHECK_CONTEXT': 'yes', 'CONFIG_SHELL': SHELL},
                command='test "$CHECK_CONTEXT" = yes && test "${PWD##*/}" = nested && test -f __source__/marker && printf ok > "$TMP" && test -f "$TMP"',
            )
            key = json.dumps(request)
            output = root / 'answers'
            probes(dict(previous=str(previous), requests={key: request}), output, consumer_final=False)
            self.assertTrue(json.loads((output / 'report.json').read_text())['capabilities'][key])

    def test_host_shell_preserves_output_despite_failed_status(self):
        def planner_answers(directory):
            chunks = sorted(directory.rglob('*=*'), key=lambda path: path.name)
            encoded = ''.join(path.name.split('=', 1)[1] for path in chunks)
            return json.loads(unquote(encoded))

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            previous = root / 'previous'
            seed = dict(capabilities={}, history=[])
            publish_answers(previous, seed)
            self.assertEqual(planner_answers(previous), dict(capabilities={}))
            self.assertEqual((previous / 'report.json').read_text(), json.dumps(seed, indent=2))
            request = self.request(kind='host-shell')
            request['command'] = "printf 'first\\nsecond\\n\\n'; printf 'warning\\n' >&2; exit 1"
            key = json.dumps(request)
            output = root / 'answers'
            probes(dict(previous=str(previous), requests={key: request}), output, consumer_final=False)
            report = dict(capabilities={key: 'first second'}, history=[[dict(
                key=key, argv=[SHELL, '-c', request['command']], status=1,
                stdout='first\nsecond\n\n', stderr='warning\n',
            )]])
            self.assertEqual(planner_answers(output), dict(capabilities=report['capabilities']))
            self.assertEqual((output / 'report.json').read_text(), json.dumps(report, indent=2))

            converged = root / 'converged'
            probes(dict(previous=str(output), requests={}), converged, consumer_final=False)
            report['converged'] = True
            self.assertEqual(planner_answers(converged), dict(converged=True))
            self.assertEqual((converged / 'report.json').read_text(), json.dumps(report, indent=2))

            # The last handoff recovers capabilities from the full report,
            # even though its predecessor's filenames no longer carry them.
            final = root / 'final'
            probes(dict(previous=str(converged), requests={}), final, consumer_final=True)
            self.assertEqual(planner_answers(final), dict(capabilities=report['capabilities'], converged=True))
            self.assertEqual((final / 'report.json').read_text(), json.dumps(report, indent=2))

            # A real probe at the final boundary must not claim convergence.
            pending = root / 'pending'
            probes(dict(previous=str(previous), requests={key: request}), pending, consumer_final=True)
            self.assertEqual(planner_answers(pending), dict(capabilities=report['capabilities']))
            del report['converged']
            self.assertEqual((pending / 'report.json').read_text(), json.dumps(report, indent=2))

    def test_host_shell_rejects_infrastructure_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            previous = root / 'previous'
            previous.mkdir()
            (previous / 'report.json').write_text(json.dumps(dict(capabilities={}, history=[])))
            request = self.request(kind='host-shell')
            request['command'] = 'printf unsupported > "$KBUILD_PROBE_ERROR"; exit 0'
            with self.assertRaisesRegex(RuntimeError, 'infrastructure failed: unsupported'):
                probes(dict(previous=str(previous), requests={json.dumps(request): request}), root / 'answers', consumer_final=False)

    def test_native_initramfs_input_shell_preserves_quoted_values(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            previous = root / 'previous'
            publish_answers(previous, dict(capabilities={}, history=[]))
            for index, (configured, expected) in enumerate([
                ('""', ''),
                ('"archive.cpio"', 'archive.cpio'),
                ('"first.list  second.list"', 'first.list  second.list'),
            ]):
                with self.subTest(configured=configured):
                    request = self.request(kind='host-shell')
                    request['command'] = 'echo ' + configured
                    key = json.dumps(request)
                    output = root / str(index)
                    probes(dict(previous=str(previous), requests={key: request}), output, consumer_final=False)
                    report = json.loads((output / 'report.json').read_text())
                    self.assertEqual(report['capabilities'][key], expected)
                    self.assertEqual(report['history'], [[dict(
                        key=key, argv=[SHELL, '-c', request['command']], status=0,
                        stdout=expected + '\n', stderr='',
                    )]])

    def test_host_shell_resolves_native_output_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            previous = root / 'previous'
            previous.mkdir()
            (previous / 'report.json').write_text(json.dumps(dict(capabilities={}, history=[])))
            logical = 'bazel-out/config/bin/kernel.generated/libsubcmd'
            request = self.request(kind='host-shell')
            request.update(
                directories=['bazel-out/config/bin/kernel.generated', logical],
                environment={'CONFIG_SHELL': SHELL, 'O': logical},
            )
            # Native Make's PWD expands to the logical __object__ alias.
            # Accept both physical and logical pwd spellings, retaining raw
            # stdout evidence. An absent cd target must still be rejected.
            for index, cwd in enumerate(['"$PWD"', '__object__']):
                request['command'] = f'cd {cwd}; cd "$O"; pwd'
                key = json.dumps(request)
                output = root / f'answers{index}'
                probes(dict(previous=str(previous), requests={key: request}), output, consumer_final=False)
                report = json.loads((output / 'report.json').read_text())
                self.assertEqual(report['capabilities'][key], logical)
                self.assertEqual(report['history'][0][0]['status'], 0)
                self.assertEqual(report['history'][0][0]['stderr'], '')
                spelling = '__object__/' if cwd == '__object__' else ''
                self.assertEqual(report['history'][0][0]['stdout'], '__probe_tmp__/object/' + spelling + logical + '\n')

            # A typo outside the declared parent must fail, rather than
            # publishing the probe's own temporary cwd as ABSOLUTE_O.
            request['environment']['O'] = 'bazel-out/config/bin/undeclared/libsubcmd'
            with self.assertRaisesRegex(ValueError, 'outside its declared output namespaces'):
                probes(dict(previous=str(previous), requests={json.dumps(request): request}), root / 'invalid', consumer_final=False)

    def test_output_directories_preserve_inputs_without_mutating_them(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            execution, work = root / 'exec', root / 'work'
            work.mkdir()
            logical = 'bazel-out/config/bin/kernel.generated'
            original = execution / logical
            original.mkdir(parents=True)
            (original / 'unrelated').write_text('original output')
            sibling = original.with_name('selected-tool')
            sibling.write_text('declared input')
            directories = stage_output_directories(work, execution, [logical, logical + '/libsubcmd'])
            self.assertEqual(directories[work / logical], logical)
            self.assertEqual((work / sibling.relative_to(execution)).read_text(), 'declared input')
            self.assertFalse((work / logical / 'unrelated').exists(), 'Output contents must not be inherited from another action')
            (work / logical / 'libsubcmd/probe').write_text('new output')
            self.assertEqual((original / 'unrelated').read_text(), 'original output')
            self.assertFalse((original / 'libsubcmd').exists(), 'The probe must not create outputs in the execution input tree')
            (work / logical / 'escape').symlink_to(original, target_is_directory=True)
            with self.assertRaisesRegex(ValueError, 'outside its declared output namespaces'):
                logical_probe_output(str(work / logical / 'escape') + '\n', directories, work)
            with self.assertRaisesRegex(ValueError, 'not a declared output path'):
                stage_output_directories(work, execution, ['bazel-out/../external/tool'])


if __name__ == '__main__':
    unittest.main()
