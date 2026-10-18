GNU Make integration consists of the native syntax-export patch and three export
includes. Linux Kconfig integration is prepared from the supplied checkout.
Generation exports file-local syntax with symbolic compiler inputs; the measured
runtime reprocesses compiler-dependent entries with the selected toolchain.

The generator launcher invokes each parser in a fresh instance of the same static
executable. `build.py` combines parser objects with `ld -r` and namespaces their
defined global symbols to avoid collisions with CPython and each other.

`build_parser.py` prepares the measurement syntax tool used by probe actions.
`build_reference.py` compiles unmodified native `conf` plus an independent symbol
value reader for tests. These currently use installed bootstrap compilers and
are explicitly part of the local acceptance setup.
