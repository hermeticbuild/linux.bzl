# linux.bzl

Bazel-native, hermetic Linux kernel builds with dynamically discovered
per-object actions, remote execution, and reusable action-cache entries.

The execution backend requires Bazel 9. The maintained CI matrix verifies
Bazel 9.1.0 and 9.2.0; Bazel 8 is not supported.

> [!WARNING]
> `linux.bzl` is experimental and pre-1.0. The public API may change while the
> Kconfig/Kbuild evaluator is extended. The supported contract is deliberately
> small. Recognized incompatible configurations are rejected explicitly.

## Quick start

Add `linux.bzl` and a hermetic C/C++ toolchain to `MODULE.bazel`:

```starlark
bazel_dep(name = "linux.bzl", version = "0.0.1")
bazel_dep(name = "llvm", version = "0.8.14")

# Copy `patches/llvm_kbuild_actions.patch` from the linux.bzl release into
# `//third_party` and export it from that package's BUILD.bazel.
single_version_override(
    module_name = "llvm",
    patch_strip = 1,
    patches = ["//third_party:llvm_kbuild_actions.patch"],
    version = "0.8.14",
)

register_toolchains(
    "@llvm//toolchain:all",
)

linux_source_repository = use_repo_rule(
    "@linux.bzl//:linux.bzl",
    "linux_source_repository",
)

linux_source_repository(
    name = "linux_6_18_39",
    version = "6.18.39",
)

linux_images = use_extension("@linux.bzl//:extensions.bzl", "linux_images")
linux_images.image(
    name = "example_kernel",
    config = "//kernel:kernel.config",
    platform = "@llvm//platforms:linux_x86_64",
    source = "@linux_6_18_39//:Kconfig",
)
use_repo(linux_images, "example_kernel")
```

When developing against a checkout, add:

```starlark
local_path_override(
    module_name = "linux.bzl",
    path = "/path/to/linux.bzl",
)
```

The referenced config is a normal exported source file:

```starlark
# kernel/BUILD.bazel
exports_files(["kernel.config"])
```

```text
# kernel/kernel.config
CONFIG_KERNEL_GZIP=y
# CONFIG_MODULES is not set
```

The source rule knows the pinned URL and integrity for maintained catalog
versions. The image extension transitions one kernel graph to the selected
platform. During action-time planning, the selected compiler reports its
target machine and Linux's own make logic derives the Kconfig architecture.

Build the boot image:

```sh
bazel build @example_kernel//:kernel
```

Build another fixed output directly:

```sh
bazel build @example_kernel//:vmlinux
```

No `BUILD.bazel` macro is required in the consuming repository. A normal Bazel
action resolves Kconfig and Kbuild using the selected compiler, and
`map_directory` expands that plan into fine-grained build actions.

## Public API

Public build rules and providers are loaded from the root `linux.bzl`.
Configured image repositories are declared through the `linux_images` module
extension in `extensions.bzl`. Source and image declarations are intended for
the root module: kernel source and product configs are application choices, not
transitive dependency resolution.

Because the module repository and its public file have the same name, Starlark
outside `MODULE.bazel` can use Bazel's shorthand label:

```starlark
load("@linux.bzl", "initramfs", "linux_module")
```

Inside `MODULE.bazel`, use
`use_repo_rule("@linux.bzl//:linux.bzl", "linux_source_repository")` for source
archives and `use_extension("@linux.bzl//:extensions.bzl", "linux_images")`
for configured images, as shown above.

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
| `source_overlays` | In-tree destination directories mapped to marker files in external source roots |
| `module_kbuild_roots` | Overlaid Kbuild directories mapped to Kconfig expressions controlling graph inclusion; use `"y"` for an unconditional root |
| `module_kconfig_roots` | Overlaid Kconfig files sourced by the root Kconfig |
| `module_make_vars` | Deterministic variables needed while parsing overlaid Kconfig/Kbuild files |
| `module_targets` | Public image-repository target names mapped to canonical in-tree `.ko` output paths |

`linux_images.image` declares a configured kernel image with this public
surface:

| Attribute | Meaning |
| --- | --- |
| `name` | Generated image facade repository name |
| `source` | Root `Kconfig` from `linux_source_repository` |
| `config` | Base Kconfig fragment |
| `config_mode` | Kconfig baseline: `default` or `allnoconfig` |
| `platform` | Linux target platform selecting a conforming C/C++ toolchain |

`linux_images.overlay` adds a named config fragment to an image:

| Attribute | Meaning |
| --- | --- |
| `image` | Name of a `linux_images.image` declaration |
| `name` | Stable variant name used below `variants/` |
| `config` | Overlay Kconfig fragment |

There is intentionally no `arch` attribute. The platform is the sole
toolchain-selection boundary. During planning, the selected compiler reports
its target machine and Linux's source Makefile/include graph derives `ARCH`,
`SRCARCH`, and `UTS_MACHINE` from that result. The base config therefore does
not need to repeat architecture symbols. The selected `CcToolchainInfo`
supplies the versioned `linux-kbuild-*` actions and their complete input
closure. No repository-generated configuration, architecture profile, or
object graph is involved. Repository and platform labels may be renamed.
The LLVM patch shown in the quick start adds that action contract to LLVM
0.8.14; Bazel requires override patches to be labels in the consuming root
module, so consumers must vendor the patch locally.
There are no public explicit Kbuild linker, compiler-path, host-probe,
image-format, or signing-key attributes.
Import each declared facade repository explicitly with `use_repo`.

### Initramfs

`initramfs` constructs a deterministic, root-owned `newc` archive. It is a
normal build rule, independent of configured image repositories, so the same
archive can be paired with any compatible kernel or VM rule:

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
from the same root entry point.

Rust-enabled kernels use the Rust toolchain registered by the consumer.
`linux.bzl` does not select or register a production Rust toolchain, and
C-only kernels do not require one:

```starlark
# MODULE.bazel
bazel_dep(name = "rules_rs", version = "0.0.102")

