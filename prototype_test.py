"""Tests for fragment ingestion and the maintained initramfs example."""
from pathlib import Path
import stat
import argparse
import importlib.util
import sys
import tempfile
import unittest

from repository_main import read_fragment
parser = argparse.ArgumentParser()
parser.add_argument('--initramfs', required=True)
parser.add_argument('--config-anchor', required=True)
inputs, test_args = parser.parse_known_args()
spec = importlib.util.spec_from_file_location('example_initramfs', inputs.initramfs)
initramfs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(initramfs)
archive = initramfs.archive


class FragmentTest(unittest.TestCase):
    def parse(self, text):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'requested.config'
            path.write_text(text)
            return read_fragment(path)

    def test_values_remain_requested_data(self):
        self.assertEqual(self.parse('CONFIG_X=y\nCONFIG_LEVEL=0x20\nCONFIG_TEXT="a=b"\n# CONFIG_OFF is not set\n'),
                         {'X': 'y', 'LEVEL': '0x20', 'TEXT': '"a=b"', 'OFF': 'n'})

    def test_last_assignment_wins_and_keeps_request_order(self):
        values = self.parse('CONFIG_X=y\nCONFIG_Y=m\n# CONFIG_X is not set\n')
        self.assertEqual(list(values), ['Y', 'X'])
        self.assertEqual(values['X'], 'n')

    def test_invalid_lines_fail(self):
        for text in ['not a config\n', 'CONFIG_BAD-NAME=y\n']:
            with self.subTest(text=text), self.assertRaises(ValueError):
                self.parse(text)

    def test_existing_fragments(self):
        root = Path(inputs.config_anchor).parent
        self.assertEqual(read_fragment(root / 'x86_64.config')['MODULES'], 'n')
        self.assertEqual(read_fragment(root / 'debug.config')['LOCALVERSION'], '"-debug"')
        self.assertEqual(read_fragment(root / 'btf.config')['DEBUG_INFO_BTF'], 'y')
        self.assertEqual(read_fragment(root / 'lz4.config')['KERNEL_LZ4'], 'y')
        self.assertEqual(read_fragment(root / 'aarch64.config')['SERIAL_AMBA_PL011'], 'y')


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
