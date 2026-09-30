# Aya consumer tests

This standalone module pins
[`aya-rs/aya`](https://github.com/aya-rs/aya) at
`05d5269f848e3565e964690fc7111817a2258033` and runs Aya's upstream
x86_64 and aarch64 integration VMs against the adjacent `linux.bzl`
checkout.

`patches/aya_linux_bzl_api.patch` mirrors the upstream repository-rule
migration. `patches/aya_rust_kernel_module_e2e.patch` is deliberately owned by
linux.bzl: it wires this module's `linux_bzl_rust_btf.rs` fixture into the Aya
VMs without adding the fixture to Aya's pull request.

Aya keeps its existing rules_rs `default_rust_toolchains` declaration pinned to
`nightly/2026-06-24`. As the root consumer, Aya registers that toolchain for
its Rust and eBPF builds; `linux.bzl` consumes the same selected rustc and the
matching `rustc_srcs` exposed by the resolved Rust-analyzer toolchain without
renaming or replacing Aya's default. Keeping this as a separate root module
isolates Aya's nightly from the stable toolchain coverage in `e2e/`.

Both VM kernels enable Rust, DWARF5, kernel BTF, and module BTF. The initramfs
contains a Rust-for-Linux module, and Aya's existing integration binary loads
it and checks the published module BTF.

Run both guest platforms in one Bazel invocation:

```sh
USE_BAZEL_VERSION=9.2.0 bazel test @aya//test/integration-test:vm_aarch64 \
  @aya//test/integration-test:vm_x86_64 --config=compiler-clang
```

CI runs this pair under Bazel 9.1.0 and 9.2.0 with both `compiler-clang` and
`compiler-gcc`. The selector changes kernel target C compilation and GNU host
tools. GCC 15.2.0 uses a local kernel-only platform marker for kernel targets
and an x86_64/Linux/glibc constraint for host tools. This keeps Kbuild's objtool
warning selection consistent with its actual host compiler. Aya's musl
userspace and LLVM C eBPF compilation retain their existing toolchains.
The GNU wrapper also applies to other helpers on the matching GNU platform;
it is not an objtool-specific compiler override.
Rust and Rust eBPF retain the nightly toolchain and target
transitions described above. Both compiler choices keep the Rust BTF overlay
and the actual module-loading assertion inside each VM.

`//:compiler_toolchain_tests` checks the resolved kernel and execution-host
compilers for both kernel architectures, both musl userspace architectures,
and C BPF. CI runs these lightweight analysis tests alongside the actual VMs
in every compiler/Bazel matrix cell. Host assertions use the GNU execution
platform selected by `--config=remote`.

The GCC repository patch is the same Kbuild action/remote execution adapter
used by `e2e/patches/gcc_toolchain_remote_execution.patch`.

`patches/libbpf_old_uapi_compat.patch` is the same compatibility fix used by
`e2e/`: libbpf derives the perf-event register type from the selected UAPI's
`bpf_perf_event_data.regs` field. This also works with the GCC distribution's
older headers, which predate the `bpf_user_pt_regs_t` typedef, without changing
the selected compiler, Linux headers, or libc/sysroot.

`patches/rules_rust_linker_path_mapping.patch` preserves declared LLVM linker
input paths through Bazel's argument path mapping. Unknown generated-path
operands disable mapping only for that Rust action, not the suite. CI also
runs the patch's linker-argument regression tests in every matrix cell.

`bazel test //:aya_vm_tests` is the equivalent local aggregate. CI keeps both
targets in the same command deliberately: one Bazel profile and BEP capture
cross-target action deduplication and cache efficiency instead of splitting
the measurements between independent servers.
