# Releasing the Kconfig graph generator

The `kconfig` and `kconfig_parse` binaries share an independent release stream
because repository rules download them before the normal Bazel analysis phase.
Generator releases are tagged as `kconfig-vX.Y.Z`.

## Graph Format

The generator emits one content-addressed graph for a base config and all
overlays. The graph contains exact source-input digests, indexed config
payloads and generated-header families, and canonical base/delta image rules.
The rules repository pins the generator release that defines this format; the
CLI and repository rule do not negotiate schemas or retain older emitters.
The current `compact-v8-adaptive-content-graph` schema also records the exact
`__GENKSYMS__` source closure and compile flags for basic module versioning.

The same binary emits the source-derived `linux-rust-profile-v2` JSON profile
and config-sensitive KASAN/KCSAN/UBSAN instrumentation flags, including Kbuild
per-object and per-directory overrides. Rust-owned objects remain excluded
from the ordinary object graph.
The profile supports x86_64 and arm64, records source-derived rustc version
gates, and selects the architecture's built-in or generated Rust target
specification. Compact object metadata preserves module roots and
`OBJECT_FILES_NON_STANDARD` overrides and carries the source-built x86 objtool
label required for Kbuild-compatible post-processing.

## Prepare

1. Run `go test ./internal/kconfig ./internal/cmd/kconfig
   ./internal/cmd/kconfig_parse ./internal/cmd/compact_metadata_check
   ./internal/rusttoolchain`.
2. Run the Bazel generator, compact-golden, and prebuilt archive tests.
3. Exercise the released generator against maintained Linux 6.12 and 6.18
   source trees with `CONFIG_RUST=y`. Verify that the supported Rust layouts
   produce valid profiles and that a modified but compatible source tree is
   accepted.
4. Exercise the base, debug/BTF, and compression overlays in one invocation.
   Verify that identical object actions and generated-header family identities
   have one content target, changed actions have distinct targets, and
   unresolved potentially active includes fail closed.
5. Build all public kernel outputs from a clean repository graph and compare
   them with the previous release. For the maintained four-config x86 fixture,
   require 4,727 object memberships, 3,533 selected variants, and exactly
   3,533 `LinuxObjectCompile` actions in the Bazel BEP. Require the 23
   generated-header families and their producer actions to retain the expected
   base/overlay sharing partitions.
6. Exercise C sanitizer configs against maintained Linux 6.12 and 6.18 source
   trees. Verify KASAN/KCSAN/UBSAN object opt-outs, test-object opt-ins, arm64
   nVHE defaults, and integer-wrap inputs.
7. Exercise `CONFIG_MODVERSIONS=y` against maintained Linux 6.12 and 6.18
   source trees on x86_64 and arm64. Cover C and assembly exports, generated C
   sources, built-in and external C modules, and verify that extended, DWARF,
   and Rust module-versioning paths fail closed.
8. Exercise x86 and RISC-V purgatory with their per-source preprocessor
   contexts, ARM64 KVM generated `hyp_constants.h`, compiler-provided ARM NEON
   headers, and a provably inactive legacy `MODVERSIONS` include.
9. Exercise an empty `CONFIG_SYSTEM_TRUSTED_KEYS` configuration with
   `CONFIG_SYSTEM_TRUSTED_KEYRING=y`, including dm-verity root-hash signature
   verification. Verify that non-empty certificate, revocation, blacklist,
   and module-signing inputs still fail closed.
10. Verify that unsupported source layouts, unresolved Make expressions,
   incomplete leaf objects, and unsupported Rust profiles fail with actionable
   errors.
11. Build the six platform archives twice from clean output trees and compare
   their SHA-256 digests.

Each deterministic `.tar.zst` archive must contain exactly two executables at
its root: `kconfig` and `kconfig_parse` on Linux and macOS, or `kconfig.exe` and
`kconfig_parse.exe` on Windows. Both tools remain in the same archive and
version stream.

## Publish

After the release-preparation change is merged, tag that merge commit:

```sh
VERSION=vX.Y.Z ./release_kconfig.sh
```

The tag workflow publishes archives for Linux, macOS, and Windows on amd64 and
arm64, plus `SHA256SUMS` and `kconfig_tool_releases.metadata`.

Once all six assets are available, update the rules repository's
`KCONFIG_TOOL_VERSION` and integrity table in a separate change. The consumer
must use the release's only graph format, and CI must verify that actual Bazel
compile action counts match the graph's selected content targets.
