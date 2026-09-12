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
bazel_dep(name = "llvm", version = "0.8.18")

# Copy `patches/llvm_kbuild_actions.patch` from the linux.bzl release into
# `//third_party` and export it from that package's BUILD.bazel.
single_version_override(
    module_name = "llvm",
    patch_strip = 1,
    patches = ["//third_party:llvm_kbuild_actions.patch"],
    version = "0.8.18",
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
| `source_overlays` | In-tree destination directories mapped to marker files in external source roots; exact `BUILD`/`BUILD.bazel` metadata files are omitted and case-only variants are rejected |
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
0.8.18; Bazel requires override patches to be labels in the consuming root
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
bazel_dep(name = "rules_rs", version = "0.0.107")

rust_toolchains = use_extension(
    "@rules_rs//rs/toolchains:module_extension.bzl",
    "toolchains",
)
rust_toolchains.toolchain(
    version = "1.97.0",
)
use_repo(rust_toolchains, "default_rust_toolchains")

register_toolchains("@default_rust_toolchains//...")
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

Overlay repositories may contain Bazel metadata. Regular files named exactly
`BUILD` or `BUILD.bazel` are not copied into the Linux source repository, so
they cannot split its single Bazel package. Case-only variants such as `Build`
or `BUILD.BAZEL` are rejected because they behave differently on
case-insensitive filesystems. Patch or rename a meaningful vendor `Build` file
before overlaying it. Directories with those names remain ordinary source
directories and are traversed normally.

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

The `:config` label and the `config` output group resolve Kconfig directly from
the measured compiler environment and configured source/overlay inputs. They do
not require family guard discovery, kernel objects, or final image linking.
`LinuxKernelInfo` and module SDKs retain the final execution configuration; the
early public projection does not replace their configured object-tree inputs.

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
Source-owned metadata scripts receive stable `KBUILD_BUILD_USER` and
`KBUILD_BUILD_HOST` defaults of `bazel`; explicit nonempty values remain declared
Make inputs instead of being discovered from the remote worker.

## Cache and in-invocation reuse behavior

For supported precise kernel compiler actions--C, C++, and compiler-preprocessed
assembly--the content-addressed plan records the exact immutable translation
unit and header TreeFiles expanded by `map_directory`. Bazel schedules and
caches the source projection and object actions individually, so an edit
invalidates only objects whose projected closure contains that source. This
exact source-edit reuse does not currently extend to Rust or source namespaces
outside the kernel tree; unsupported action shapes remain opaque.

The base image and all named overlays are reduced symmetrically into one family
graph. Structurally equivalent variant instances first union their exact source
and config dependencies, then receive semantic node identities independent of
variant order and variant-specific storage paths. Identical semantic inputs
therefore select one node and action across configurations. Within one Bazel
invocation, that shared action is part of the graph once; this is graph
deduplication, not an action-cache lookup or hit. Variant-specific Kbuild probe
fragments are likewise merged into one deterministic, content-addressed union
plan, expanded once with the host execution owner and once with the target
execution owner.

Generated-input reuse starts with an initial family plan and ends with a
verified replay. The initial plan records the
ordinary generated headers that stop dependency analysis and declared auxiliary
executables used by complete compiler recipes, reduces the complete family with
full configuration inputs, and selects a bounded dependency-closed set of
generators. Executable candidates must serve an already-precise compiler or a
compiler with a recorded generated-header demand; completeness alone does not
make an otherwise opaque compiler a prospective sharing benefit.
An optional numeric-header tier also considers exact declared bindings of
opaque compilers, independently of where their source scan stopped. It checks
the original configured generator contract; a bound path is neither a read
receipt nor proof that the header is configuration-independent.
After those actions complete, replay reads their actual output bytes
while checking the original lowering and tool/config contracts. Precise consumers
can then stage equal byte/mode-addressed headers at their original include paths,
independently of the generating variant. Header receipts alone never authorize
changes to executable, sequence, auxiliary-command, or observed-state dependencies.

A separate executed-artifact observation authenticates every selected action's
complete output vector, including executable permissions and sidecar writer
identities. For a precisely analyzed, complete compiler envelope, an executable
at its unchanged declared staging path can be replaced by an exact content-backed
copy. Exclusively internal sidecar-base inputs can use the same mechanism, but
their original envelope bytes and writers are preserved: equal payloads from
different writers are not equal states. Sequence edges, public views, ambiguous
aliases, mutable staging paths, and unsupported consumers retain their original
dependencies. The content tree is only a lookup catalog for `map_directory`;
each copy/compiler action receives its exact child files, not the whole catalog.

Planner-owned sidecars in the public metadata tree can also feed exclusively
internal compiler state. Their original producers and public views remain
unchanged; authenticated consumer copies use the private prehost tree. This is
disabled when a whole-prehost-tree consumer could acquire those new copies as
additional inputs. Present/deleted states retain their exact writer records,
and neither equal payloads nor metadata filenames alone establish equivalence.
The initial cut considers these sidecar generators for already-precise compilers
and consumers of prospective header/executable groups. It selects ordinary
producer outputs and observes their complete output vectors; a sidecar is never
treated as a generated header. A validation-private ordinary root retains both
generator versions and their exact byte-comparison obligation in the cut, without
becoming public. Exhausting this optional discovery budget leaves
the original header/executable candidates intact.

Selection groups corresponding logical prerequisites across variants and
prioritizes the number of compiler instances they could unblock. A group's
complete dependency closure must fit the execution budget and either increase
the number of prospective consumers outside that closure, or complement an
already selected consumer without newly pinning any previously useful compiler.
This admits a header and executable needed by the same compiler without trading
away an existing sharing opportunity for an equal-count late dependency.
After this baseline selection, numeric-header groups may additionally pin
prerequisite compilers when their unique remaining consumer opportunities
number at least the newly pinned compiler nodes. The complete closure still
has to fit the same budgets. This scheduling heuristic does not guarantee a
net reuse improvement: pinning preserves conservative identities, and the
production reuse regression gate remains the acceptance check.
Selection is deterministic and bounded to 128 group attempts, without filename
or toolchain allowlists. Rejected groups remain conservative; exceeding
an optimization budget does not fail an otherwise valid build. Malformed
contracts still fail validation.

Before publishing the final graph, replay verifies that every pre-executed
action still has exactly the same semantic identity and complete output vector.
The final expansion copies all its slots, including sidecars, without rerunning
its recipe or rewriting provenance. Verification gates template expansion; its
whole-family seal is not an input to every compiler action. An unrelated change
to that seal therefore does not by itself invalidate unchanged compiler inputs.
Authenticated execution roots retain generators even when replacing a header
removes their last compiler edge; this adds no compiler inputs or public views.
This is a first-frontier observation pass, not an assumption that all generated
headers have become known: unresolved or unsupported consumers remain opaque,
and an execution-cut mismatch fails before final graph publication.

The family plan is split into four execution shards: `prehost={prehost}`,
`bootstrap={bootstrap}`, `host={host}`, and `target={prep,target}`. Each shard
retains the shared lexical node index and toolsets, but carries only its local
nodes, referenced recipes, sources, and Kconfig capsule bytes; compact
descriptors hand prior-shard outputs forward.

Materialized input sets are persistent, content-addressed radix trees rather
than duplicated lists of every ancestor's files. Consumers and variants share
unchanged subtrees, while each entry preserves its exact source or producer
output and target namespace. Consumer-specific usage flags are projected through
a shared immutable-subtree cache and sparse path updates. A bounded store-local
cache also reuses conflict-free unions; conflicting inputs still go through the
caller's resolver on every merge. This bounds retained frontier state by unique
subtrees; some path-sensitive analysis still creates temporary flattened views,
so it does not make every planning pass linear.

Family recipe actions retain the same exact manifest, source, and producer
leaf inputs while transporting their bindings compactly. One typed root
manifest locates its content-addressed child witnesses; one already-declared
producer leaf per physical store anchors its provenance-addressed layout.
Bounded ordinal packs assign the verified producer keys to those stores.
Staging destinations never locate artifacts, whole stores are not added as
inputs, and Bazel still maps the typed anchors using the complete action input
set. Distinct configured stores may remain distinct or legally converge under
path mapping. This reduces argument transport, not dependency requirements or
the proof needed for cross-config compiler reuse.

Large recipe argument vectors use explicit, bounded JSON chunks because Bazel
9.1/9.2 action templates drop `Args.use_param_file` settings. Small actions keep
their direct arguments. Chunk writers retain the consumer's complete input and
tool closures so both actions make the same path-mapping decision, including
when cross-configuration input paths collide. This adds transport actions for
large nodes; it does not change recipe identity or increase compiler reuse.

For precise compiler nodes, the planner scans the translation unit and literal
include closure for `CONFIG_*` references and stages a filtered six-file
Kconfig capsule: the resolved `.config`, `auto.conf`, `auto.conf.cmd`,
`autoconf.h`, `rustc_cfg`, and `kernel.release`. Each translation unit starts
from the selected configured compiler's probed predefines and configured and
source-selected `-D`/`-U` operations. Because a predefine dump does not enumerate
every compiler builtin, the planner also collects a bounded set of reserved
macro names from immutable source conditional directives and asks the selected
compiler whether each is defined. Source discovery supplies query hints, not
absence proofs; unsupported or unqueried names stay unknown. Family variants
share this source-name inventory for one sequential planning action, keyed by
their immutable source-root bindings. Each variant retains its own name union
and compiler queries and answers; the inventory does not persist across actions.

After the selected generator cut completes, three bounded supplemental rounds
can discover additional names from the complete bytes of source and generated
headers actually entered by the scanner, including an opaque stop or a header
cache hit. Literal `defined(...)` expressions are included. This does not walk
the source tree again. Names already covered by measured initial facts are not
queried again. Each round executes a separate frozen host/target probe plan;
it does not relax missing-result errors in the original Kbuild workload.
Replay checks exact query membership, toolsets, ordered compiler context, and
the current measured values of symbolic argument dependencies before applying
answers to a fresh initial namespace. Source `#define`/`#undef` operations still
take effect in their original order. These answers authorize no source reads
or generated-header substitutions by themselves: the original lowering,
executed cut, and current source closure are still verified independently.
Unavailable generated inputs outside the selected cut and exhausted query
budgets retain the conservative fallback; the rounds do not expand that cut.
If a completed round has no queries and its validated probe plan is empty,
later discovery rounds carry that empty work forward without repeating
per-variant lowering and scanning. This relies on the rule-owned chain's
identical immutable inputs, not a source-convergence claim in the manifest.
Existing input loaders and cut validation still run; final replay performs
all per-variant config, lowering, current-source, and frozen-query checks.
Truncated rounds do not take this shortcut.

When complete macro-call analysis stops at an actually requested reserved name
whose initial compiler binding is still unknown, it can also register an
optional definedness attempt. This observes the first unknown expansion; it
does not continue past that stop, inspect unused macro bodies, or grant facts
from a source spelling. The configured compiler must successfully answer the
complete vector before any of its names become known. A normal nonzero compiler
exit makes only that exact attempt unqueryable and supplies no answers, even
if it wrote partial output. Other vectors and singleton queries remain
independent. Missing, malformed, signaled, skipped, or stale results remain
errors; prior accepted and rejected attempts are replay-validated alike.

The same rounds can measure C/C++ `__has_attribute(identifier)`,
`__has_builtin(identifier)`, `__has_feature(identifier)`, and
`__has_extension(identifier)` calls from entered files. Value-sensitive queries
preserve exact `-D` bodies and return the compiler's integer tokens, not merely
their truth values. Each round batches reached `(operator, operand)` pairs for
one exact compiler context into a single action. Its sorted result vector is
validated in full before
any answer is admitted; overlapping batches must agree and retain their
contributing request identities. The round's v5 manifest stores each exact
compiler context once and dictionary-encodes repeated argument and environment
strings. Query kinds and sibling configs reference the shared context. Decoding
preserves argument order, exact bytes, and absent versus empty collections;
context and query identities do not change. Only storage is shared: every
variant still replays each query against its own dependencies and toolsets.
Budgets separately bound 8,192 unique complete query descriptors, 32,768
per-variant memberships, values, compact discovery data (32 MiB), serialized
data (64 MiB), and expanded replay descriptors (128 MiB). The latter two
membership/work allowances are fixed, not multiplied by family size; these
are not total process-memory limits. Overflow in the existing directive and
intrinsic frontier discards that whole new frontier. Optional expansion demands
are staged separately: every variant's existing frontier finishes before
deterministic whole-query admission spends the remaining capacity. An optional
staging or admission limit omits that optional work without truncating the
existing frontier or relaxing prior-result validation. Retained optional plans
have separate structural and encoded-size bounds, not a total heap bound.
Ordinary logs distinguish whole-frontier truncation from optional omission;
published query counters include admitted optional queries, while omission
events are not a count of omitted names or distinct queries.
The optional `compiler_guards` output group exposes the canonical round
manifests for query-count and truncation diagnostics, without downloading
compiler result trees. It does not substitute for building kernel outputs.
For a first-round limit investigation, `compiler_guard0_manifest` requests only
that round's manifest; it does not demand later rounds or final kernel outputs.
The source interpreter admits an answer only when measured initial definedness
and the absence of a textual predefinition establish the original intrinsic
binding. Each operator's availability is established independently; textual
fallbacks remain ordinary macros. For each operator, any subsequent definition,
undefinition, or unknown header effect revokes that binding. Operands must be
proven currently undefined single identifiers, and the query freezes that
operand against initial macro re-expansion. Missing
answers grant no precision. Assembly, file-sensitive operators such as
`__has_include`, and other unmodeled calls remain conservative. The optional
completed-analysis cache is bypassed when intrinsic answers are attached;
ordinary final artifact sharing still uses the verified dependency result.

Measured answers participate in macro-state and cache identities. The
interpreter processes forced headers
in preprocessing order (`-imacros` before `-include`), then carries the resulting
macro state through the translation unit and its ordinary include closure.
Boolean combinations of `defined(...)` tests are evaluated from that state.
The fast scanner inspects replacement bodies for literal `CONFIG_*` aliases.
When a reachable token paste prevents precision, a bounded second pass can
prove the complete ordered macro-call stream. It retains exact definition
text and origin, restores final compiler `-D`/`-U` values after the shared
definedness probe, and expands every active ordinary-text span and conditional
expression. Conditional arithmetic uses a checked, portable signed-integer
subset; unsigned conversions, overflow, language-dependent keywords, and
unproved identifiers remain unsupported. This pass
bypasses definedness-only header caches and requires a single translation
unit with `-nostdinc`; unknown compiler bindings, unresolved conditions,
unsupported expansion forms, or incomplete include coverage reject the entire
proof. Self-references use token-local suppression across argument prescan and
replacement rescans; pasting a suppressed token and calls spanning source or
replacement boundaries remain unsupported. No successful prefix grants reuse.
An independent, lower-priority observer inventories leading-underscore names
in already-opened source and authenticated generated headers. Its cached,
bounded inventory includes ordinary tokens and supported literal replacement
bodies, including later and inactive spans, but excludes bound macro formals,
strings, comments, and include operands in the entered-file inventory.
Each compilation unit selects complete file inventories within the existing
limits, ordered by original token work and then exact file identity. Oversized
or incomplete files supply no hints; aggregate overflow stops selection without
discarding earlier complete files. Cached inventories retain their original
work charge. Baseline queries and actual demands retain their separate admission
rules. Family-stage omission counters do not count these local omissions.
After entered files, remaining optional budget may inspect literal-include
candidates, including later or inactive includes, with the original lookup
order and input ownership. Candidate lookup has separate mutable state and
does not add entered files, CONFIG dependencies, source paths, or read receipts.
These candidates can also supply reserved names in conditional operands, but
never intrinsic answers or computed include paths. Traversal, cycles, reads,
directive visits and lookups are bounded; cached reads retain their work charge.
Missing, oversized, unsupported or uninspectable candidates supply no facts and
cannot discard already-emitted hints. Generated origins still require the
existing authenticated observed-cut bytes before any query is admitted.
These are hints for future compiler measurements,
not expansion reads or assumed negative answers. Their optional query vectors
are separate from actual first-unknown demands and exclude higher-priority
names. All configs' existing queries and expansion demands are admitted before
entered-file token hints, followed by speculative literal-include hints, use
the remaining fixed frontier capacity. These last two tiers have separate query
vectors and staging ledgers; their retained plans share the original size and
node limits. A later config's stronger work may evict speculative hints, never
the reverse. Each tier retains one shared immutable probe graph across configs,
so identical dependency closures consume its staging budget only once. Each
config keeps its original allowed terminals; selecting its admitted work cannot
pull in another config's private probes. A rejected hint vector
supplies no facts and cannot poison a separate demanded-name attempt. Fresh
complete source replay is still required after successful measurements.
Within each variant and optional tier, equivalent definedness contexts schedule
each pending name once, retaining original raw descriptors and finite residual
vectors. Priority also applies across these equivalent contexts. A previously
rejected vector disables this scheduling optimization for its context class;
it neither rejects a separate singleton nor grants any per-name facts. Bounded
scheduling indexes reset per variant and never initialize the source namespace.
Measured definedness answers use the same object-like `-D` value projection
as their compiler queries, allowing equivalent queries to share answers across
compilation units. Original macro replacements are restored for each unit's
source analysis; value-sensitive intrinsic queries keep their exact context.
Macro-debug options which can record unused definitions retain the full config
capsule, including when they occur in configured compiler arguments.
Dollar punctuation requires an
executed compiler probe with the projected invocation's language, ordered
options, and environment. Neither an option spelling nor a compiler-family
name grants this capability; a negative or unavailable result retains the
conservative fallback. This initial
bounded coverage does not yet prove general Linux C/assembly translation units.
Source-bearing compiler-driver links also retain the full-config fallback when
their configured prefix, suffix, or environment differs from the compile-only
contract used by initial-state probes.
Unproved dynamically constructed config names, dynamic includes, preprocessor
behavior outside the supported model,
unsafe compiler controls, source scripts, `CONFIG_MODVERSIONS`, and other
inputs that cannot be proved safe fail closed to the full resolved config and
ordinary per-config execution.

Bounded analysis-local caches memoize supported forced-header prefixes and
eligible source-header includes directly reached from a translation unit,
including their nested closures. Hits validate the applicable compiler context,
consumed macro state, and live include-resolution and binding witnesses before
replaying recorded macro effects. Cache misses and admission-limit failures use
normal interpretation; they do not themselves make an action opaque. This is
planner memoization, separate from graph deduplication and Bazel artifact
caching. It neither relaxes precision requirements nor supplies runtime
cache-hit statistics to the reuse report.

Generated text needed by that include analysis is handled without a
generator-name table. A statically bounded Kbuild recipe may be measured with
the selected script runtime, configured tool proxies, declared immutable source
inputs, and the exact source-exported environment. The planner substitutes its
bytes only when target and host scopes agree and the probe proves the recipe has
one ordinary output, no other filesystem effect, no transient execution path,
and bounded runtime and size. Any unsupported or inconclusive recipe remains a
normal mapped Kbuild action and keeps conservative per-configuration inputs.

Config projection for an AWK file-check generator additionally requires a
bounded source-language proof: literal affirmative config selectors, an exact
file-counter initialization and increment, and immutable nonempty inputs before
the final config operand. Unsupported syntax or unproven input bindings retain
the full config. Eligible generators still compare their projected output with
the full-config output at execution time; projection never bypasses that check.

Unknown recipe environment variables remain byte-exact in the probe identity,
while Linux Make/script variables whose effects are already present in compiler
argv are omitted. Locale and `SOURCE_DATE_EPOCH` may change diagnostics or
ordinary macro replacement text, but cannot change the defined-name set or
introduce a literal `CONFIG_*` alias in the supported model, so they are also
omitted. The selected tool action's configured environment is installed by the
same identity-bound proxy for the real compile and the probe. Known
argument-injection, config-file, dynamic-loader, and interpreter controls fail
closed; wrappers with hidden flag semantics must expose those flags in their
configured action argv. A host compiler action whose retained source-exported
environment depends on target-only probe state likewise falls back to opaque,
per-configuration execution instead of crossing execution scopes.

Each precise compiler action receives an action-private kernel tree containing
only its exact source closure. Its root Kconfig is excluded from the spawn
inputs through the exact-source anchor, while opaque actions keep the complete
source and resolved-config closures. Opaque kernel readers receive the complete
source closure through one shared, source-repository runfiles aggregate. Its
root mappings are checked against the caller's exact original Bazel Files,
including the root marker; an exec-configured wrapper resolving a generated
input to a different File is rejected. No source bytes are copied, and precise
actions do not acquire the aggregate. This lets Bazel retain aggregate upload
state instead of treating the complete source tree as a flat input list for
every opaque action. It does not narrow dependencies, prove additional compiler
reuse, or change the runner's restrictions on copied working trees.
Opaque readers now see the aggregate's physical source path. Source-owned
filename/debug prefix-map flags are expanded against that same root; without
such flags, compiler-emitted filenames and debug paths can reflect the new
location. This transport change does not promise byte-identical objects to the
previous physical layout.

The target shard also emits exact
variant-view markers. One batched projection action per non-empty
`(variant, tree)` consumes those markers and the corresponding
content-addressed TreeFiles, then copies the public facade outputs while
preserving executable mode. Public base and variant labels remain unchanged.

Each configured kernel exposes a machine-readable reuse report through the
`reuse_report` output group. It reports planned in-invocation graph
deduplication: variant instances, unique and shared nodes, reused instances,
pairwise compile/archive-node and recognized compiler-node reuse rates, and the
subset of compiler actions whose source/object/config closure was analyzed
precisely. The precise-compiler coverage rate measures analyzer coverage;
effective precise reuse measures precisely analyzed shared nodes against the
complete recognized compiler-node population. The report also includes node
membership groups and grouped opaque fallback reasons. Schema v2 records the
canonical host and target toolset identities plus one typed, precision-marked
membership record per semantic node, so consumers can reconstruct every
aggregate instead of trusting summary counters alone.

Observed replay also reports an `observed_header_frontier`: exact generated
producer/slot bindings that still stop analysis, including whether their bytes
were already observed or their producers already executed. These diagnostics
describe the per-variant replay plans before pruning, not the final live-node
totals. Explicit truncation flags distinguish incomplete collection from an
empty frontier; none of these diagnostic fields authorize reuse.

These are static planner counts and ratios for the emitted image-family graph.
They are not local, disk, or remote action-cache hit rates, and they do not
measure actions executed, wall time, probe generation, planning, facade
projection, or other Bazel actions outside that graph. Runtime cache behavior
must be measured separately from Bazel execution metrics. The fallback reasons
make a lost planned-reuse opportunity attributable to a specific unsupported
include or action shape instead of reporting only a zero rate:

```sh
bazel build @example_kernel//:kernel --output_groups=+reuse_report
```

For dependency diagnostics, `--output_groups=plan_snapshots` builds only the
resolved per-variant planner snapshots, not kernel objects or images. These
compressed internal snapshots retain exact producer/slot edges, persistent input
sets, and config-dependency classifications before family reduction. The `plan`
output group instead exposes the reduced execution-plan shards and therefore
requires the selected generator cut to execute before observed replay. Neither
group is a substitute for building the real targets when measuring performance.

Ordinary compiler-probe discovery registers requests without retaining discarded
dependency annotations. An exact, already registered but unavailable compiler
state lets discovery skip the source scan that would stop at that same request;
unmatched compiler invocations still use the full admission checks. Supplemental
guard-only rounds restore the original lowered graph from an initial-action
checkpoint and scan the authenticated generated headers, but do not finalize a
graph or publish reuse evidence. Initial planning publishes a bounded compressed
checkpoint per variant in one declared directory: the provisional graph and its
compiler request/expression namespace, not cached compiler answers or source-read
proofs. Compiler requests are stored once and referenced by their original IDs.
Every guard round and final replay consumes that directory alongside the original
source, config, tool, probe, and execution-cut inputs. Replay independently resolves
the current Kconfig, rebinds current source roots and compiler scopes, and validates
the saved contracts before analysis. It skips repeated Make evaluation and lowering;
an invalid checkpoint fails instead of silently selecting another planning path.
Final family replay still performs the full analysis, content addressing, and
evidence sealing before any cross-config substitution.

For planner CPU diagnostics, `--output_groups=compiler_guard1_cpu_profile`
samples the second supplemental guard round for up to 180 seconds using its
original inputs, arguments, environment and execution platform. It requires the
initial generator cut and first guard round, but not later rounds or final
replay. Only the CPU profile is published; disposable planner outputs are never
accepted as build results. This opt-in action bypasses its own cache without
changing normal planner actions or default outputs. Inspect the resulting
`.compiler-guards-1.cpu.pprof` with `go tool pprof -top` before using it: capture
checks the complete gzip stream, while pprof validates the profile itself.
A bounded CPU sample is not a completed-kernel timing or a cache-hit measurement.

To isolate initial Kbuild discovery instead, use
`--output_groups=kbuild_cpu_profile`. It runs the selected variant's original
Kbuild discovery arguments after compiler and Kconfig capability setup, without
demanding the generator cut or any supplemental guard round. The same bounded
180-second capture publishes only `.kbuild-discovery.cpu.pprof`, never a planner
result. A planner that finishes earlier publishes its completed CPU profile.

For allocation-stack attribution at the same bounded round-1 workload, use
`--output_groups=compiler_guard1_heap_profile`. This separate diagnostic action
uses a heap-enabled planner binary with the same planning sources and original
inputs, arguments, environment and platform. It publishes only
`.compiler-guards-1.heap.pprof`; the CPU profile that drives its 180-second
lifecycle and disposable planner outputs remain private. Both profiles must
finish within the supervisor's existing 15-second flush margin.

Heap capture does not force a GC or change the sampling rate. The profile
reflects Go's sampled, most recently published GC accounting, which can lag
current allocations; it is not an exact `HeapAlloc` measurement or a retaining
reference graph. Existing CPU phase labels do not label heap samples.
Validate and inspect it with `go tool pprof -sample_index=inuse_space -top`
and `go tool pprof -sample_index=alloc_space -top` before drawing conclusions.
Allocation totals cover the process lifetime, not just the CPU sample window.
The ordinary planner has no heap-writer binding; only the diagnostic binary
retains heap profiling code. Adding the inert shared hook changes ordinary tool
digests once, but does not add heap sampling, flags or outputs to normal actions.

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
bazel test //boot:init_test //internal/modversions:modversions_test \
  //cmd/qemuboot:qemuboot_test
bazel shutdown
bazel test //:linux_6_12_96_x86_64_boot_test \
  //:linux_6_12_96_x86_64_module_test \
  //:modversions_6_12_96_x86_64_build_test
bazel shutdown
bazel test //:linux_6_12_96_aarch64_boot_test \
  //:linux_6_12_96_aarch64_module_test
bazel shutdown
bazel test //:kernel_outputs_x86_64_build_test \
  //:linux_6_18_39_x86_64_boot_test \
  //:linux_6_18_39_x86_64_module_test \
  //:modversions_6_18_39_x86_64_build_test
bazel shutdown
bazel test //:kernel_outputs_aarch64_build_test \
  //:linux_6_18_39_aarch64_boot_test \
  //:linux_6_18_39_aarch64_module_test \
  //:modversions_6_18_39_aarch64_build_test
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

The MODVERSIONS suites retain their build checks and also compare the built
modules' version records against their producers' `Module.symvers`. Each boots
the matching MODVERSIONS kernel, requires it to reject a copy of the external
module with an altered `module_layout` CRC, and then loads the original module.
CRC values come from the selected compiler's actual build, not fixed fixtures.

## Development

The root workspace contains planner, transition, rule, and tool tests:

```sh
bazel test //...
```

The standalone examples, QEMU compatibility workspace, and Aya consumer
workspace are excluded from root package discovery with `.bazelignore`; run
them from their own directories. Release preparation is documented in
[`RELEASING.md`](RELEASING.md).
