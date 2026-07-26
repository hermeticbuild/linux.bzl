# linux.bzl e2e

This is a standalone Bzlmod workspace for exercising `linux.bzl` against the
catalog-backed Linux 6.12.96 and 6.18.39 source archives and
repository-generated x86_64 and aarch64 kernel graphs. It deliberately renames
both the rules and LLVM repositories to cover Bzlmod repository mappings across
generated BUILD files.

Build the real `:kernel`, `:image`, `:vmlinux`, `:config`, `:system_map`, and
`:kernel_release` outputs one architecture at a time:

```sh
bazel test //:kernel_outputs_x86_64_build_test --jobs=1
bazel shutdown
bazel test //:kernel_outputs_aarch64_build_test --jobs=1
```

Build and boot the catalog releases under TCG in bounded batches. Each
`bazel shutdown` keeps the next configured kernel graph out of the previous
server's analysis memory:

```sh
bazel test //boot:init_test //cmd/qemuboot:qemuboot_test --jobs=1
bazel shutdown
bazel test //:linux_6_12_96_x86_64_boot_test \
  //:linux_6_12_96_x86_64_module_test --jobs=1 --nocache_test_results
bazel shutdown
bazel test //:linux_6_12_96_aarch64_boot_test \
  //:linux_6_12_96_aarch64_module_test --jobs=1 --nocache_test_results
bazel shutdown
bazel test //:kernel_outputs_x86_64_build_test \
  //:linux_6_18_39_x86_64_boot_test \
  //:linux_6_18_39_x86_64_module_test --jobs=1 --nocache_test_results
bazel shutdown
bazel test //:kernel_outputs_aarch64_build_test \
  //:linux_6_18_39_aarch64_boot_test \
  //:linux_6_18_39_aarch64_module_test --jobs=1 --nocache_test_results
bazel shutdown
bazel test //:linux_6_18_39_rust_module_test \
  --jobs=1 --nocache_test_results
```

The four `*_boot_test` targets pair each kernel with an initramfs containing a
static Go init binary, start the architecture-specific hermetic QEMU system
binary, and require the serial marker `LINUX_BZL_BOOT_OK` after userspace
starts. Run one matrix entry directly while debugging:

```sh
bazel test //:linux_6_12_96_x86_64_boot_test --test_output=streamed
```

The Rust fixture enables `CONFIG_RUST` on Linux 6.18.x targeting x86_64, builds
`rust_test_module.rs` through the public `linux_module` rule, inserts the
resulting `.ko`, and checks the module-load marker:

```sh
bazel test //:linux_6_18_39_rust_module_test --test_output=streamed
```

Rust-for-Linux actions require a Linux x86_64 executor and the registered
stable Rust 1.97.0 toolchain. They intentionally fail for other kernel
versions, target architectures, unsupported instrumentation paths, or module
sources that emit `MODULE_VERSION`, `version=`, or `srcversion=` metadata.

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
