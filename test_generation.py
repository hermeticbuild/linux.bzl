"""Native syntax transport keeps conditions and compiler query context symbolic."""
import unittest
from export import parse_events
from programs import lower_probes


class SymbolicExportTest(unittest.TestCase):
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
    unittest.main()
