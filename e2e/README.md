# linux.bzl e2e

This is a standalone Bzlmod workspace for exercising `linux.bzl` against the
catalog-backed Linux 6.12.96 and 6.18.39 source archives and
repository-generated x86_64 and aarch64 kernel graphs. It deliberately renames
both the rules and LLVM repositories to cover Bzlmod repository mappings across
generated BUILD files.

Build the real `:kernel`, `:image`, `:vmlinux`, `:config`, `:system_map`, and
`:kernel_release` outputs for both architectures:

```sh
bazel test //:kernel_outputs_build_test
```

Build and boot both catalog releases on both architectures under TCG:

```sh
bazel test //...
```

The four `*_boot_test` targets pair each kernel with an initramfs containing a
static Go init binary, start the architecture-specific hermetic QEMU system
binary, and require the serial marker `LINUX_BZL_BOOT_OK` after userspace
starts. Run one matrix entry directly while debugging:

```sh
bazel test //:linux_6_12_96_x86_64_boot_test --test_output=streamed
```

Build one fixed output directly:

```sh
bazel build @e2e_x86_64//:kernel
```

Each generated kernel repository owns its mandatory target platform, so these
commands do not need a caller-supplied `--platforms` flag. Add
`--config=remote` when a remote executor is configured.

QEMU runs with TCG, so the boot tests do not require host virtualization. The
registered system-QEMU toolchains require a Linux executor; macOS and Windows
hosts must configure Linux remote execution for these tests.
