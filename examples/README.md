# Kernel examples

These are ordinary packages in the linux.bzl workspace. `kernel_example` creates
one native configuration from the raw `configs/` fragments and passes that
provider to its measured build. The same platform/toolchain transition applies
to both public aliases.

`<name>_config` resolves only configuration. `<name>` requests the full product
graph. Expensive targets are tagged `manual`; configuration assertions consume
native values rather than comparing a recreated configuration with a second
native run.

```sh
bazel test //examples:x86_64_config_test //examples:aarch64_config_test \
  //examples:debug_config_test //examples:lz4_config_test \
  //examples:btf_capability_test --test_output=errors
# Set exporter to the canonical host archive directory as described in ../README.md.
bazel build //examples:x86_64_kernel --override_repository=kbuild_exporter="$exporter"
bazel build //examples:x86_64_debug_kernel --override_repository=kbuild_exporter="$exporter"
bazel build //examples:x86_64_lz4_kernel --override_repository=kbuild_exporter="$exporter"
bazel build //examples:aarch64_kernel --override_repository=kbuild_exporter="$exporter"
```

The BTF configuration test checks its actual native request and selected tool.
The initramfs fixture produces a deterministic archive with a declared Python
runtime. These fixtures do not establish complete boot/module or cross-config
reuse qualification for the migrated product implementation.
