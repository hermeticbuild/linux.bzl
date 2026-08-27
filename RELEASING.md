# Releasing linux.bzl

Each rules release pins its supported Linux source catalog. Kconfig/Kbuild
planning ships with the module and runs as a normal Bazel exec tool; it has no
independent prebuilt or release stream.

The release line is Bazel 9-only. Do not add a compatibility fallback for
Bazel 8; validate both maintained Bazel 9 minors instead.

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
5. Test a consumer that uses the canonical `@linux.bzl` repository, declares
   configured images through `linux_images` from `extensions.bzl`, and imports
   each facade repository explicitly with `use_repo`.
6. On a Linux x86_64 executor with the registered stable Rust 1.97.0
   toolchain, run
   `bazel test //:linux_6_12_96_rust_module_test
   //:linux_6_12_96_aarch64_rust_module_test
   //:linux_6_18_39_rust_module_test
   //:linux_6_18_39_aarch64_rust_module_test --test_output=streamed` from
   `e2e`.
   This builds and loads version-native Rust-for-Linux modules for the
   cataloged x86_64 and aarch64 kernels. Also verify that cross-kernel
   dependencies are rejected. Build the C source-version fixture to verify
   that native Kbuild carries `MODULE_VERSION` and
   `CONFIG_MODULE_SRCVERSION_ALL` through modpost.
7. From `aya_e2e`, run the pinned Aya x86_64 and aarch64 VM targets in one
   Bazel invocation.
8. Audit the pinned source URLs and integrities in the catalog.
9. Run the full GCC and Clang RBE matrix and verify the selected toolchain is
   recorded in resolved Kconfig values and action plans.

## Publish

1. Tag the verified release commit as `vX.Y.Z` and push the tag. The release
   workflow creates the module source archive and starts the BCR publishing
   workflow using the configuration under `.bcr/`.
2. Verify the release archive and the generated BCR pull request.
3. Test a clean consumer module using the tag, without `local_path_override`.

## Support checks

A release is ready only when:

- Bazel 9.1.0 and 9.2.0 pass the root test suite.
- Linux 6.12 and 6.18 build for x86_64 and aarch64 with both registered GCC and
  Clang toolchains.
- All four maintained kernel/architecture pairs boot under the registered QEMU
  system toolchains, reach the initramfs userspace marker, and exercise
  configured module loading.
- The fixed `:modules`, `:module_symvers`, `:modules_order`,
  `:modules_builtin`, and `:modules_builtin_modinfo` labels produce the
  corresponding Kbuild artifacts.
- Rust-for-Linux modules build for the cataloged x86_64 and aarch64 Linux
  targets on a Linux x86_64 executor with stable Rust 1.97.0 through the public
  `linux_module` API. Cross-kernel module dependencies fail explicitly.
- External C and Rust modules use the same source-derived Kbuild planner as the
  configured kernel. C module coverage includes `MODULE_VERSION` and
  `CONFIG_MODULE_SRCVERSION_ALL` through native modpost stages.
- The pinned Aya x86_64 and aarch64 VM tests pass from the standalone
  `aya_e2e` module root.
- BPF-syscall and BTF-enabled configurations are covered by a compatibility
  build.
- The base and named-overlay repositories produce the documented fixed outputs.
- Unsupported Kbuild constructs and toolchain/config mismatches fail with
  actionable diagnostics.

Release CI verifies plan determinism and real kernel/module outputs through the
expanded `map_directory` actions rather than generated BUILD targets.

Signing kernel images or modules and compiling device-tree artifacts are
outside the current release contract.
