"""Compiler probes retain native helper context, including diagnostic behavior."""
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import sys

COMPILER = shutil.which("cc")
if "--compiler" in sys.argv:
    index = sys.argv.index("--compiler")
    COMPILER = str(Path(sys.argv[index + 1]).resolve())
    del sys.argv[index:index + 2]

from probe_runtime import absolute_arguments, probe_command, probes


class ProbeTest(unittest.TestCase):
    def request(self, flags='', kind='cc-option', argument='-fno-common'):
        return dict(kind=kind, argument=argument, flags=flags, compiler=COMPILER, command='', environment={}, directory='.', source='.')

    def test_accumulated_flags_change_the_actual_answer(self):
        compiler = COMPILER
        if not compiler:
            self.skipTest('C compiler needed for execution probe regression')
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
                source=str(source), directory='nested', environment={'CHECK_CONTEXT': 'yes'},
                command='test "$CHECK_CONTEXT" = yes && test "${PWD##*/}" = nested && test -f __source__/marker && printf ok > "$TMP" && test -f "$TMP"',
            )
            key = json.dumps(request)
            output = root / 'answers'
            probes(dict(previous=str(previous), requests={key: request}), output)
            self.assertTrue(json.loads((output / 'report.json').read_text())['capabilities'][key])


if __name__ == '__main__':
    unittest.main()
