# linux.bzl measured kernel prototype

Linux's source-selected `conf` owns configuration. Bazel builds it from the
selected source, then runs it with ordered raw fragments, the selected compiler,
and declared process tools. Its native `.config`, `auto.conf`, autoconf header,
and complete symbol/type inventory feed the measured Kbuild graph. There is no
second Kconfig solver or declared compiler-capability table.

One generic Make/Python exporter, built with Bazel and pinned by host artifact
integrity, exports source-only Kbuild syntax at repository time. Execution-time
callbacks measure native compiler and host-library queries and register separate
compiler, generator, archive, and linker actions. Each dependency scan is a
separate compiler action with declared candidate source and generated Files.
Its measured reads select the inputs for both scan reuse and compilation.
Target product actions do not run Make. The BTF resolver uses a host libbpf archive and installed
headers built together by its selected kernel source's native Make rules.

## Consumer interface

Load `kernel_source` from `//rules:source.bzl`, `kernel_config` from
`//rules:config.bzl`, `kernel_build` from `//rules:kernel.bzl`, and
`kernel_artifact` from `//rules:artifact.bzl`. A source is a declared file group
plus its root Makefile anchor. Configurations select that source, ordered
fragments, and a declared `pahole`; architecture and compiler follow the target
platform. A build consumes the configuration provider and requests native paths
through `roots`. Artifact selectors expose individual native products.

Use `kernel_source_archive` from `//:repository.bzl` for the pinned raw archive.
It accepts `urls`, `integrity`, `strip_prefix`, native `patches`/`patch_strip`,
and a caller `build_file`. That BUILD file should declare its source group with
`kernel_source_files(name = "sources")` from `//rules:source_files.bzl`.
The repository adds Bazel package metadata beside native `Build` files, whose
names otherwise hide whole directories on case-insensitive filesystems. Native
files remain unchanged; package discovery requires host `find` on Linux/macOS.

A build also takes `syntax = "@source_syntax//:program"`. Create that repository
with `kernel_syntax_repository(source = "@raw_source//:Makefile", tool =
"@exporter//:kbuild-exporter")` from `//:repository.bzl`; use
`kernel_syntax_tool(manifest = "//:exporter.json")` to download the generic host
tool. Manifest format 1 maps `platforms` keys `linux_aarch64`, `linux_x86_64`,
`macos_aarch64`, and `macos_x86_64` to pinned `url` and `integrity` entries. The
build rejects a syntax anchor that differs from its native configuration source.

Only kernel builds depend on that syntax target. Raw sources and configurations
remain independent, so configuration-only requests do not fetch or run the
exporter. No Kconfig parser, fragments, compiler probes or configured values are
part of the exporter. Its static module binds the callback once, avoiding an
execution-time syntax transport. The source tree and tool are watched for changes.

The execution-tools contract currently selects Linux x86-64. A macOS caller may
use a Linux execution platform; this does not make Linux `conf` executable on
Darwin. The generic Make exporter also builds on native macOS.

The root must apply the Bison, rules_cc_autoconf, and zlib patches used in
[MODULE.bazel](MODULE.bazel). Bzlmod ignores dependency-owned
`single_version_override` declarations; those patch files must be copied into
the consuming root. The Make input patch is needed only when building the exporter
or running its producer-owned native Make validation fixture. Native source
patches in `native/` have a separate owner: they apply to the pinned kernel archive itself.

The zlib patch keeps its unconfigured public header compatible with native
`-Wundef` diagnostics without supplying header availability. Native zlib configure
still regenerates that header from its unchanged template and runs its own checks.

## Build and test

