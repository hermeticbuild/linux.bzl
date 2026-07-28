# Aya consumer tests

This standalone module pins
[`aya-rs/aya`](https://github.com/aya-rs/aya) at
`9e943b24ca2256eecce7db73de43c8d2b49c0c33` and runs Aya's upstream
x86_64 and aarch64 integration VMs against the adjacent `linux.bzl`
checkout.

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
bazel test @aya//test/integration-test:vm_aarch64 \
  @aya//test/integration-test:vm_x86_64
```

`bazel test //:aya_vm_tests` is the equivalent local aggregate. CI keeps both
targets in the same command deliberately: one Bazel profile and BEP capture
cross-target action deduplication and cache efficiency instead of splitting
the measurements between independent servers.
