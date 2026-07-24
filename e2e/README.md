# linux.bzl e2e

This is a standalone Bzlmod workspace for exercising `linux.bzl` against the
catalog-backed Linux 6.12.96 and 6.18.39 source archives and
repository-generated x86_64 and aarch64 kernel graphs. It deliberately renames
both the rules and LLVM repositories to cover Bzlmod repository mappings across
generated BUILD files.

Build the real `:kernel`, `:image`, `:vmlinux`, `:config`, `:system_map`, and
`:kernel_release` outputs for both architectures:

```sh
bazel test //:kernel_outputs_build_test
```

Build both catalog releases for both architectures:

```sh
bazel test //...
```

Build one fixed output directly:

```sh
bazel build @e2e_x86_64//:kernel
```

Each generated kernel repository owns its mandatory target platform, so these
commands do not need a caller-supplied `--platforms` flag. Add
`--config=remote` when a remote executor is configured.

This workspace intentionally tests only implemented core image outputs.
