# linux.bzl measured kernel prototype

This branch contains one kernel implementation. A statically linked generator
exports symbolic Linux Kconfig and GNU Make syntax. Bazel selects the platform
and C/C++ toolchains during analysis, measures compiler capabilities as actions,
and resolves Kconfig and Kbuild in execution-time Starlark callbacks. Each
compile, generator, archive, and link remains an individual Bazel action.
Kernel build actions do not invoke Make.

Compiler probes use the separate
[autoconf.bzl module](https://github.com/hermeticbuild/autoconf.bzl), pinned in
[MODULE.bazel](MODULE.bazel). Local module development uses Bazel's standard
`--override_module=autoconf.bzl=/path/to/checkout` flag.

## Build and test

Run from the repository root with Bazel 9.3.0 and a Linux checkout:

```sh
sudo apt-get install musl-tools build-essential bison flex patch bc \
  libelf-dev zlib1g-dev pkg-config
python3 build.py --linux _tmp/linux
python3 configure.py --linux _tmp/linux
bazel test //... --test_output=errors
python3 validate.py
```

The current parser is qualified against Linux commit
`0fcb05b72344f40806cbe810de8872f1698bf053`. Bootstrap checks its parser sources
against [native/provenance.json](native/provenance.json).
The bootstrap uses installed tools. The generated static Linux/x86-64 executable
contains GNU Make's syntax exporter, the kernel's Kconfig parser, CPython, and the
exporter code. Generation runs with an empty `PATH`; parser subprocesses execute
the same ELF. `configure.py` also prepares native measurement and independent
reference binaries for the current local acceptance setup.

`configure.py` creates ignored `.generated/` input data and repository mappings.
Run it after changing Kconfig or Makefiles. Source edits are tracked directly by
Bazel. To reuse an export from this version of the generator, pass
`--generated /path/to/export`.

This is still a prototype with declared installed execution utilities and system
headers/libraries. The measured adapter currently recognizes x86-64 and ARM64
platforms; complete build/link acceptance covers the minimal x86-64 kernel.
Configuration parity covers x86-64 and ARM64. Remote execution, fully hermetic
execution, other architecture images, modules, and booting are not qualified.

## Targets

| Target | Result |
| --- | --- |
| `//:kernel` | Minimal x86-64 `vmlinux` and artifact graph |
| `//:config` | Resolved minimal `.config`, header, and all-symbol report |
| `//:allno_config` | Allnoconfig-style configuration |
| `//:debug_config` | Debug configuration with DWARF5 |
| `//:custom_config` | Additional Kconfig symbols driven by an autoconf compile probe |
| `//examples:…` | Original example fragments, configuration tests, and explicit kernel builds |

The [examples](examples/README.md) are ordinary packages in this workspace.
Expensive example kernel builds are explicit targets. `bazel test //...` includes
their configuration assertions; the same fragment requests are also compared
with unmodified native Linux `conf`.

## Qualification

`python3 validate.py` builds the minimal kernel, checks its ELF and entry symbols,
compares all symbols and written settings for three configurations and the built
kernel against pristine `conf`, compares six ordered Kbuild flag groups against
native Make, and checks the custom probe consumer. Reports go to `validation/`.

```sh
bazel test //:parity_x86_64_test //:parity_debug_test //:parity_lz4_test \
  //:parity_btf_test //:arm64_parity_test \
  --extra_toolchains=@llvm//toolchain:linux_x86_64_to_linux_x86_64,@llvm//toolchain:linux_x86_64_to_linux_aarch64 \
  --test_output=errors
bazel test //:action_built_parity_test \
  --extra_toolchains=//:driver_toolchain --test_output=errors
```

The action-built compiler regression verifies that a compiler produced in the
same invocation supplies the measured configuration. It uses a compiled driver
around the selected compiler; it does not bootstrap LLVM from source.
`check_reuse.py` uses Bazel execution logs to verify that a source edit recompiles
only compiler actions in its actual downstream dependency closure.

Audit the standalone generation boundary with optional host `strace`:

```sh
python3 test_repository.py --tool _tmp/kernel-repository/kernel-repository \
  --linux _tmp/linux --out _tmp/kernel-repository/check --strace
```

The `Kernel acceptance` workflow runs these checks on pushes to the prototype
branch, pull requests, and manual dispatch. It fetches autoconf.bzl through Bzlmod.

## Source layout

- `renderer/`: native syntax transport, symbolic Make lowering, and input export.
- `native/`: native parser integration, bootstrap inputs, and reference tooling.
- `rules/`: the single execution-time Starlark resolver and artifact scheduler.
- `probe_runtime.py`, `scheduler.py`, `execute.py`: probe, dependency-scan, and action adapters.
- `examples/`: retained config fragments and platform-selected kernel targets.

The static exporter also powers [repository.bzl](repository.bzl), which publishes
a symbolic input repository from a source anchor and prebuilt generator label.
It does not register toolchains or configure the compiler. The local acceptance
setup above supplies the measurement/reference tools separately.
