"""Tests for fragment ingestion and the maintained initramfs example."""
from pathlib import Path
import stat
import argparse
import sys
import unittest

from native.configure import merge_fragments
from examples.initramfs import archive
parser = argparse.ArgumentParser()
parser.add_argument('--config-anchor', required=True)
inputs, test_args = parser.parse_known_args()


class FragmentTest(unittest.TestCase):
    def test_values_remain_requested_data(self):
        text = 'CONFIG_X=y\nCONFIG_LEVEL=0x20\nCONFIG_TEXT="a=b"\n# CONFIG_OFF is not set\n'
        self.assertEqual(merge_fragments([text], {}), text)

    def test_last_assignment_wins_and_keeps_request_order(self):
        self.assertEqual(merge_fragments(['CONFIG_X=y\nCONFIG_Y=m\n', '# CONFIG_X is not set\n'], {}),
                         'CONFIG_Y=m\n# CONFIG_X is not set\n')
        self.assertEqual(merge_fragments(['CONFIG_X=y\nCONFIG_Y=m\n'], {'X': 'n'}),
                         'CONFIG_Y=m\n# CONFIG_X is not set\n')

    def test_invalid_lines_fail(self):
        for text in ['not a config\n', 'CONFIG_BAD-NAME=y\n']:
            with self.subTest(text=text), self.assertRaises(ValueError):
                merge_fragments([text], {})

    def test_existing_fragments(self):
        root = Path(inputs.config_anchor).parent
        for name, assignment in [('x86_64', '# CONFIG_MODULES is not set'),
                                 ('debug', 'CONFIG_LOCALVERSION="-debug"'),
                                 ('btf', 'CONFIG_DEBUG_INFO_BTF=y'),
                                 ('lz4', 'CONFIG_KERNEL_LZ4=y'),
                                 ('aarch64', 'CONFIG_SERIAL_AMBA_PL011=y')]:
            with self.subTest(fragment=name):
                result = merge_fragments([(root / (name + '.config')).read_text()], {})
                self.assertIn(assignment, result.splitlines())


class InitramfsTest(unittest.TestCase):
    def test_newc_archive_and_devices(self):
        data = archive(b'example motd\n')
        self.assertEqual(len(data) % 512, 0)
        entries, offset = {}, 0
        while True:
            self.assertEqual(data[offset:offset + 6], b'070701')
            fields = [int(data[offset + 6 + i * 8:offset + 14 + i * 8], 16) for i in range(13)]
            start = offset + 110
            name = data[start:start + fields[11] - 1].decode()
            body_start = (start + fields[11] + 3) // 4 * 4
            body = data[body_start:body_start + fields[6]]
            entries[name] = (fields, body)
            offset = (body_start + fields[6] + 3) // 4 * 4
            if name == 'TRAILER!!!':
                break
        self.assertEqual(entries['etc/motd'][1], b'example motd\n')
        self.assertEqual(entries['dev/console'][0][9:11], [5, 1])
        self.assertEqual(entries['dev/null'][0][9:11], [1, 3])
        self.assertEqual(stat.S_IFMT(entries['dev/console'][0][1]), stat.S_IFCHR)
        self.assertEqual(stat.S_IFMT(entries['proc'][0][1]), stat.S_IFDIR)
        self.assertEqual(data, archive(b'example motd\n'))


if __name__ == '__main__':
    unittest.main(argv=[sys.argv[0], *test_args])