rust_toolchains = use_extension(
    "@rules_rs//rs/toolchains:module_extension.bzl",
    "toolchains",
)
rust_toolchains.toolchain(
    version = "1.97.0",
)
use_repo(rust_toolchains, "default_rust_toolchains")

register_toolchains("@default_rust_toolchains//:all")
```

The execution-time planner derives Rust configuration and arguments from the
configured kernel and the selected toolchain. There is no checked-in compiler
capability table or analysis-time compiler guess.

Load the rule from the public entry point:

```starlark
load("@linux.bzl", "linux_module")

linux_module(
    name = "hello",
    kernel = "@example_kernel//:kernel",
    srcs = ["hello.rs"],
)
```

The target's default output is `hello.ko`. `kernel` identifies the configured
kernel and supplies its resolved source, configuration, object tree, measured
toolsets, and probe results. There is no separate architecture attribute or
second compiler selection. Its mapped actions execute the SDK-provided tools
and verify that their C and host-C execution groups resolve the same execution
platform labels used to create the kernel SDK. Set
`crate_root` when `srcs` does not make the crate root unambiguous. `deps`
accepts other `linux_module`
targets built against the same configured kernel; cross-kernel dependencies
are rejected.

Rust-for-Linux coverage includes the cataloged Linux 6.12.x and 6.18.x kernels
targeting x86_64 and aarch64. External Rust modules are staged with a minimal
declarative Kbuild and run through the same source-derived Kconfig, Kbuild,
probe, and `map_directory` pipeline as the configured kernel. Compiler flags,
generated metadata, objtool, modpost, linking, and optional BTF stages come
from that native Kbuild graph rather than a separate Rust-module backend.

The standalone end-to-end workspace compiles this path, boots the kernel under
QEMU, inserts the resulting module, and checks its load marker:

```sh
cd e2e
bazel test //:linux_6_12_96_rust_module_test \
  //:linux_6_12_96_aarch64_rust_module_test \
  //:linux_6_18_39_rust_module_test \
  //:linux_6_18_39_aarch64_rust_module_test \
  --test_output=streamed
```

This rule builds a Rust-for-Linux `.ko`: native kernel code loaded through the
Linux module loader. It does not build eBPF bytecode. Aya builds and loads eBPF
programs through the kernel's BPF subsystem; the standalone
[`aya_e2e/`](aya_e2e/) workspace verifies that separate consumer workflow
against kernels produced by `linux.bzl`.

### C modules

`linux_cc_module(name, kernel, srcs, copts = [], deps = [], hdrs = [],
defines = [], local_defines = [])` builds one out-of-tree C loadable module
against a configured kernel with `CONFIG_MODULES=y`. `deps` may reference
other external modules built against the same configured kernel.

```starlark
load("@linux.bzl", "linux_cc_module")

