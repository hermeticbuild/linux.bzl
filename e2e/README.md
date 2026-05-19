# linux.bzl e2e

This is a standalone Bzlmod workspace for exercising `linux.bzl` against a real
Linux source archive and a generated Kconfig repository.

Run the kernel build smoke test from this directory:

```sh
bazel build --config=remote //:kernel
```

Build the arm64 kernel target with:

```sh
bazel build --config=remote --platforms=@llvm//platforms:linux_arm64 //:kernel_arm64
```
