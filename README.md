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
bazel_dep(name = "linux.bzl", version = "0.0.1")
bazel_dep(name = "llvm", version = "0.8.14")

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
versions. The image extension derives the Linux target profile from the
selected platform, supplies its architecture symbols while resolving the
fragment through Kconfig, and rejects explicit config assignments that
contradict the platform.

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

`linux_images.image` declares a configured kernel image with this public
surface:

| Attribute | Meaning |
| --- | --- |
| `name` | Generated image facade repository name |
| `source` | Root `Kconfig` from `linux_source_repository` |
| `config` | Base Kconfig fragment |
| `config_mode` | Kconfig baseline: `default` or `allnoconfig` |
| `platform` | Linux x86_64, aarch64, or armv7 target platform selecting Clang |

`linux_images.overlay` adds a named config fragment to an image:

| Attribute | Meaning |
| --- | --- |
| `image` | Name of a `linux_images.image` declaration |
| `name` | Stable variant name used below `variants/` |
| `config` | Overlay Kconfig fragment |

There is intentionally no `arch` attribute. The platform is the sole source of
the target profile and must carry Linux plus exactly one supported CPU
constraint: x86_64, aarch64, or armv7. The base config does not need to repeat
`CONFIG_X86`, `CONFIG_X86_64`, `CONFIG_ARM64`, or `CONFIG_ARM`; repository
generation supplies the platform-selected architecture to Kconfig. An explicit architecture
selection or unset that contradicts the platform is rejected. The platform
also selects a matching LLVM/Clang toolchain, which must expose `llvm-nm` and
`llvm-objcopy` through `CcToolchainInfo.all_files`. Repository and platform
labels may be renamed. There are no public graph-profile, explicit Kbuild
linker, compiler-path, host-probe, image-format, or signing-key attributes.
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

Rust-enabled kernels use the standard rules_rs/rules_rust toolchain registered
by the consumer. `linux.bzl` does not select or register a production Rust
toolchain, and C-only kernels do not require one:

```starlark
# MODULE.bazel
bazel_dep(name = "rules_rs", version = "0.0.98")

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

The maintained kernels require at least Rust 1.78.0. A consumer may register
any supported newer stable toolchain or a pinned nightly such as
`version = "nightly/2026-06-24"`. The same rules_rs repository also registers
the Rust-analyzer toolchain that exposes the matching `rustc_srcs`; omitting
`rust_analyzer_version` keeps those sources on the compiler version.
`linux.bzl` resolves both standard toolchain types, probes the selected rustc
release and embedded LLVM version during actions, writes those exact values
into the resolved kernel config, and applies source-derived version predicates.
There is no separate checked-in Rust source archive or analysis-time compiler
guess.

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
kernel and supplies its generated headers, Rust SDK, symbol versions, and
module linker inputs. The rule follows that kernel's target platform, so there
is no separate architecture attribute. Set `crate_root` when `srcs` does not
make the crate root unambiguous. `deps` accepts other `linux_module` targets
built against the same configured kernel; cross-kernel dependencies are
rejected.

Rust-for-Linux support covers the cataloged Linux 6.12.x and 6.18.x kernels
targeting x86_64 and aarch64. The repository generator derives each release's
crate graph, built-in or generated target specification, and compiler flags
from its kernel sources. Full DWARF5 debug information, kernel BTF, and module
BTF are supported together. DWARF4, toolchain-default/reduced/split/compressed
debug information, symbol versioning, LTO/CFI, and sanitizers remain explicit
errors. Rust modules also reject `MODULE_VERSION` and module `version=` or
`srcversion=` metadata; source-version manifests are currently modeled only
for C and assembly module objects.

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

`linux_cc_module(name, kernel, srcs, copts = [], deps = [])` builds one
out-of-tree C loadable module against a configured kernel with
`CONFIG_MODULES=y`. The initial public surface accepts exactly one `.c` source;
`deps` may reference other external modules built against the same configured
kernel.

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
Rust modules, the `kernel` provider fixes the target platform and rejects
cross-kernel dependencies.

When the configured kernel enables `CONFIG_MODVERSIONS`, linux.bzl generates
GENKSYMS CRCs for C and assembly exports and carries them through in-tree and
out-of-tree C module modpost actions. Linux 6.18's basic format is supported;
extended/DWARF formats and Rust modules with symbol versioning remain explicit
errors.

### Vendor Kbuild modules

Vendor modules that expect to live in the kernel tree are overlaid into the
Linux source repository. Their Kconfig and Kbuild roots then flow through the
same resolver and compact graph generator as every upstream in-tree module;
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
)

linux_source_repository(
    name = "linux_6_18_39",
    version = "6.18.39",
    source_overlays = {
        "drivers/net/ethernet/broadcom/sdklt": "@sai_bcm_modules//:platform/broadcom/saibcm-modules/sdklt/Makefile",
    },
    module_kbuild_roots = {
        "drivers/net/ethernet/broadcom/sdklt/linux/bde": "X86",
    },
    module_make_vars = {
        "BDE_CPPFLAGS": "-UBCMDRD_INCLUDE_CUSTOM_CONFIG",
        "SDK": "$(srctree)/drivers/net/ethernet/broadcom/sdklt",
    },
)
```

