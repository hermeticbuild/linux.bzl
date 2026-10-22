# linux.bzl measured kernel prototype

Linux's source-selected `conf` owns configuration. Bazel builds it from the
selected source, then runs it with ordered raw fragments, the selected compiler,
and declared process tools. Its native `.config`, `auto.conf`, autoconf header,
and complete symbol/type inventory feed the measured Kbuild graph. There is no
second Kconfig solver or declared compiler-capability table.

One generic Make/Python exporter, built with Bazel and pinned by host artifact
integrity, exports source-only Kbuild syntax at repository time. Execution-time
callbacks measure native compiler and host-library queries and register separate
compiler, generator, archive, and linker actions. Dependency scans select the
source/header inputs for each compiler action. Product actions do not run Make.

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

Use Bazel 9.3.0 and the registered Linux execution toolchain:

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
`check_reuse.py` retains the baseline/unchanged/source-edit experiment using actual
execution logs and producer dependencies. Expensive build/configuration targets
are explicitly tagged `manual`.

The native-config checkpoint is qualified. The migrated product path still needs
runtime qualification. Complete boot/module support across all six source lines,
both architectures, cross-configuration compiler sharing, and production mutation
coverage remain acceptance requirements; config-only results do not establish them.

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
