# linux.bzl examples

This standalone Bzlmod workspace exercises the public repository-rule API from
the adjacent checkout. Its `local_path_override` is only for repository
development; consumers using a published release should remove that block.

Build the fixed kernel outputs:

```sh
bazel build //:x86_64_kernel
bazel build //:aarch64_kernel
```

Build a deterministic `newc` initramfs through the public `@linux.bzl` entry
point:

```sh
bazel build //:example_initramfs
```

Build the named config variant:

```sh
bazel build //:x86_64_debug_kernel
bazel build //:x86_64_btf_vmlinux
bazel build //:x86_64_lz4_kernel
bazel build //:x86_64_kvm_kernel
bazel build //:x86_64_verity_kernel
```

The verity variant enables the device-mapper verity target. Signed root-hash
verification remains outside the supported configuration surface because it
selects the kernel trusted-key and certificate path.

The BTF variant builds `vmlinux` with `CONFIG_DEBUG_INFO_BTF=y`. This consumer
does not declare a direct `pahole` dependency; the build exercises the
tool supplied transitively by `linux.bzl`.

Kernel graph repositories also expose their fixed in-tree module outputs:

```sh
bazel build @example_x86_64//:modules
bazel build @example_x86_64//:module_symvers
bazel build @example_x86_64//:modules_order
bazel build @example_x86_64//:modules_builtin
bazel build @example_x86_64//:modules_builtin_modinfo
```

The example configurations set `CONFIG_MODULES=n`, so the `:modules`
projection is an empty file set; these commands exercise the fixed label
contract rather than compiling a module. Module build and load coverage lives
in `e2e`.

These examples focus on repository-rule configuration and deterministic
packaging. Runtime coverage lives in the sibling `e2e` workspace, which boots
the kernels with an initramfs under hermetic QEMU and verifies module loading.
Aya's eBPF consumer tests live in the separate `aya_e2e` workspace so its
nightly Rust toolchain remains isolated from the stable Rust-for-Linux
toolchain.
