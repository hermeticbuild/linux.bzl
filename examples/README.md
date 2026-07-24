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
bazel build //:x86_64_lz4_kernel
```
