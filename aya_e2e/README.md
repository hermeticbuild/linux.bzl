# Aya consumer tests

This standalone module pins
[`dzbarsky/aya`](https://github.com/dzbarsky/aya) at
`231e54f29f5de163924671029e72da4672eba274` and runs Aya's upstream
x86_64 and aarch64 integration VMs against the adjacent `linux.bzl`
checkout.

Aya uses a pinned nightly Rust toolchain for eBPF programs. Keeping this as a
separate root module isolates that toolchain from the stable Rust toolchain used
by Rust-for-Linux kernel modules.

Run each architecture in a fresh Bazel server to bound the configured graph:

```sh
bazel test @aya//test/integration-test:vm_x86_64 --jobs=1 --nocache_test_results
bazel shutdown
bazel test @aya//test/integration-test:vm_aarch64 --jobs=1 --nocache_test_results
```

`//:aya_vm_tests` remains an aggregate test suite for machines with enough
memory to analyze both guest platforms together.