linux_cc_module(
    name = "hello_module",
    kernel = "@example_kernel//:kernel",
    srcs = ["hello_module.c"],
)
```

The module consumes the default configured kernel directly; it does not need a
module-specific image or config overlay. The same rule and source shape are
supported for x86_64, aarch64, and armv7 kernels. As with
Rust modules, the `kernel` provider supplies the exact configured SDK tools
and rejects cross-kernel dependencies. Consumers may use the generated
`:kernel` label directly, use a fixed `native.alias` (including an alias
chain), or select among such labels in the module's `kernel` attribute. Module
actions use the SDK's compiler files, arguments, environment, and execution
requirements authoritatively, while checking that their target and host
execution groups resolve the same execution-platform labels as the kernel.

The rule stages a minimal declarative Kbuild for the declared C sources,
headers, defines, and compiler options. Native Kbuild selects the configured
GENKSYMS, source-version, objtool, modpost, link, and optional BTF stages; the
rule does not reconstruct those commands or maintain a separate compiler-flag
table.

Source integrations may add deterministic `module_make_vars`, but `M`,
`LIBELF_FLAGS`, `LIBELF_LIBS`, and `RUST_LIB_SRC` are owned by the external
module backend. Repository configuration rejects those names instead of
allowing a value to replace the SDK source root, host dependency flags, or Rust
source selection.

### Vendor Kbuild modules

Vendor modules that expect to live in the kernel tree are overlaid into the
Linux source repository. Their Kconfig and Kbuild roots then flow through the
same dynamic resolver as every upstream in-tree module;
there is no separate module rule or Make invocation.

```starlark
# MODULE.bazel
http_archive = use_repo_rule(
    "@bazel_tools//tools/build_defs/repo:http.bzl",
    "http_archive",
)
http_archive(
    name = "sai_bcm_modules",
    urls = ["https://github.com/sonic-net/sonic-buildimage/archive/0058681761a86abd324514d817faf0720aa27405.tar.gz"],
    integrity = "sha256-W907LnUlUHP0wluU7aIaD2pm6LTn2o74T310/ePHeIQ=",
    strip_prefix = "sonic-buildimage-0058681761a86abd324514d817faf0720aa27405",
    build_file_content = 'exports_files(glob(["**"]))',
    # The SONiC end-to-end fixture patches this vendor revision with a
    # Kconfig entry point defining LINUX_NGBDE.
    patches = ["//third_party:sai_bcm_kconfig.patch"],
    patch_args = ["-p1"],
)

linux_source_repository(
    name = "linux_6_18_39",
    version = "6.18.39",
    source_overlays = {
        "drivers/net/ethernet/broadcom/sdklt": "@sai_bcm_modules//:platform/broadcom/saibcm-modules/sdklt/Makefile",
    },
    module_kbuild_roots = {
        "drivers/net/ethernet/broadcom/sdklt/linux/bde": "LINUX_NGBDE",
    },
    module_kconfig_roots = [
        "drivers/net/ethernet/broadcom/sdklt/linux/bde/Kconfig",
    ],
    module_make_vars = {
        "BDE_CPPFLAGS": "-UBCMDRD_INCLUDE_CUSTOM_CONFIG",
        "SDK": "$(srctree)/drivers/net/ethernet/broadcom/sdklt",
    },
    module_targets = {
        "linux_ngbde": "drivers/net/ethernet/broadcom/sdklt/linux/bde/linux_ngbde.ko",
    },
)
```

If a vendor tree has Kconfig entry points, list their in-tree paths in
`module_kconfig_roots`. Selecting the vendor symbol as `m` makes its module a
normal output of `@configured_kernel//:modules`, including the usual objtool,
modpost, module-link, and optional BTF stages.

`module_targets` is the immutable public-product contract for an overlaid
source tree. Every configured image generated from that source automatically
exposes the named, manifest-validated file target. Consumers build
`@configured_kernel//:linux_ngbde`; they do not declare a module rule or repeat
the `.ko` path in a `BUILD.bazel` file. If Kconfig excludes the module for that
image, building its file target fails the manifest check instead of returning
an unrelated or stale file.

`module_kbuild_roots` values use Kconfig expression syntax (`X86`, not
`CONFIG_X86`). linux.bzl lowers conditional expressions to hidden tristate
selectors before adding the directory to the root Kbuild. Use `"y"` when the
root must be traversed for every configured architecture.

## Why

Wrapping `make` in one Bazel action hides the kernel build graph from Bazel.
Any source or configuration change invalidates that action, and remote workers
cannot cache or schedule the individual compile steps.

`linux.bzl` instead:

- evaluates the relevant Kconfig and Kbuild language without invoking `make`;
- emits an action-time plan whose exact source and action edges are expanded by
  Bazel's `map_directory` API;
