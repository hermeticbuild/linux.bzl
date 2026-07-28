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
bazel_dep(name = "llvm", version = "0.8.14")

register_toolchains(
    "@llvm//toolchain:linux_aarch64_to_linux_aarch64",
    "@llvm//toolchain:linux_aarch64_to_linux_x86_64",
    "@llvm//toolchain:linux_x86_64_to_linux_aarch64",
    "@llvm//toolchain:linux_x86_64_to_linux_x86_64",
    "@llvm//toolchain:macos_aarch64_to_linux_aarch64",
    "@llvm//toolchain:macos_aarch64_to_linux_x86_64",
    "@llvm//toolchain:macos_x86_64_to_linux_aarch64",
    "@llvm//toolchain:macos_x86_64_to_linux_x86_64",
    "@llvm//toolchain:windows_aarch64_to_linux_aarch64",
    "@llvm//toolchain:windows_aarch64_to_linux_x86_64",
    "@llvm//toolchain:windows_x86_64_to_linux_aarch64",
    "@llvm//toolchain:windows_x86_64_to_linux_x86_64",
)

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

## Public API

Every public symbol is loaded directly from the root `linux.bzl`. The source
and image repository rules are intended for the root module: kernel source and
product configs are application choices, not transitive dependency resolution.

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
| `config_mode` | Kconfig baseline: `default` or `allnoconfig` |
| `platform` | Hermetic LLVM Linux target platform |
| `overlays` | Named config fragments |

There is intentionally no `arch` attribute. `platform` accepts exactly the
Hermetic LLVM `linux_x86_64`, `linux_aarch64`, or `linux_arm64` platform
targets. With the default module repository name these are under
`@llvm//platforms`; a consumer repository mapping may give `@llvm` a different
apparent name. Architecture is derived from the canonical target, and the
config must select the same architecture. There are also no compiler paths,
host probe overrides, image-format switches, or signing keys in the public API.

### Initramfs

`initramfs` constructs a deterministic, root-owned `newc` archive. It is a
normal build rule, independent of `linux_image`, so the same archive can be
paired with any compatible kernel or VM rule:

```starlark
load("@linux.bzl", "initramfs")

initramfs(
    name = "boot_files",
    character_devices = {
        "/dev/console": "5:1",
        "/dev/null": "1:3",
    },
    executables = {
        "/init": "//init",
    },
    files = {
        "/etc/motd": "motd",
    },
    symlinks = {
        "/bin/sh": "/bin/busybox",
    },
)
```

All archive paths are canonical absolute paths. Parent directories are created
automatically. `directories`, `files`, `executables`, `symlinks`, and
`character_devices` are the complete initial surface; file modes and ownership
are fixed to reproducible values.

### Rust-for-Linux modules

`linux_module(name, kernel, srcs, crate_root = None, deps = [])` builds one
out-of-tree Rust-for-Linux loadable module. It is a normal BUILD rule loaded
from the same root entry point:

```starlark
load("@linux.bzl", "linux_module")

linux_module(
    name = "hello",
    kernel = "@example_kernel//:kernel",
    srcs = ["hello.rs"],
)
```

The target's default output is `hello.ko`. `kernel` identifies the configured
kernel and supplies its generated headers, Rust SDK, symbol versions, and
module linker inputs. The rule follows that kernel's target platform, so there
is no separate architecture attribute. Set `crate_root` when `srcs` does not
make the crate root unambiguous. `deps` accepts other `linux_module` targets
built against the same configured kernel; cross-kernel dependencies are
rejected.

Rust-for-Linux support covers the cataloged Linux 6.12.x and 6.18.x kernels
targeting x86_64. The repository generator derives each release's crate graph
and flags from its kernel sources, while Bazel uses the exact stable Rust
1.97.0 toolchain registered by `linux.bzl`. Unsupported architectures, symbol
versioning, LTO/CFI, sanitizers, and Rust debug/BTF configurations fail during
analysis with an explicit diagnostic. `MODULE_VERSION` and module `version=`
or `srcversion=` metadata are not supported.

The standalone end-to-end workspace compiles this path, boots the kernel under
QEMU, inserts the resulting module, and checks its load marker:

