"""Exercise the declared Linux generator and the public initramfs rule."""

import argparse
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest


parser = argparse.ArgumentParser()
parser.add_argument("--archive", required=True, type=Path)
parser.add_argument("--reordered", required=True, type=Path)
parser.add_argument("--generator", required=True, type=Path)
parser.add_argument("--config", required=True, type=Path)
parser.add_argument("--message", required=True, type=Path)
args, remaining = parser.parse_known_args()


class InitramfsTest(unittest.TestCase):
    def read_archive(self, data: bytes) -> dict[str, tuple[tuple[int, ...], bytes]]:
        entries = {}
        offset = 0
        while True:
            self.assertEqual(data[offset:offset + 6], b"070701")
            header = data[offset + 6:offset + 110]
            self.assertEqual(len(header), 104)
            fields = tuple(int(header[index:index + 8], 16) for index in range(0, 104, 8))
            offset += 110
            name_bytes = data[offset:offset + fields[11]]
            self.assertEqual(name_bytes[-1:], b"\0")
            name = name_bytes[:-1].decode()
            offset += fields[11]
            aligned = (offset + 3) & ~3
            self.assertEqual(data[offset:aligned], b"\0" * (aligned - offset))
            offset = aligned
            content = data[offset:offset + fields[6]]
            self.assertEqual(len(content), fields[6])
            offset += fields[6]
            aligned = (offset + 3) & ~3
            self.assertEqual(data[offset:aligned], b"\0" * (aligned - offset))
            offset = aligned
            if name == "TRAILER!!!":
                self.assertEqual(fields, (0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 11, 0))
                self.assertEqual(data[offset:], b"\0" * (len(data) - offset))
                self.assertEqual(len(data) % 512, 0)
                return entries
            self.assertNotIn(name, entries)
            entries[name] = (fields, content)

    def test_rule_archive(self) -> None:
        data = args.archive.read_bytes()
        self.assertEqual(data, args.reordered.read_bytes())
        entries = self.read_archive(data)
        expected = {
            "etc": (stat.S_IFDIR | 0o755, b""),
            "usr": (stat.S_IFDIR | 0o755, b""),
            "usr/share": (stat.S_IFDIR | 0o755, b""),
            "etc/config": (stat.S_IFREG | 0o644, args.config.read_bytes()),
            "init": (stat.S_IFREG | 0o755, Path(__file__).read_bytes()),
            "usr/share/message": (stat.S_IFREG | 0o644, args.message.read_bytes()),
        }
        self.assertEqual(list(entries), list(expected))
        inodes = set()
        for name, (mode, content) in expected.items():
            with self.subTest(name=name):
                fields, actual = entries[name]
                self.assertEqual(actual, content)
                self.assertNotIn(fields[0], inodes)
                inodes.add(fields[0])
                self.assertEqual(fields[1:], (
                    mode, 0, 0, 2 if stat.S_ISDIR(mode) else 1, 0,
                    len(content), 3, 1, 0, 0, len(name.encode()) + 1, 0,
                ))

    def test_native_file_timestamps(self) -> None:
        generator = args.generator.resolve()
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = directory / "payload"
            source.write_bytes(b"native timestamp normalization\n")
            manifest = directory / "manifest"
            manifest.write_text(f"file /payload {source} 0644 0 0\n")
            outputs = []
            for timestamp in (1, 1_000_000_000):
                os.utime(source, (timestamp, timestamp))
                output = directory / f"archive-{timestamp}.cpio"
                subprocess.run([generator, "-t", "0", "-o", output, manifest], check=True)
                outputs.append(output.read_bytes())
            first, second = outputs
            self.assertEqual(first, second)
            fields, content = self.read_archive(first)["payload"]
            self.assertEqual(fields[5], 0)
            self.assertEqual(content, source.read_bytes())


if __name__ == "__main__":
    unittest.main(argv=[__file__, *remaining])