If a vendor tree has Kconfig entry points, list their in-tree paths in
`module_kconfig_roots`. Selecting the vendor symbol as `m` makes its module a
normal output of `@configured_kernel//:modules`, including the usual objtool,
modpost, module-link, and optional BTF stages.

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
- emits a Bazel target for each supported generated file, object, archive, and
  final image step;
- obtains compilers and binary utilities from registered Bazel toolchains;
- declares source, generated-header, tool, and response-file inputs explicitly;
  and
- lets Bazel schedule and cache compilation at object granularity.

Repository generation resolves Kconfig and Kbuild against a deterministic
LLVM 22.1.8 capability baseline. Consumers may select a newer Clang toolchain;
doing so opts into treating it as compatible with that baseline rather than
probing it. The transitioned graph rejects a non-Clang toolchain or an explicit
config architecture that disagrees with the platform-selected target profile.

## Supported configurations

| Area | Supported |
| --- | --- |
| Catalog releases | 6.12.96 and 6.18.39 |
| Target architectures | x86_64, aarch64, and armv7, inferred from the target platform |
| Repository evaluation | Pinned generator archives for Linux, macOS, and Windows on amd64 and arm64 |
| Build toolchain | Clang with LLVM 22.1.8 baseline semantics from the published Hermetic LLVM release |
| Images | x86_64 `bzImage`, aarch64 `Image`, and armv7 `zImage` |
| Config variants | Base fragment plus named overlay fragments |
| Initramfs | Deterministic root-owned `newc` archives |
| In-tree modules | Loadable `.ko` files plus Kbuild module metadata |
| Out-of-tree modules | C `.ko` files through `linux_cc_module` on all three profiles; Rust-for-Linux `.ko` files through `linux_module` on x86_64 and aarch64 |
| Module symbol versions | Classic Linux 6.12 and Linux 6.18 basic GENKSYMS for C and assembly objects and C modules |
| Module source versions | `MODULE_VERSION` for C modules; `CONFIG_MODULE_SRCVERSION_ALL` for in-tree C/assembly modules and out-of-tree C modules |
| Trusted keyrings | Empty built-in trusted keyring for verification consumers; no embedded certificates or signing keys |
| Kernel BPF/BTF | BPF syscall configurations, BTF-enabled `vmlinux`, and module BTF with Rust+DWARF5 kernels |
| VM verification | Hermetic QEMU boots with initramfs and module-load checks |

The two LTS lines are the maintained compatibility catalog. Other
integrity-pinned Linux 6.x releases may work, but are experimental until added
to that catalog and its release checks. The repository generator is published
for each host listed above; kernel compile actions target the registered Clang
toolchain selected by the image platform.

The public kernel contract includes resolved configs, native boot images or the
documented `vmlinux` fallback, `System.map`, kernel release metadata,
configured in-tree modules, and their
installation metadata. The separate `initramfs` rule supplies boot userspace
archives, while `linux_cc_module` and `linux_module` build out-of-tree C and
Rust-for-Linux modules respectively.
BPF-syscall and BTF-enabled kernel configurations are supported; eBPF program
compilation remains the responsibility of consumers such as Aya.
C modules may use `MODULE_VERSION`, and configured kernels may enable
`CONFIG_MODULE_SRCVERSION_ALL` for C and assembly modules; linux.bzl generates
the Kbuild source and dependency manifests consumed by modpost. Direct
user-authored `srcversion=` metadata remains rejected because modpost owns that
value.

Kernel and module signing, embedded trusted or revocation certificates,
extended or DWARF module versions, Rust module source versions, compiled
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

Every source file and directory in the root package is a public target. This
allows consumer rules to use directory labels such as `@linux_custom//:include`
and `@linux_custom//:arch/arm/boot/dts` as include roots. The `:dtb_sources`
filegroup contains the upstream `.dts`, `.dtsi`, and DT binding headers for
consumers such as `rules_devicetree`; linux.bzl does not compile those sources
itself.

## Configs and variants