- obtains compilers and binary utilities from registered Bazel toolchains;
- declares source, generated-header, tool, and response-file inputs explicitly;
  and
- lets Bazel schedule and cache compilation at object granularity.

Kconfig and Kbuild run capability probes with the tools selected by Bazel.
Compiler, assembler, linker, and optional feature symbols therefore describe
the real registered toolchain rather than a checked-in compiler baseline.

## Supported configurations

| Area | Supported |
| --- | --- |
| Catalog releases | 6.12.96 and 6.18.39 |
| CI-tested target architectures | x86_64, aarch64, and armv7, derived by Linux from the selected compiler |
| Repository evaluation | Thin source and configured-image facade repositories; no downloaded graph generator |
| Build toolchain | GCC or Clang toolchains implementing the versioned `linux-kbuild-*` action contract |
| Images | x86_64 `bzImage`, aarch64 `Image`, and armv7 `zImage` |
| Config variants | Base fragment plus named overlay fragments |
| Initramfs | Deterministic root-owned `newc` archives |
| In-tree modules | Loadable `.ko` files plus Kbuild module metadata |
| Out-of-tree modules | C `.ko` files through `linux_cc_module` on all tested targets; Rust-for-Linux `.ko` files through `linux_module` on x86_64 and aarch64 |
| Module metadata | Native Kbuild GENKSYMS, source-version, modpost, link, and optional BTF stages selected by the maintained kernels |
| Trusted keyrings | Empty built-in trusted keyring for verification consumers; no embedded certificates or signing keys |
| Kernel BPF/BTF | BPF syscall configurations, BTF-enabled `vmlinux`, and module BTF with Rust+DWARF5 kernels |
| VM verification | Hermetic QEMU boots with initramfs and module-load checks |

The two LTS lines are the maintained compatibility catalog. Other
integrity-pinned Linux 6.x releases may work, but are experimental until added
to that catalog and its release checks. Kernel actions target the registered
toolchain selected by the image platform.

The public kernel contract includes resolved configs, native boot images,
`System.map`, kernel release metadata,
configured in-tree modules, and their
installation metadata. The separate `initramfs` rule supplies boot userspace
archives, while `linux_cc_module` and `linux_module` build out-of-tree C and
Rust-for-Linux modules respectively.
BPF-syscall and BTF-enabled kernel configurations are supported; eBPF program
compilation remains the responsibility of consumers such as Aya.
External modules use the same Kbuild-selected metadata pipeline as in-tree
modules. The planner reports a source-defined command or artifact rule it
cannot lower instead of substituting a hand-maintained approximation.

Kernel and module signing, embedded trusted or revocation certificates,
compiled device-tree artifacts, and other unimplemented Kbuild products remain
outside the supported contract. Action plans that require an unsupported
artifact rule are rejected with an actionable diagnostic.

## Source repositories

`linux_source_repository` fetches the upstream tree once. Multiple configured
kernels and overlays can reuse it.

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

Every source file and directory in the root package is a public target. This
allows consumer rules to use directory labels such as `@linux_custom//:include`
and `@linux_custom//:arch/arm/boot/dts` as include roots. The `:dtb_sources`
filegroup contains the upstream `.dts`, `.dtsi`, and DT binding headers for
consumers such as `rules_devicetree`; linux.bzl does not compile those sources
itself.

## Configs and variants

The base input is a Kconfig fragment. Its architecture comes from the compiler
selected by the `linux_images.image` platform, so the fragment can stay
architecture-neutral and need not set architecture symbols. The planner
derives Linux's make variables, applies Kconfig defaults, dependencies,
selects, and implies, and runs live tool probes. For other symbols, an absent
assignment follows Kconfig semantics; use
`# CONFIG_NAME is not set` for a deliberate unset.

Named overlays contain only deliberate assignments and unsets:

```text
CONFIG_DEBUG_KERNEL=y
# CONFIG_RANDOMIZE_BASE is not set
```

Declare them on the same image extension:

```starlark
linux_images = use_extension("@linux.bzl//:extensions.bzl", "linux_images")
linux_images.image(
    name = "example_kernel",
    config = "//kernel:x86_64.config",
    platform = "@llvm//platforms:linux_x86_64",
    source = "@linux_6_18_39//:Kconfig",
)
linux_images.overlay(
    name = "debug",
    config = "//kernel:debug.config",
    image = "example_kernel",
)
use_repo(linux_images, "example_kernel")
```

Each overlay is merged onto the base fragment and resolved independently.
Variant outputs use the same fixed contract:

```sh
bazel build @example_kernel//variants/debug:kernel
bazel build @example_kernel//variants/debug:vmlinux
```

