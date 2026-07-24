# linux.bzl

Bazel-native, hermetic Linux kernel builds with per-object actions, remote
execution, and reusable action-cache entries.

> [!WARNING]
> `linux.bzl` is experimental and pre-1.0. The public API may change while the
> Kconfig/Kbuild evaluator is extended. The supported contract is deliberately
> small. Recognized incompatible configurations are rejected explicitly.

## Quick start

Add `linux.bzl` and a hermetic C/C++ toolchain to `MODULE.bazel`:

```starlark
bazel_dep(name = "linux.bzl", version = "0.1.0")
bazel_dep(name = "llvm", version = "0.8.3")

register_toolchains("@llvm//toolchain:all")

linux_source_repository = use_repo_rule(
    "@linux.bzl//:linux.bzl",
    "linux_source_repository",
)

linux_source_repository(
    name = "linux_6_18_39",
    version = "6.18.39",
)

linux_image = use_repo_rule(
    "@linux.bzl//:linux.bzl",
    "linux_image",
)

linux_image(
    name = "example_kernel",
    config = "//kernel:x86_64.config",
    platform = "@llvm//platforms:linux_x86_64",
    source = "@linux_6_18_39//:Kconfig",
)
```

The `bazel_dep` coordinates apply once `0.1.0` is published. When developing
against a checkout, add:

```starlark
local_path_override(
    module_name = "linux.bzl",
    path = "/path/to/linux.bzl",
)
```

The referenced config is a normal exported source file:

```starlark
# kernel/BUILD.bazel
exports_files(["x86_64.config"])
```

```text
# kernel/x86_64.config
CONFIG_64BIT=y
CONFIG_X86=y
CONFIG_X86_64=y
CONFIG_KERNEL_GZIP=y
# CONFIG_MODULES is not set
```

The source rule knows the pinned URL and integrity for maintained catalog
versions. The kernel rule derives the architecture from the supported target
platform, resolves the config fragment through Kconfig, and verifies that its
architecture selection agrees.

Build the boot image:

```sh
bazel build @example_kernel//:kernel
```

Build another fixed output directly:

```sh
bazel build @example_kernel//:vmlinux
```

No `BUILD.bazel` macro is required in the consuming repository. Repository
generation turns the selected Linux source, config, and architecture into the
per-object Bazel graph.

## Repository API

Both public repository rules are loaded directly from the root `linux.bzl`.
They are intended for the root module: kernel source and product configs are
application choices, not transitive dependency resolution.

Because the module repository and its public file have the same name, Starlark
outside `MODULE.bazel` can use Bazel's shorthand label:

```starlark
load("@linux.bzl", "linux_image")
```

Inside `MODULE.bazel`, use the `use_repo_rule("@linux.bzl//:linux.bzl", ...)`
form shown above.

`linux_source_repository` has this public surface:

| Attribute | Meaning |
| --- | --- |
| `name` | External repository name |
| `version` | Exact upstream Linux version |
| `urls` | Optional explicit archive mirrors |
| `integrity` | SHA-256 SRI digest required with explicit URLs |
| `strip_prefix` | Archive prefix; overrides the catalog default when set |
| `patches` | Deterministic patch files |
| `patch_strip` | Strip count for `patches` |

`linux_image` is the kernel-graph repository rule and has this public surface:

| Attribute | Meaning |
| --- | --- |
| `name` | Generated kernel graph repository name |
| `source` | Root `Kconfig` from `linux_source_repository` |
| `config` | Base Kconfig fragment |
| `platform` | Hermetic LLVM Linux target platform |
| `overlays` | Named config fragments |

There is intentionally no `arch` attribute. `platform` accepts exactly the
Hermetic LLVM `linux_x86_64`, `linux_aarch64`, or `linux_arm64` platform
targets. With the default module repository name these are under
`@llvm//platforms`; a consumer repository mapping may give `@llvm` a different
apparent name. Architecture is derived from the canonical target, and the
config must select the same architecture. There are also no compiler paths,
host probe overrides, image-format switches, or signing keys in the public API.

## Why

