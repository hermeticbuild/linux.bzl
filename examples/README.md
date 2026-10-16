# linux.bzl prototype examples

These are the maintained examples, updated to consume the static repository
compiler. All five config fragments in `configs/` remain the requested inputs.
The repository rule reads them as data; Kconfig resolution and platform/toolchain
selection happen during Bazel analysis. Debug, BTF and LZ4 requests overlay the
x86 fragment, while ARM64 uses its own fragment and Bazel platform. Unspecified
prompts use the prototype's `allnoconfig` policy.

From the repository root, first build and configure the prototype:

```sh
python3 build.py --linux _tmp/linux
_tmp/kernel-repository/kernel-repository generate \
  --linux _tmp/linux --out _tmp/kernel-repository/repo --all-backends
python3 example.py
```

`example.py` writes only ignored `.generated/` module support and dependency
patches. It preserves this workspace's BUILD file and config fragments. Re-run
it if the source checkout/tool location changes. Fragment edits are tracked by
the repository rule and do not need a separate generation invocation.

Then, from `examples/`:

```sh
bazel test //...
bazel build //:x86_64_kernel //:aarch64_kernel
bazel build //:x86_64_debug_kernel //:x86_64_lz4_kernel
bazel build //:example_initramfs
```

The kernel targets currently produce linked `vmlinux` ELF artifacts. Compression
requests remain part of the resolved configuration; boot-image compression is
not demonstrated by the ELF outputs. `example_initramfs` produces deterministic
`newc` data with `/dev/console`, `/dev/null`, `/proc`, and this README as
`/etc/motd`, using the declared Python execution runtime.

Each kernel target has a corresponding `_config` target exposing its expanded
`.config`, generated header and resolution report. `x86_64_shared_graph_metadata`
now exposes the prototype's action manifest instead of the old graph-repository
metadata format.

The `x86_64_btf_vmlinux` target is retained as a manual target with an explicit
`DEBUG_INFO_BTF=y` requirement. The current conservative capability policy does
not supply the required pahole capabilities: this target reports the gap rather
than quietly producing an ELF without BTF. Its requested fragment remains
available through `x86_64_btf_vmlinux_config`.

The former module-output labels and release-specific graph repositories belong
to the replaced implementation. The prototype currently proves configuration,
individual object actions, native code generation, and kernel build/link.

CI builds the static generator once, then runs independent example jobs:

| CI example | Bazel build target | Analysis test |
| --- | --- | --- |
| x86_64 | `//:x86_64_kernel` | `//:x86_64_config_test` |
| aarch64 | `//:aarch64_kernel` | `//:aarch64_config_test` |
| debug | `//:x86_64_debug_kernel` | `//:debug_config_test` |
| lz4 | `//:x86_64_lz4_kernel` | `//:lz4_config_test` |
| btf-capability-gap | `//:x86_64_btf_vmlinux_config` | `//:btf_capability_test` |
| initramfs | `//:example_initramfs` | — |
| graph-metadata | `//:x86_64_shared_graph_metadata` | — |

A separate platform job runs the generated architecture tests, builds the
multi-platform configs, and checks execution tools. Every example build blocks
host tool directories. The BTF test expects the specific missing-capability
analysis failure; it does not establish that a BTF kernel builds successfully.
