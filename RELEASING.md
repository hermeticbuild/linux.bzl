# Releasing linux.bzl

Each rules release pins both its supported Linux source catalog and the
platform-specific Kconfig graph generator archives. The generator has its own
version and release assets; changing it is an explicit compatibility update to
the rules module.

## Prepare

1. Choose a pre-1 semantic version and update the module version and release
   notes.
2. Run `bazel test //...`, the standalone examples, and the Linux 6.12 and 6.18
   compatibility builds for x86_64 and aarch64.
3. Boot all four maintained kernel/architecture pairs under hermetic QEMU and
   verify that each reaches the initramfs userspace marker. For module-enabled
   configurations, also verify that the selected in-tree `.ko` loads.
4. Build a named overlay and verify its kernel release and fixed output
   contract.
5. Test a consumer that renames the `linux.bzl` module repository and loads
   `linux_image` only from the root `linux.bzl` entry point.
6. On a Linux x86_64 executor with the registered stable Rust 1.97.0
   toolchain, run
   `bazel test //:linux_6_18_39_rust_module_test --test_output=streamed`
   from `e2e`. This builds and loads a Rust-for-Linux module for an x86_64
   Linux 6.18.x kernel. Also verify that cross-kernel dependencies and
   `MODULE_VERSION`/module source-version metadata are rejected.
7. From `aya_e2e`, run the pinned Aya x86_64 and aarch64 VM targets separately
   with `--jobs=1 --nocache_test_results`, shutting down Bazel between targets
   to bound the configured graph.
8. Audit the pinned source URLs and integrities in the catalog.
9. Audit `KCONFIG_TOOL_VERSION` and all six entries in
   `internal/kconfig_tool_releases.bzl`.

When updating the generator, first publish deterministic
`kconfig-<version>` archives for Linux, macOS, and Windows on amd64 and arm64.
Each archive must contain `kconfig_parse` at its root. Compute the Subresource
Integrity values from the published assets, update the checked-in table, and
run repository-generation smoke tests for every host archive. Rebuild the
archives once from a clean checkout and compare digests.

After verifying the generator commit, create and push its release tag with:

```sh
VERSION=vX.Y.Z ./release_kconfig.sh
```

## Publish

1. Tag the verified release commit as `vX.Y.Z`.
2. Publish the module source archive and verify it against the release
   integrity.
3. Test a clean consumer module using the tag, without `local_path_override` or
   a locally built generator.

## Support checks

A release is ready only when:

- Bazel 8.x and 9.x pass the root test suite.
- Linux 6.12 and 6.18 build for x86_64 and aarch64 with the registered Bazel C++
  toolchains.
- All four maintained kernel/architecture pairs boot under the registered QEMU
  system toolchains, reach the initramfs userspace marker, and exercise
  configured module loading.
- The fixed `:modules`, `:module_symvers`, `:modules_order`,
  `:modules_builtin`, and `:modules_builtin_modinfo` labels produce the
  corresponding Kbuild artifacts.
- Rust-for-Linux modules build for the documented x86_64 Linux 6.18.x target
  on a Linux x86_64 executor with stable Rust 1.97.0 through the public
  `linux_module` API. Cross-kernel module dependencies and
  `MODULE_VERSION`/module source-version metadata fail explicitly.
- The pinned Aya x86_64 and aarch64 VM tests pass from the standalone
  `aya_e2e` module root.
- BPF-syscall and BTF-enabled configurations are covered by a compatibility
  build.
- Repository generation is smoke-tested with all six host generator archives.
- The base and named-overlay repositories produce the documented fixed outputs.
- Unsupported Kbuild constructs and toolchain/config mismatches fail with
  actionable diagnostics.

Version 0.1 intentionally gives named variants independent object graphs.
Cross-variant action sharing is not a release requirement until the config
input model can prove it correct.

Signing kernel images or modules, `MODULE_VERSION`/module source-version
metadata, and device-tree artifacts are outside the current release contract.