Wrapping `make` in one Bazel action hides the kernel build graph from Bazel.
Any source or configuration change invalidates that action, and remote workers
cannot cache or schedule the individual compile steps.

`linux.bzl` instead:

- evaluates the relevant Kconfig and Kbuild language without invoking `make`;
- emits a Bazel target for each supported generated file, object, archive, and
  final image step;
- obtains compilers and binary utilities from registered Bazel toolchains;
- declares source, generated-header, tool, and response-file inputs explicitly;
  and
- lets Bazel schedule and cache compilation at object granularity.

Repository generation resolves Kconfig with a deterministic probe profile for
Hermetic LLVM 22.1.4. The kernel repository requires that exact profile, rather
than treating module `llvm` 0.8.3 as a minimum version, and the transitioned
graph rejects a non-Clang C/C++ toolchain.

## Supported configurations

| Area | Supported |
| --- | --- |
| Catalog releases | 6.12.96 and 6.18.39 |
| Target architectures | x86_64 and aarch64 |
| Repository evaluation | Pinned generator archives for Linux, macOS, and Windows on amd64 and arm64 |
| Build toolchain | Hermetic LLVM 22.1.4 through module `llvm` 0.8.3 |
| Images | x86 `bzImage`, arm64 `Image`, and `vmlinux` |
| Config variants | Base fragment plus named overlay fragments |

The two LTS lines are the maintained compatibility catalog. Other
integrity-pinned Linux 6.x releases may work, but are experimental until added
to that catalog and its release checks. The repository generator is published
for each host listed above; kernel compile actions still target the registered
Linux Hermetic LLVM toolchain.

The initial public contract is limited to resolved configs, boot images,
`vmlinux`, `System.map`, and kernel release metadata. Modules, device trees,
BPF syscall support, BTF, signing, Rust, and other build paths are not exported
or supported. Module selections and known incompatible BPF, BTF, signing, and
Rust selections are rejected during repository generation. So is
`CONFIG_X86_NATIVE_CPU`, because `-march=native` would make an action depend on
worker CPU features that are absent from its cache key. Generated graphs that
require device-tree or other unsupported artifact rules are also rejected.
Selecting other unimplemented features remains outside the supported contract.
No placeholder labels or provider fields are reserved for unsupported outputs.

## Source repositories

`linux_source_repository` fetches the upstream tree once. Kernel graph
repositories for multiple architectures or configurations can reuse it.

### Catalog source

For a maintained release, `version` is enough:

```starlark
linux_source_repository(
    name = "linux_6_12_96",
    version = "6.12.96",
)
```

The catalog pins the archive URL, strip prefix, and integrity. It is a
convenience index, not a floating release channel. `version` is checked against
the version fields in the downloaded tree's root `Makefile`.

### Explicit source

An uncataloged release requires both an explicit URL and integrity:

```starlark
linux_source_repository(
    name = "linux_custom",
    integrity = "sha256-BASE64_DIGEST",
    strip_prefix = "linux-6.x.y",
    urls = [
        "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.x.y.tar.xz",
    ],
    version = "6.x.y",
)
```

Deterministic source patches may be supplied with `patches` and `patch_strip`.
Arbitrary patch commands and host patch tools are intentionally not part of the
API. The complete upstream source archive, including documentation, samples,
and license material, remains available in the repository.

## Configs and variants

The base input is a Kconfig fragment. It must select exactly one supported
architecture, for example `CONFIG_X86_64=y` or `CONFIG_ARM64=y`. Repository
generation applies Kconfig defaults, dependencies, selects, and implies using
the pinned LLVM probe profile. An absent symbol follows Kconfig semantics; use
`# CONFIG_NAME is not set` for a deliberate unset.

Named overlays contain only deliberate assignments and unsets:

```text
CONFIG_DEBUG_KERNEL=y
# CONFIG_RANDOMIZE_BASE is not set
```

Declare them on the same kernel graph repository:

```starlark
linux_image(
    name = "example_kernel",
    config = "//kernel:x86_64.config",
    overlays = {
        "debug": "//kernel:debug.config",
    },
    platform = "@llvm//platforms:linux_x86_64",
    source = "@linux_6_18_39//:Kconfig",
)
```

