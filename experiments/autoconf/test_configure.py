"""Keep imported Make and Kconfig data symbolic and regenerate portable inputs."""
import json
from pathlib import Path
import tempfile
import unittest

from configure import decode_literal, exported_call, fixtures


class FixturesTest(unittest.TestCase):
    def test_exported_programs_and_requests_are_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            generated, output = root / 'export', root / 'inputs'
            for path in ['options', 'configs', 'rules']:
                (generated / path).mkdir(parents=True)
            syntax = {'Kconfig': json.dumps([{'symbol': '__KCONFIG_CC_VERSION', 'kind': 'config'}])}
            (generated / 'options/BUILD.bazel').write_text(
                'kernel_option_sources(name="options", files=json.decode(' + repr(json.dumps(syntax)) + '))')
            actions = {'generated.h': json.dumps({'target': 'generated.h', 'driver': 'build'})}
            (generated / 'BUILD.bazel').write_text('kbuild_catalog(name="catalog", actions=' + repr(actions) + ')')
            (generated / 'configs/BUILD.bazel').write_text(
                'kernel_config(name="minimal", base=json.decode(\'{"SMP":"n"}\'), requested={"EXPERT":"y"})')
            host = {'op': 'function', 'name': 'kbuild_host_fallback', 'arguments': [[{'op': 'literal', 'value': 'probe-command'}]]}
            program = {'body': [host, host], 'unresolved': {'op': 'config', 'name': 'CONFIG_SMP'}}
            raw = 'FLAG_PROGRAM = json.decode(' + repr(json.dumps(program)) + ')\n'
            (generated / 'rules/flag_program.bzl').write_text(raw)
            fixtures(generated, output)
            self.assertEqual((output / 'rules/flag_program.bzl').read_text(), raw)
            self.assertEqual(json.loads((output / 'baseline.json').read_text()), syntax)
            self.assertEqual(json.loads((output / 'host-programs.json').read_text()), ['probe-command'])
            profile = json.loads((output / 'profile.json').read_text())
            self.assertEqual(profile['base'], {'SMP': 'n'})
            self.assertEqual(profile['requested']['EXPERT'], 'y')
            self.assertEqual(profile['requested']['OBJTOOL'], 'n')
            self.assertEqual(json.loads((output / 'provenance.json').read_text())['kconfig_files'], 1)

    def test_rejects_executable_or_ambiguous_exports(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'BUILD.bazel'
            path.write_text('kernel_config(base=run_compiler())')
            with self.assertRaises(ValueError):
                decode_literal(exported_call(path, 'kernel_config')['base'])
            path.write_text('kernel_config()\nkernel_config()')
            with self.assertRaises(ValueError):
                exported_call(path, 'kernel_config')


if __name__ == '__main__':
    unittest.main()
