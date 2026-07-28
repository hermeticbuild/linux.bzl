# linux.bzl e2e

This is a standalone Bzlmod workspace for exercising `linux.bzl` against the
catalog-backed Linux 6.12.96 and 6.18.39 source archives and
repository-generated x86_64 and aarch64 kernel graphs. It uses the canonical
`@linux.bzl` API entry point while retaining a renamed LLVM repository to cover
Bzlmod repository mappings across generated BUILD files.

Build the real `:kernel`, `:image`, `:vmlinux`, `:config`, `:system_map`, and
`:kernel_release` outputs one architecture at a time:

```sh
bazel test //:kernel_outputs_x86_64_build_test
bazel shutdown
bazel test //:kernel_outputs_aarch64_build_test
```

Build and boot the catalog releases under TCG in bounded batches. Each
`bazel shutdown` keeps the next configured kernel graph out of the previous
server's analysis memory:

```sh
bazel test //boot:init_test //cmd/qemuboot:qemuboot_test
bazel shutdown
bazel test //:linux_6_12_96_x86_64_boot_test \
  //:linux_6_12_96_x86_64_module_test
bazel shutdown
bazel test //:linux_6_12_96_aarch64_boot_test \
  //:linux_6_12_96_aarch64_module_test
bazel shutdown
bazel test //:kernel_outputs_x86_64_build_test \
  //:linux_6_18_39_x86_64_boot_test \
  //:linux_6_18_39_x86_64_module_test
bazel shutdown
bazel test //:kernel_outputs_aarch64_build_test \
  //:linux_6_18_39_aarch64_boot_test \
  //:linux_6_18_39_aarch64_module_test
bazel shutdown
bazel test //:linux_6_12_96_rust_module_test \
  //:linux_6_12_96_aarch64_rust_module_test \
  //:linux_6_18_39_rust_module_test \
  //:linux_6_18_39_aarch64_rust_module_test
```

The four `*_boot_test` targets pair each kernel with an initramfs containing a
static Go init binary, start the architecture-specific hermetic QEMU system
binary, and require the serial marker `LINUX_BZL_BOOT_OK` after userspace
starts. Run one matrix entry directly while debugging:

```sh
bazel test //:linux_6_12_96_x86_64_boot_test --test_output=streamed
```

The Rust fixtures enable `CONFIG_RUST`, DWARF5, kernel BTF, and module BTF on
Linux 6.12.x and 6.18.x targeting x86_64 and aarch64. They build
version-native module sources through the public `linux_module` rule, insert
the resulting `.ko`, and check the module-load marker:

```sh
bazel test //:linux_6_12_96_rust_module_test \
  //:linux_6_12_96_aarch64_rust_module_test \
  //:linux_6_18_39_rust_module_test \
  //:linux_6_18_39_aarch64_rust_module_test \
  --test_output=streamed
```

This consumer workspace declares and registers the standard rules_rs stable
Rust 1.97.0 toolchains. Both maintained kernel lines enforce their upstream
minimum of Rust 1.78.0 when the selected compiler is probed.
`linux.bzl` consumes the selected compiler and the matching `rustc_srcs` from
the resolved Rust-analyzer toolchain; it does not register a production Rust
toolchain itself. Rust-for-Linux actions require a Linux x86_64 executor even
when the kernel target is aarch64. They intentionally fail for unsupported
debug/instrumentation paths or module sources that emit `MODULE_VERSION`,
`version=`, or `srcversion=` metadata.

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