Each overlay is merged onto the base fragment and resolved independently.
Variant outputs use the same fixed contract:

```sh
bazel build @example_kernel//variants/debug:kernel
bazel build @example_kernel//variants/debug:vmlinux
```

Overlay names are stable label path components. Rename an overlay only when
changing its public target names is intentional. Names use lowercase ASCII
letters, digits, `_`, and `-`; `base`, Windows-reserved names, and names that
collide after target-name sanitization are rejected.

## Fixed output contract

Every base and variant package exposes the same labels, and every label returns
a real kernel artifact:

| Label | Current contents |
| --- | --- |
| `:kernel` | Real architecture boot image and `LinuxKernelInfo` |
| `:image` | Real architecture boot image |
| `:vmlinux` | Real linked ELF kernel |
| `:config` | Resolved kernel configuration |
| `:system_map` | Real `System.map` |
| `:kernel_release` | Real computed kernel release |

The `:kernel` target's `DefaultInfo` contains only the boot image, so it can be
used directly by packaging and VM rules without selecting an output group.

### Providers

Load public providers from the root entry point:

```starlark
load("@linux.bzl//:linux.bzl", "LinuxKernelInfo")
```

`LinuxKernelInfo` exposes:

```text
arch
version
kernel_release
image
vmlinux
config
system_map
```

`arch` and `version` are strings. Every other field is a concrete `File`.

## Toolchains and hermeticity

`platform` is mandatory on every kernel repository. The initial API accepts the
canonical Hermetic LLVM Linux x86_64 and aarch64/arm64 platforms and applies
that platform once at the public `:kernel` gateway. The configured rules
consume the standard rules_cc toolchain interface, but other toolchain
implementations are not part of the supported contract yet.

Repository generation downloads the platform-specific, integrity-pinned
Kconfig graph generator selected by the rules release's checked-in table. The
Starlark rule adapts its legacy source and `require_real` declarations, checks
their expected schema, and verifies that the emitted metadata names the
requested config and contains object variants. The generator never consumes a
build output, which keeps module resolution valid and reproducible.

The build does not read ambient host tools or environment variables. All tools
are Bazel inputs, temporary paths are action-local, timestamps and release
metadata are normalized, and source downloads require integrity. Remote cache
and executor settings belong to the consuming workspace or CI environment;
this repository does not prescribe a service.

## Cache behavior

Source and object actions are separate, so changing one source file does not
invalidate a monolithic kernel-build action. Source inputs are classified:
ordinary compiles receive headers, bounded special lookups, and same-directory
include candidates rather than the complete source archive.

Version 0.1 deliberately includes the resolved config artifact and generated
header set in each object action. This is a conservative cache key: named
variants currently use independent graphs and do not share object actions.
Reducing that config input requires a future generator/schema revision and
broader differential coverage of source-level `CONFIG_*` references.

For x86 boot images, the resolved config must select either
`CONFIG_KERNEL_GZIP=y` or `CONFIG_KERNEL_LZ4=y`. Other x86 payload compression
modes fail during repository evaluation rather than producing a mismatched
image.

## Examples

[`examples/`](examples/) is a standalone Bzlmod workspace containing:

- catalog-backed x86_64 and aarch64 kernels;
- named debug and LZ4 overlays;
- aliases for real fixed image outputs.

From a repository checkout:

```sh
cd examples
bazel build @example_x86_64//:kernel
bazel build @example_x86_64//variants/debug:kernel
bazel build @example_x86_64//variants/lz4:kernel
bazel build @example_aarch64//:kernel
```

## Development

The root workspace contains parser, generator, transition, rule, and tool tests:

```sh
bazel test //...
```

The standalone examples and compatibility workspaces are excluded from root
package discovery with `.bazelignore`; run them from their own directories.
Release preparation and generator distribution are documented in
[`RELEASING.md`](RELEASING.md).

## License

`linux.bzl` is licensed under the
[Apache License 2.0](LICENSE). Downloaded Linux source remains under its
upstream licenses and is not redistributed as part of this module.
