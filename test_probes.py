"""Compiler probes retain native helper context, including diagnostic behavior."""
import json
import argparse
from pathlib import Path
import subprocess
import tempfile
import unittest
import sys

parser = argparse.ArgumentParser()
parser.add_argument('--compiler', required=True, type=Path)
parser.add_argument('--shell', required=True, type=Path)
tools, remaining = parser.parse_known_args()
COMPILER, SHELL = str(tools.compiler.absolute()), str(tools.shell.absolute())
sys.argv[1:] = remaining

from probe_runtime import absolute_arguments, probe_command, probes


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
        args = ['--target=aarch64-linux-gnu', '-resource-dir', 'external/clang/lib/clang/23', '-isystem', 'external/libc/include', '-DVALUE=1', '-Xclang', '-internal-isystem', '-Xclang', 'external/compiler-rt/include']
        self.assertEqual(absolute_arguments(args, Path('/exec')), ['--target=aarch64-linux-gnu', '-resource-dir', '/exec/external/clang/lib/clang/23', '-isystem', '/exec/external/libc/include', '-DVALUE=1', '-Xclang', '-internal-isystem', '-Xclang', '/exec/external/compiler-rt/include'])

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
            probes(dict(previous=str(previous), requests={key: request}), output)
            self.assertTrue(json.loads((output / 'report.json').read_text())['capabilities'][key])

    def test_host_shell_preserves_output_despite_failed_status(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            previous = root / 'previous'
            previous.mkdir()
            (previous / 'report.json').write_text(json.dumps(dict(capabilities={}, history=[])))
            request = self.request(kind='host-shell')
            request['command'] = "printf 'first\\nsecond\\n\\n'; exit 1"
            key = json.dumps(request)
            output = root / 'answers'
            probes(dict(previous=str(previous), requests={key: request}), output)
            report = json.loads((output / 'report.json').read_text())
            self.assertEqual(report['capabilities'][key], 'first second')
            self.assertEqual(report['history'][0][0]['status'], 1)

    def test_host_shell_rejects_infrastructure_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            previous = root / 'previous'
            previous.mkdir()
            (previous / 'report.json').write_text(json.dumps(dict(capabilities={}, history=[])))
            request = self.request(kind='host-shell')
            request['command'] = 'printf unsupported > "$KBUILD_PROBE_ERROR"; exit 0'
            with self.assertRaisesRegex(RuntimeError, 'infrastructure failed: unsupported'):
                probes(dict(previous=str(previous), requests={json.dumps(request): request}), root / 'answers')


if __name__ == '__main__':
    unittest.main()
