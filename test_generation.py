"""Native syntax transport keeps conditions and compiler query context symbolic."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from export import native_events, parse_events
import inputs
from programs import lower_probes
from syntax import runtime_program


class SymbolicExportTest(unittest.TestCase):
    def test_catalog_serialization_is_independent_of_hash_seed(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            linux = root / 'linux'
            linux.mkdir()
            fixture = linux / 'Kbuild'
            fixture.write_text(
                'hostprogs := zulu alpha missing-z missing-a rust-z rust-a\n'
                'rust-z-rust := y\n'
                'rust-a-rust := y\n'
                'generated/result: generated/z.o generated/a.o\n\t:\n'
                'generated/pair-second generated/pair-first &:\n\t:\n'
                'missing/root: missing/z missing/a\n\t:\n'
                'cycle/root: cycle/right cycle/left\n\t:\n'
                'cycle/right: cycle/root\n\t:\n'
                'cycle/left: cycle/right\n\t:\n')
            basic = linux / 'scripts/basic'
            basic.mkdir(parents=True)
            (basic / 'Kbuild').write_text('hostprogs := fixdep\n')
            for name in ('alpha.c', 'zulu.c', 'scripts/basic/fixdep.c',
                         'generated/a.c', 'generated/z.c'):
                source = linux / name
                source.parent.mkdir(parents=True, exist_ok=True)
                source.touch()
            tree = parse_events(native_events([MAKE], [fixture, basic / 'Kbuild'], expressions=True))
            tree_path = root / 'tree.json'
            tree_path.write_text(json.dumps(tree))
            outputs = []
            for seed in ('1', '2', '3', '4'):
                output = root / seed
                subprocess.run([
                    sys.executable, '-c',
                    'import json, sys\n'
                    'from pathlib import Path\n'
                    'from inputs import write_inputs\n'
                    'tree = json.loads(Path(sys.argv[1]).read_text())\n'
                    'write_inputs(Path(sys.argv[3]), tree, Path(sys.argv[2]), '
                    'tree["files"][0]["body"], "native link script\\n")\n',
                    str(tree_path), str(linux), str(output),
                ], cwd=Path(inputs.__file__).parent,
                    env=os.environ | {'PYTHONHASHSEED': seed}, check=True)
                outputs.append({name: (output / name).read_bytes()
                                for name in ('program.bzl', 'report.json')})
            for seed, output in zip(('2', '3', '4'), outputs[1:]):
                with self.subTest(seed=seed):
                    self.assertEqual(output, outputs[0])
            report = json.loads(outputs[0]['report.json'])
            self.assertEqual(set(report['actions']), {
                'alpha', 'zulu', 'scripts/basic/fixdep', 'generated/pair-second',
                'arch/x86/include/generated/asm/cpufeaturemasks.h'})
            self.assertEqual(report['actions']['generated/pair-second']['outs'],
                             ['generated/pair-second', 'generated/pair-first'])
            rejected = {entry['target']: entry for entry in report['diagnostics']}
            self.assertEqual(set(rejected), {
                'missing-a', 'missing-z', 'rust-a', 'rust-z', 'generated/result',
                'generated/a.o', 'generated/z.o', 'missing/root',
                'cycle/root', 'cycle/right', 'cycle/left'})
            self.assertEqual(rejected['missing/root']['prerequisites'], ['missing/a', 'missing/z'])
            self.assertIn('dependency cycle', rejected['cycle/root']['prerequisites'])
            self.assertEqual(report['program'], runtime_program(tree['files'][0]['body'], linux))

    def test_native_export_does_not_evaluate_source(self):
        with tempfile.TemporaryDirectory() as temporary:
            fixture = Path(temporary) / 'Makefile'
            fixture.write_text('VALUE := $(error export evaluated a Make expression)\nall:\n\t$(error export executed a recipe)\n')
            events = native_events([MAKE], [fixture], expressions=True)
            self.assertTrue(any(event.get('name') == 'VALUE' for event in events), events)
            self.assertTrue(any(event.get('kind') == 'recipe' for event in events), events)
            projected = runtime_program(parse_events(events)['files'][0]['body'], fixture.parent.resolve())
            assignment = next(node for node in projected if node.get('name') == 'VALUE')
            original = next(node for node in events if node.get('name') == 'VALUE')
            self.assertEqual(assignment['value'], original['value'])
            self.assertEqual(assignment['value_expression'], original['value_expression'])
            self.assertEqual((assignment['file'], assignment['line']), ('Makefile', 1))
            self.assertNotIn('text', assignment)
            self.assertNotIn('words', assignment)

    def test_native_setup_rejects_missing_required_include(self):
        with tempfile.TemporaryDirectory() as temporary:
            fixture = Path(temporary) / 'Makefile'
            setup = Path(temporary) / 'setup.json'
            for directive, succeeds in [('include', False), ('-include', True)]:
                with self.subTest(directive=directive):
                    fixture.write_text(f'{directive} {temporary}/absent.mk\nexport VALUE := native\n')
                    result = subprocess.run([MAKE, '-rR', '-f', fixture], capture_output=True, text=True,
                                            env={'PATH': '', 'LINUX_BZL_MAKE_SETUP': str(setup)})
                    self.assertEqual(result.returncode == 0, succeeds, result.stderr)
                    if succeeds:
                        self.assertEqual(json.loads(setup.read_text())['VALUE'], 'native')
                    else:
                        self.assertFalse(setup.exists(), 'Failed include published a partial native setup')

    def test_probe_keeps_explicit_compiler_and_accumulated_flags(self):
        literal = lambda text: [{'op': 'literal', 'value': text}]
        compiler = [{'op': 'reference', 'name': literal('HOSTCC')}]
        flags = [{'op': 'reference', 'name': literal('HOSTCFLAGS')}]
        result = lower_probes({'op': 'function', 'name': 'call', 'arguments': [
            literal('__cc-option'), compiler, flags, literal('-Werror'), literal('fallback')]})
        self.assertEqual(result['name'], 'kbuild_probe')
        self.assertEqual(result['probe_context'], {'compiler': compiler, 'flags': flags})
        self.assertEqual(result['arguments'], [literal('cc-option'), literal('-Werror'), literal('fallback')])

    def test_native_condition_preserves_both_config_dependent_branches(self):
        events = [dict(kind='file_begin', file='Kbuild'),
                  dict(kind='directive', directive='ifdef', arguments='CONFIG_SMP', file='Kbuild', line=1),
                  dict(kind='assignment', name='obj-y', value='smp.o'),
                  dict(kind='directive', directive='else', arguments='', file='Kbuild', line=3),
                  dict(kind='assignment', name='obj-y', value='up.o'),
                  dict(kind='directive', directive='endif', arguments='', file='Kbuild', line=5),
                  dict(kind='file_end')]
        branches = parse_events(events)['files'][0]['body'][0]['branches']
        self.assertEqual(branches[0]['arguments'], 'CONFIG_SMP')
        self.assertEqual([b['body'][0]['value'] for b in branches], ['smp.o', 'up.o'])
        with self.assertRaisesRegex(ValueError, 'Unclosed conditional'):
            parse_events(events[:-2] + [dict(kind='file_end')])


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--make', type=Path, required=True)
    arguments, remaining = parser.parse_known_args()
    MAKE = arguments.make.resolve()
    unittest.main(argv=[__file__, *remaining])