The base input is a Kconfig fragment. Its architecture comes from the
`linux_images.image` platform, so the fragment can stay architecture-neutral
and need not set `CONFIG_X86`, `CONFIG_X86_64`, `CONFIG_ARM64`, or `CONFIG_ARM`.
Repository generation supplies
the selected architecture and then applies Kconfig defaults, dependencies,
selects, and implies using the fixed LLVM capability baseline. Explicit
architecture assignments are still validated: selecting another profile, or
deliberately unsetting a symbol required by the platform, is an error. For
other symbols, an absent assignment follows Kconfig semantics; use
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
letters, digits, `_`, and `-`; `base`, Windows-reserved names, and names that
collide after target-name sanitization are rejected.

## Fixed output contract

Every base and variant package exposes the same projection labels:

| Label | Current contents |
| --- | --- |
| `:kernel` | Profile image (or documented fallback) and `LinuxKernelInfo` |
| `:image` | Profile image or documented fallback |
| `:vmlinux` | Real linked ELF kernel |
| `:config` | Resolved kernel configuration |
| `:system_map` | Real `System.map` |
| `:kernel_release` | Real computed kernel release |
| `:modules` | Configured in-tree loadable `.ko` files |
| `:module_symvers` | Kernel and in-tree module `Module.symvers` |
| `:modules_order` | Deterministic Kbuild module load order |
| `:modules_builtin` | Deterministic built-in module inventory |
| `:modules_builtin_modinfo` | Deterministic built-in module metadata |

The `:kernel` target's `DefaultInfo` contains one profile-native image: x86_64
uses `bzImage`, aarch64 uses `Image`, and armv7 uses `zImage`. This output can
be used directly by packaging and VM rules without selecting an output group. When no
loadable in-tree modules are configured, `:modules` is an empty file set; the
metadata labels remain available.

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

`platform` is mandatory on every `linux_images.image` tag. It must carry a
Linux OS constraint and exactly one of the x86_64, aarch64, or armv7 CPU
constraints, and it must select a matching LLVM/Clang toolchain.
The extension applies that platform transition once at the public facade,
selects only the corresponding architecture graph, and leaves the selected
private graph transition-free. Analysis rejects non-Clang compilers,
contradictory config architecture assignments, and toolchains that do not
expose `llvm-nm` and `llvm-objcopy` through `CcToolchainInfo.all_files`. The
supported toolchain uses LLVM 22.1.8 from the published Hermetic LLVM release
referenced in the quick start. Repository and platform
labels may be renamed, but using another LLVM packaging or version is outside
the supported contract.

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

For the maintained x86_64 base, BTF, debug, and LZ4 invocation, 4,727 config
memberships resolve to 3,533 `LinuxObjectCompile` actions. Base and LZ4 share
all 1,156 object actions; generated-header family identities additionally
share 38 base/debug actions. BTF compiler flags differ, so BTF correctly shares
generated-header producers but no object compile actions.

### Inspecting object inputs

`@linux.bzl//tools:linuxobjectinputreport` summarizes the logical inputs
attached to Linux compile actions. Generate the action graph with `deps(...)`
so it contains the actions that produce generated compile inputs:

```sh
bazel build //path/to:kernel
bazel aquery \
  --output=jsonproto \
  --include_artifacts \
  --noinclude_commandline \
  'deps(//path/to:kernel)' > /tmp/linux-object-inputs.json
bazel run @linux.bzl//tools:linuxobjectinputreport -- \
  -input /tmp/linux-object-inputs.json \
  -execroot "$(bazel info execution_root)"
```

Use the same configuration and platform flags for the build and `aquery`.
The report filters `LinuxObjectCompile` actions by default. Passing
`-mnemonic` replaces that default; repeat the flag to report several action
classes as one population. Do not filter the `aquery` itself with
`mnemonic(...)` when producer fanout is needed, because that removes the
producer actions from the JSON graph.

`-execroot` is optional. When present, it measures materialized input file
bytes in the current local execution-root snapshot, deduplicated by local file
identity. Build the target first to make more generated inputs available
locally; unavailable inputs are reported as unknown rather than zero bytes.
With remote execution or minimal output downloading, use
`--remote_download_all` when complete local byte coverage is required.

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
kernel graph in a fresh Bazel server to bound peak analysis memory. Aya's
x86_64 and aarch64 VMs intentionally run in one Bazel invocation so CI can
measure shared action-cache behavior and deduplication in one profile. These
are separate Bzlmod roots because each consumer owns its Rust toolchain: the
e2e workspace registers stable Rust 1.97.0, while Aya keeps its pinned nightly.

## Development

The root workspace contains parser, generator, transition, rule, and tool tests:

```sh
bazel test //...
```

The standalone examples, QEMU compatibility workspace, and Aya consumer
workspace are excluded from root package discovery with `.bazelignore`; run
them from their own directories. Release preparation and generator
distribution are documented in [`RELEASING.md`](RELEASING.md).
