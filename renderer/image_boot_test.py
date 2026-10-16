#!/usr/bin/env python3
"""Boot the generated PVH ELF and require a message from a userspace PID 1."""
import gzip
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile


def cpio(entries):
    result = bytearray()
    for ino, (name, mode, data, major, minor) in enumerate(entries + [('TRAILER!!!', 0, b'', 0, 0)], 1):
        encoded = name.encode() + b'\0'
        fields = [ino, mode, 0, 0, 1, 0, len(data), 0, 0, major, minor, len(encoded), 0]
        result += b'070701' + ''.join(f'{x:08x}' for x in fields).encode() + encoded
        result += b'\0' * (-len(result) % 4)
        result += data
        result += b'\0' * (-len(result) % 4)
    return bytes(result)


def main():
    image, init, qemu, data = map(Path, sys.argv[1:5])
    machine, accel = sys.argv[5:]
    with tempfile.TemporaryDirectory(prefix='kernel-bazel-boot-') as tmp:
        initrd = Path(tmp) / 'initrd.gz'
        archive = cpio([('dev', stat.S_IFDIR | 0o755, b'', 0, 0),
                        ('dev/console', stat.S_IFCHR | 0o600, b'', 5, 1),
                        ('init', stat.S_IFREG | 0o755, init.read_bytes(), 0, 0)])
        initrd.write_bytes(gzip.compress(archive, mtime=0))
        args = [qemu, '-L', str(data), '-machine', machine, '-accel', accel, '-cpu', 'max', '-m', '256M', '-smp', '1',
                '-display', 'none', '-serial', 'stdio', '-monitor', 'none', '-no-reboot',
                '-kernel', str(image), '-initrd', str(initrd),
                '-append', 'console=ttyS0 earlycon=uart8250,io,0x3f8 rdinit=/init panic=-1']
        try:
            result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=45)
            output, code = result.stdout, result.returncode
        except subprocess.TimeoutExpired as e:
            output, code = e.stdout or b'', None
        outdir = Path(os.environ.get('TEST_UNDECLARED_OUTPUTS_DIR', tmp))
        outdir.mkdir(parents=True, exist_ok=True)
        (outdir / 'boot.log').write_bytes(output)
        sys.stdout.buffer.write(output)
        assert b'BAZEL_KERNEL_BOOT_OK' in output, f'Kernel did not reach test PID 1 (QEMU status {code})'
        assert b'Kernel panic' not in output, 'Kernel panicked during the smoke test'
        print('PASS: Bazel-built kernel reached userspace PID 1')


if __name__ == '__main__':
    main()