Overlay names are stable label path components. Rename an overlay only when
changing its public target names is intentional. Names use lowercase ASCII
letters, digits, `_`, and `-`; `base` and Windows-reserved names are rejected.

## Fixed output contract

Every base and variant package exposes the same projection labels:

| Label | Current contents |
| --- | --- |
| `:kernel` | Native boot image and `LinuxKernelInfo` |
| `:image` | Native boot image |
| `:vmlinux` | Real linked ELF kernel |
| `:config` | Resolved kernel configuration |
| `:system_map` | Real `System.map` |
| `:kernel_release` | Real computed kernel release |
| `:modules` | One directory artifact containing configured in-tree `.ko` files at canonical Kbuild-relative paths |
| `:module_symvers` | Kernel and in-tree module `Module.symvers` |
| `:modules_order` | Deterministic Kbuild module load order |
| `:modules_builtin` | Deterministic built-in module inventory |
| `:modules_builtin_modinfo` | Deterministic built-in module metadata |

The `:kernel` target's `DefaultInfo` contains one native image: x86_64
uses `bzImage`, aarch64 uses `Image`, and armv7 uses `zImage`. This output can
be used directly by packaging and VM rules without selecting an output group. When no
loadable in-tree modules are configured, `:modules` is an empty directory
artifact; the metadata labels remain available. Source repositories may also
declare named `module_targets`; each becomes an ordinary, manifest-validated
file label in every generated image repository:

```shell
bazel build @example_kernel//:linux_ngbde
```

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

`version` is a string. Every other field is a concrete `File`. In particular,
`arch` is intentionally an action-produced file, not the former
analysis-time profile string. Its newline-terminated contents are the exact
Linux `ARCH` selected by the source Makefiles from the configured compiler.
This keeps arbitrary toolchains and source-defined architectures dynamic
without restoring a platform-to-architecture table.

## Toolchains and hermeticity

`platform` is mandatory on every `linux_images.image` tag. It must carry a
Linux OS constraint and select a conforming C/C++ toolchain. The extension
applies that platform transition once at the public facade and analyzes one
kernel graph after the transition. Toolchains expose the exact
`linux-kbuild-{host,target}-{ar,as,cc,cxx,ld,nm,objcopy,objdump,ranlib,readelf,strip}`
actions with one `__LINUX_BZL_KBUILD_ARGS_V1__` argument sentinel. Every tool
and its support files must be present in `CcToolchainInfo.all_files`;
feature-selected static linker runtimes are obtained from
`CcToolchainInfo.static_runtime_lib()` and added to the corresponding action
closure automatically. There is no fallback to standard C++ actions,
executable basename discovery, or host tools.

The build does not read ambient host tools or environment variables. All tools
are Bazel inputs, temporary paths are action-local, timestamps and release
metadata are normalized, and source downloads require integrity. Remote cache
and executor settings belong to the consuming workspace or CI environment;
this repository does not prescribe a service.

## Cache behavior

Source and object actions are separate, so changing one source file does not
invalidate a monolithic kernel-build action. The content-addressed plan records
the exact TreeFile dependencies expanded by `map_directory`; Bazel schedules
and caches those actions individually. Public base and variant labels remain
unchanged even though their action graph is discovered at execution time.

For x86 boot images, the resolved config must select a supported payload
compression mode. Unsupported modes fail during execution-time planning rather
than producing a mismatched image.

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
  //:linux_6_12_96_aarch64_rust_module_test \
  //:linux_6_18_39_rust_module_test \
  //:linux_6_18_39_aarch64_rust_module_test
bazel shutdown

cd ../aya_e2e
bazel test @aya//test/integration-test:vm_aarch64 \
  @aya//test/integration-test:vm_x86_64
```

The e2e commands boot maintained kernels with a deterministic initramfs under
hermetic QEMU, verify configured module loading, and keep each configured
kernel in a fresh Bazel server to bound peak analysis memory. Aya's x86_64 and
aarch64 VMs intentionally run in one Bazel invocation. These
are separate Bzlmod roots because each consumer owns its Rust toolchain: the
e2e workspace registers stable Rust 1.97.0, while Aya keeps its pinned nightly.

## Development

The root workspace contains planner, transition, rule, and tool tests:

```sh
bazel test //...
```

The standalone examples, QEMU compatibility workspace, and Aya consumer
workspace are excluded from root package discovery with `.bazelignore`; run
them from their own directories. Release preparation is documented in
[`RELEASING.md`](RELEASING.md).