Use Bazelisk with the version in [`.bazelversion`](.bazelversion) and the registered
Linux execution toolchain. The temporary
[Bazel fork](https://github.com/tamird/bazel/releases/tag/10.0.0-bb0242a676)
provides input discovery and native literal writes for execution-time action
templates and includes their expanded child actions in action graph exports.
These interfaces support per-object preprocessing reuse, metadata generation,
and their qualification. The release includes source,
binary hashes, and native Linux and macOS test provenance for x86-64 and ARM64.
The fork also repairs modified inputs during in-memory sandbox reuse. The
qualification workflow enables in-memory stashes with synchronous stash updates
and deletion. Input discovery guards already evaluated inputs, including
cache-restored inputs, together with scheduling dependencies. Newly discovered
inputs are evaluated before execution guards them, so pruned runfiles do not
require metadata that was never loaded.
Stock Bazel 9.3.0 does not provide these interfaces.

The [LLVM toolchain index](llvm_toolchain_index.json) selects
[corrected LLVM 23.1.2 packages](https://github.com/tamird/hermetic-llvm/releases/tag/llvm-23.1.2-ir1)
for Linux and macOS. The correction preserves diagnostics and inlining-chain
notes when compiling LLVM IR without an AST source manager. Compiler tools,
C/C++ toolchains, and builtin headers still come from the same ordinary archive.
A consuming root must select this exported index with
`llvm_toolchain_minimal.index(file = "@linux//:llvm_toolchain_index.json")`,
using its repository name for this dependency; root index tags take precedence.

```sh
bazel test //native:config_tests //:action_built_config_test \
  --platforms=@llvm//platforms:linux_x86_64 \
  --extra_execution_platforms=@llvm//platforms:linux_x86_64 \
  --extra_toolchains=//:driver_toolchain --test_output=errors
# Bootstrap one generic host tool; select the matching host CPU and OS.
# Linux uses linux_{aarch64,x86_64}_musl; macOS uses macos_{aarch64,x86_64}.
bazel test //native:exporter_archive //:repository_test //:generation_test \
  --platforms=@llvm//platforms:linux_x86_64_musl \
  --extra_execution_platforms=@llvm//platforms:linux_x86_64
exporter="$PWD/$(bazel cquery //native:exporter_archive \
  --platforms=@llvm//platforms:linux_x86_64_musl \
  --extra_execution_platforms=@llvm//platforms:linux_x86_64 \
  --output=starlark --starlark:expr='"\n".join([file.path for file in target.files.to_list() if file.is_directory])')"
bazel test //:kernel_acceptance_test \
  --override_repository=kbuild_exporter="$exporter" \
  --platforms=@llvm//platforms:linux_x86_64 \
  --extra_execution_platforms=@llvm//platforms:linux_x86_64 --test_output=errors
```

The focused native-config tests cover modern and 5.10 source semantics, raw LZ4
overrides, old allnoconfig defaults and unwritten symbols, hidden/visible choices,
typed measured defaults, native Make string representation, and a compiler built
in the same invocation. That compiler fixture is a compiled forwarding driver,
not a bootstrap of LLVM itself.

`//:kernel_acceptance_test` checks the existing minimal vmlinux, its ELF/entry
symbols, and six ordered flag groups against native GNU Make. It uses the
already-produced native configuration, without configuring the kernel again.
`check_reuse.py --family` runs base, unchanged, LZ4, debug, BTF, and the bounded
`PANIC_TIMEOUT=37` control, then runs both object/diagnostic and reuse comparisons.
The default mode runs the six source-mutation phases. Combine `--family` with
`--source /path/to/linux/init/main.c` to capture base and unchanged once for both comparisons,
before the remaining family configurations and source edits. This retains the
first invocation's compiler observations, which a repeated local cache hit may
omit from its execution log. Both retain actual execution logs and producer
dependencies. The qualification reporters authenticate native
compiler inventory, split C object ancestry, integrated reference parity, and
cross-configuration backend action identity. Mutation selectivity includes native
host compilers; the family parity denominator remains target C objects.
The [static family baseline](qualification/family_baseline.json) records the
resolved configurations, product compiler inventory, and measured action-key
sharing from a complete family qualification. It bounds aggregate product
compiler work and every backend family membership. It requires at least 187
LZ4, 184 debug and seven BTF backend retrievals matching earlier captures,
as measured from disk-cache hits in the warm sequence. Each target counts once
per capture, without attributing which matching predecessor populated the cache.
These 378 required retrievals bound fresh executions to at most 7,786 of the
8,164 product compiler roles. Additional sharing is allowed. Update this baseline
deliberately when changing the source, configurations, or compiler inputs.
Product compiler counts exclude integrated references and diagnostic controls.
Absent execution records are reported separately from cache hits. These bounds
cover compiler roles; they do not bound archive, link, or other native recipe
work, nor build duration.
The reporters also authenticate every preprocessing scan, including host
helpers and integrated references, against the expanded action graph and
compact execution log. An unchanged build may retrieve cached results but must
not execute a `KernelArtifact`, `KernelLLVMBackend`, or `KernelDependencyScan`
action. Source and header mutations must execute the affected scans without
repeating unrelated preprocessing. These scan counts are retained alongside
the native compiler-role denominator and backend reuse checks.

Earlier batched-scan reports omitted preprocessing from their compiler-work
accounting. Backend cache-hit evidence from those reports remains specific to
backend actions; it does not establish total compiler-work isolation. The
current qualification captures expanded child actions inside the measured build
with `--experimental_aquery_dump_after_build_format=jsonproto`, before a later
command can invalidate retained analysis state. This capture disables Skymeld.
It includes original discovery inputs and omits command text: producer ownership
and compiler ancestry use the declared action edges, while scan work and backend
cache identity come from the compact execution log. It does not establish each
scanner's used-input set.

Reference objects are enabled by
`--//:validate_native_objects`, which also records the source-selected LZ4 and
Make recipe-ordering checks and the two deferred compiler diagnostic controls.
The manual `native_compiler_checks` and `native_compiler_checks_5_10` targets
request only `lib/ctype.o` for focused execution of these controls. Production
builds omit the validation actions. Example variants
transition one build label so corresponding backend paths share an action owner.
Expensive build/configuration targets are explicitly tagged `manual`.

Kernel acceptance requires boot and module coverage across all six source lines
and both architectures, native compiler parity, cross-configuration compiler
sharing, and production source-mutation checks. Configuration-only results do not
establish these properties.

## Owners

- `native/`: source-built conf, process adapters, and generic GNU Make bootstrap.
- `renderer/`: symbolic native Make transport and source-owned Kbuild export.
- `rules/`: configuration providers, measured Kbuild graph, artifact scheduler.
- `probe_runtime.py`, `scheduler.py`, `execute.py`: native queries and declared actions.
- `examples/`: raw fragments and platform-selected consumers.

Additional measured configuration defaults use
[autoconf.bzl](https://github.com/hermeticbuild/autoconf.bzl). The selected
toolchain must match the native configuration's compiler. Real optional Kconfig
declarations consume those typed defaults; hidden defaults are not rewritten as
user requests.