```sh
cd e2e
bazel test //:linux_6_12_96_rust_module_test \
  //:linux_6_18_39_rust_module_test \
  --test_output=streamed
```

This rule builds a Rust-for-Linux `.ko`: native kernel code loaded through the
Linux module loader. It does not build eBPF bytecode. Aya builds and loads eBPF
programs through the kernel's BPF subsystem; the standalone
[`aya_e2e/`](aya_e2e/) workspace verifies that separate consumer workflow
against kernels produced by `linux.bzl`.

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
Hermetic LLVM 22.1.8. The kernel repository requires that exact profile, rather
than treating module `llvm` 0.8.14 as a minimum version, and the transitioned
graph rejects a non-Clang C/C++ toolchain.

## Supported configurations

| Area | Supported |
| --- | --- |
| Catalog releases | 6.12.96 and 6.18.39 |
| Target architectures | x86_64 and aarch64 |
| Repository evaluation | Pinned generator archives for Linux, macOS, and Windows on amd64 and arm64 |
| Build toolchain | Hermetic LLVM 22.1.8 through module `llvm` 0.8.14 |
| Images | x86 `bzImage`, arm64 `Image`, and `vmlinux` |
| Config variants | Base fragment plus named overlay fragments |
| Initramfs | Deterministic root-owned `newc` archives |
| In-tree modules | Loadable `.ko` files plus Kbuild module metadata |
| Out-of-tree modules | Rust-for-Linux `.ko` files on cataloged x86_64 kernels through `linux_module` |
| Kernel BPF/BTF | BPF syscall configurations and BTF-enabled `vmlinux` |
| VM verification | Hermetic QEMU boots with initramfs and module-load checks |

The two LTS lines are the maintained compatibility catalog. Other
integrity-pinned Linux 6.x releases may work, but are experimental until added
to that catalog and its release checks. The repository generator is published
for each host listed above; kernel compile actions still target the registered
Linux Hermetic LLVM toolchain.

The public kernel contract includes resolved configs, boot images, `vmlinux`,
`System.map`, kernel release metadata, configured in-tree modules, and their
installation metadata. The separate `initramfs` rule supplies boot userspace
archives, while `linux_module` builds out-of-tree Rust-for-Linux modules.
BPF-syscall and BTF-enabled kernel configurations are supported; eBPF program
compilation remains the responsibility of consumers such as Aya.

Kernel and module signing, `MODULE_VERSION`/module source-version metadata,
device-tree artifacts, and other unimplemented Kbuild products remain outside
the supported contract.
`CONFIG_X86_NATIVE_CPU` is rejected because `-march=native` would make an action
depend on worker CPU features that are absent from its cache key. Generated
graphs that require an unsupported artifact rule are rejected with an
actionable diagnostic.

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

Every base and variant package exposes the same projection labels:

| Label | Current contents |
| --- | --- |
| `:kernel` | Real architecture boot image and `LinuxKernelInfo` |
| `:image` | Real architecture boot image |
| `:vmlinux` | Real linked ELF kernel |
| `:config` | Resolved kernel configuration |
| `:system_map` | Real `System.map` |
| `:kernel_release` | Real computed kernel release |
| `:modules` | Configured in-tree loadable `.ko` files |
| `:module_symvers` | Kernel and in-tree module `Module.symvers` |
| `:modules_order` | Deterministic Kbuild module load order |
| `:modules_builtin` | Deterministic built-in module inventory |
| `:modules_builtin_modinfo` | Deterministic built-in module metadata |

The `:kernel` target's `DefaultInfo` contains only the boot image, so it can be
used directly by packaging and VM rules without selecting an output group.
When no loadable in-tree modules are configured, `:modules` is an empty file
set; the metadata labels remain available.

### Providers

Load public providers from the root entry point:

```starlark
load("@linux.bzl", "LinuxKernelInfo")
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
Starlark rule requires indexed content-graph metadata, verifies exact source
inputs and content identities, and checks every generated-header family and
compile environment before exposing the graph. The generator never consumes a
build output, which keeps module resolution valid and reproducible.

The build does not read ambient host tools or environment variables. All tools
are Bazel inputs, temporary paths are action-local, timestamps and release
metadata are normalized, and source downloads require integrity. Remote cache
and executor settings belong to the consuming workspace or CI environment;
this repository does not prescribe a service.

## Cache behavior

Source and object actions are separate, so changing one source file does not
invalidate a monolithic kernel-build action. Source inputs are classified:
ordinary compiles receive headers, bounded special lookups, and the exact
repository-generated closure of source-like includes rather than the complete
source archive.

Repository generation resolves the base config and every named overlay in one
graph invocation. Compile environments, exact source inputs, and
generated-header families are content-addressed, and variant images are emitted
as deltas from one canonical base graph. Equivalent configs therefore reference
the same object targets. Configs with only some identical generated
headers share those family inputs without conflating the remaining
generated-header tree. The public `@repo//:kernel` and
`@repo//variants/<name>:kernel` labels stay unchanged.

For the maintained x86_64 base, BTF, debug, and LZ4 invocation, 4,711 config
memberships resolve to 3,519 `LinuxObjectCompile` actions. Base and LZ4 share
all 1,152 object actions; generated-header family identities additionally
share 40 base/debug actions. BTF compiler flags differ, so BTF correctly shares
generated-header producers but no object compile actions.

For x86 boot images, the resolved config must select either
`CONFIG_KERNEL_GZIP=y` or `CONFIG_KERNEL_LZ4=y`. Other x86 payload compression
modes fail during repository evaluation rather than producing a mismatched
image.

## Examples

[`examples/`](examples/) is a standalone Bzlmod workspace containing:

- catalog-backed x86_64 and aarch64 kernels;
- named debug and LZ4 overlays;
- a deterministic initramfs built through the public `@linux.bzl` entry point;
- in-tree module and Kbuild metadata outputs; and
- aliases for real fixed image outputs.

From a repository checkout:

```sh
cd examples
bazel build @example_x86_64//:kernel
bazel build @example_x86_64//variants/debug:kernel
bazel build @example_x86_64//variants/lz4:kernel
bazel build @example_aarch64//:kernel
bazel build //:example_initramfs
```

The standalone compatibility suites exercise the runtime contracts:

```sh
cd e2e
bazel test //boot:init_test //cmd/qemuboot:qemuboot_test
bazel shutdown
bazel test //:linux_6_12_96_x86_64_boot_test \
  //:linux_6_12_96_x86_64_module_test
bazel shutdown
bazel test //:linux_6_12_96_aarch64_boot_test \
  //:linux_6_12_96_aarch64_module_test
bazel shutdown
bazel test //:kernel_outputs_x86_64_build_test \
  //:linux_6_18_39_x86_64_boot_test \
  //:linux_6_18_39_x86_64_module_test
bazel shutdown
bazel test //:kernel_outputs_aarch64_build_test \
  //:linux_6_18_39_aarch64_boot_test \
  //:linux_6_18_39_aarch64_module_test
bazel shutdown
bazel test //:linux_6_12_96_rust_module_test \
  //:linux_6_18_39_rust_module_test
bazel shutdown

cd ../aya_e2e
bazel test @aya//test/integration-test:vm_x86_64
bazel shutdown
bazel test @aya//test/integration-test:vm_aarch64
```

The e2e commands boot maintained kernels with a deterministic initramfs under
hermetic QEMU, verify configured module loading, and keep each configured
kernel graph in a fresh Bazel server to bound peak analysis memory. The final
two commands apply the same isolation to Aya's x86_64 and aarch64 eBPF
integration VMs. These are separate Bzlmod roots because Aya pins a nightly
Rust toolchain, while Rust-for-Linux modules use the stable toolchain registered
by `linux.bzl`.

## Development

The root workspace contains parser, generator, transition, rule, and tool tests:

```sh
bazel test //...
```

The standalone examples, QEMU compatibility workspace, and Aya consumer
workspace are excluded from root package discovery with `.bazelignore`; run
them from their own directories. Release preparation and generator
distribution are documented in [`RELEASING.md`](RELEASING.md).
