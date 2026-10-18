# Kernel examples

These are packages in the root linux.bzl workspace. Run `build.py` and
`configure.py` from the repository root first, as described in the root README.
The original files in `configs/` remain the requested fragments. The measured
Kconfig resolver evaluates them using the selected platform and compiler;
configuration tests compare their results with native Linux tooling.

```sh
# From the repository root:
bazel test //examples/... --test_output=errors
bazel build //examples:x86_64_kernel
bazel build //examples:x86_64_debug_kernel
bazel build //examples:x86_64_lz4_kernel
bazel build //examples:aarch64_kernel
```

`kernel_example` selects a platform and a BCR LLVM toolchain through a transition.
`<name>_config` resolves only the configuration; `<name>` declares the full
artifact build. Kernel builds are tagged `manual` because they are expensive.
The measured adapter's complete build/link acceptance currently covers the root
minimal x86-64 kernel. The retained example image targets exercise larger graphs
and are not yet qualified as bootable images.

`x86_64_btf_vmlinux` requests the BTF fragment. The `btf_capability_test` checks
native configuration parity, including unavailable capabilities; it does not
assert that the configured toolchain and installed prerequisites enable BTF.

`example_initramfs` generates the deterministic archive with a declared Python
executable. This fixture is retained for future image/boot integration and is
not injected into the current minimal kernel build.
