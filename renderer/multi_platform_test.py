#!/usr/bin/env python3
"""Check that every split branch produced an ELF kernel for its selected CPU."""
import json
import os
from pathlib import Path
import sys

MACHINES = {'x86_64': (62, 2, 1), 'aarch64': (183, 2, 1),
            'riscv64': (243, 2, 1), 's390x': (22, 2, 2), 'armv7': (40, 1, 1),
            'hexagon': (164, 1, 1), 'loongarch64': (258, 2, 1),
            'mips32': (8, 1, 2), 'ppc32': (20, 1, 2), 'sparc64': (43, 2, 2)}
root = Path(os.environ['TEST_SRCDIR']) / os.environ['TEST_WORKSPACE']
spec = json.loads(Path(sys.argv[1]).read_text())
for cpu, path in spec['kernels'].items():
    data = (root / path).read_bytes()
    assert data[:4] == b'\x7fELF', (cpu, 'not an ELF image')
    machine, elf_class, encoding = MACHINES[cpu]
    assert (data[4], data[5]) == (elf_class, encoding), (cpu, data[:6])
    actual = int.from_bytes(data[18:20], 'little' if encoding == 1 else 'big')
    assert actual == machine, (cpu, actual, machine)
    assert int.from_bytes(data[16:18], 'little' if encoding == 1 else 'big') in (2, 3), (cpu, 'not a loadable kernel ELF')
    print('PASS:', cpu, path)

for cpu, paths in spec['boot'].items():
    for path in paths:
        assert (root / path).stat().st_size > 0, (cpu, 'empty native boot image')
