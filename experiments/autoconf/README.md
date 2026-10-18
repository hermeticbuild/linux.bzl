# Measured autoconf kernel example

This is the integrated `autoconf-prototype/full` experiment. It resolves Kconfig
and Kbuild in execution-time Starlark `map_directory` callbacks after measuring
compiler capabilities, and registers each compile, generator, archive and link
step as a separate action. Make is used only during generation and independent
validation. Unresolved probes in the final graph fail the build.

The reusable probe API remains a **separate Bzlmod dependency**, currently named
`autoconf_prototype`. No copy of its implementation is included here.
`configure.py` writes an ignored `local_path_override` to the supplied checkout.
The existing `examples/` workspace and analysis-time implementation remain
available separately.

From the linux.bzl repository root:

```sh
# Requires a Linux checkout and a separate autoconf module checkout.
python3 build.py --linux _tmp/linux
python3 experiments/autoconf/configure.py \
  --linux _tmp/linux --autoconf _tmp/autoconf-prototype
cd experiments/autoconf
python3 validate.py
```

To reuse an existing static compiler export, pass
`--generated /path/to/generated/repo` to `configure.py`. Otherwise it invokes the
static compiler itself. It reads literal exported data without executing BUILD
files and preserves all symbolic Make programs and compiler queries. It creates
`.generated/` inputs: Kconfig syntax, Make programs, action catalogue, requested
profiles, measured host-capability programs, parser/reference binaries and the
local module setup. None of the large data files or binaries is committed.
Changes to the Linux tree require running configure again. Native Kconfig source
hashes are checked against `native/provenance.json` before bootstrapping.

Bazel 9.3.0 and `--experimental_allow_map_directory` are pinned here. The local
acceptance setup requires Linux/x86-64, the resolved `/usr/bin/gcc` toolchain,
GNU binutils, Python 3, Make, Bison, Flex, musl bootstrap tools, Perl, bc,
pkg-config, libelf and zlib development files. On Debian/Ubuntu:

```sh
sudo apt-get install musl-tools build-essential bison flex patch bc \
  libelf-dev zlib1g-dev pkg-config
```

This setup uses declared installed utilities and system headers/libraries. It
has not been validated for hermetic builds, remote execution, other architectures,
modules, bzImage or booting. The existing hermetic LLVM examples do not acquire
these local execution dependencies.

Directory intermediates travel as tar artifacts; the action adapter reconstructs
each declared input overlay. Large generated text is emitted in bounded chunks
because the inspected Bazel template implementation drops parameter-file
metadata. These prototype adapters are retained from the validated handoff.

The main targets are:

| Target | Result |
| --- | --- |
| `//:kernel` | Complete minimal x86-64 `vmlinux` tree |
| `//:config` | Minimal resolved `.config`, header and all-symbol report |
| `//:allno_config` | Allnoconfig-style configuration |
| `//:debug_config` | Debug configuration with DWARF5 |
| `//:custom_config` | Custom compile probe and dependent Kconfig symbols |
| `//:execute_test` | Declared input overlays, directory transport and failure propagation |
| `//:configure_test` | Symbolic export import and rejection of executable inputs |

`validate.py` builds these targets and runs both Bazel tests. It compares all
symbol values and written configs for minimal/allno/debug with an independently
compiled unmodified upstream resolver, checks the configuration used for the
linked image, compares six ordered global Kbuild flag groups and native
`make olddefconfig` outputs, verifies the custom probe, and checks the ELF and
entry symbols. It writes machine-readable reports under ignored `validation/`.
These checks do not establish byte-identical upstream output or a successful boot.

The `Autoconf kernel acceptance` workflow is manually dispatched with an
`autoconf_repository` and `autoconf_ref`. It checks out that separate module and
uses the same local override and acceptance path. Automatic prototype CI remains
independent of an unpublished module checkout.

`PROVENANCE.json` records the imported adapter's original hashes, and
`native/provenance.json` records the tested upstream parser sources. The
Linux-derived link script retains its upstream SPDX license. The GPL parser
sources themselves are taken from the supplied Linux checkout at bootstrap time.

Bazel gives all actions expanded by one `map_directory` a shared mnemonic:
`KernelConfig`, `KernelPlan`, or `KernelArtifact` here. Compile/archive/link
progress messages identify the individual operations. These are separate
spawn actions created in the callback through the input-overlay adapter;
they execute the selected compiler rather than using `cc_common.compile`.

The latest local acceptance result is recorded in [`validation-summary.json`](validation-summary.json).
