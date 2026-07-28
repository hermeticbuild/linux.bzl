# Releasing the Kconfig graph generator

The `kconfig` and `kconfig_parse` binaries share an independent release stream
because repository rules download them before the normal Bazel analysis phase.
Generator releases are tagged as `kconfig-vX.Y.Z`.

## Compatibility

`-compact_schema` defaults to `v0.0.11`. Existing in-tree consumers and
goldens must remain byte-compatible until the rules repository explicitly
requests another schema.

Schema `v0.0.12` is opt-in. It emits classified source-tree inputs, recursively
resolved literal source include closures, module object roots, and fail-closed
source actions. Rust-owned objects are excluded from the ordinary object graph.
`-rust_profile_out` additionally emits a source-derived Rust profile and is
valid only with `-compact_schema=v0.0.12`.

Generator `v0.0.13` keeps schema `v0.0.12` and adds config-sensitive
KASAN/KCSAN/UBSAN instrumentation flags, including Kbuild per-object and
per-directory overrides.

Generator `v0.0.14` keeps compact schema `v0.0.12`, upgrades the separate Rust
profile to `linux-rust-profile-v2`, and supports both x86_64 and arm64. The
profile records source-derived rustc version gates and the architecture's
built-in or generated Rust target specification. `kconfig_parse` also accepts
the exact selected Rust toolchain probe so action-time config resolution can
verify that dynamic rustc capabilities do not change the repository-generated
structural snapshot. Compact object metadata preserves module roots and
`OBJECT_FILES_NON_STANDARD` overrides, and can carry the source-built x86
objtool label required for Kbuild-compatible post-processing.

Generator `v0.0.15` keeps compact schema `v0.0.12` and extends each generated
object with its recursively resolved private header closure. Global and
selected-architecture headers remain classified source-tree inputs instead of
being repeated on every object. A computed preprocessor include, unresolved
forced source include, or unmodeled source include-search flag marks the
closure incomplete so rules consumers retain their exhaustive full-tree
fallback. Assembler `.incbin` dependencies also use that fallback. Source-root
include directories, forced includes, comments, and preprocessor line splices
are included in the scan. Rules consumers pass exact
generated include spellings, with source backings for generated asm-generic
wrappers. Other unresolved literals cannot name source files once the source
search model is complete; generated and toolchain providers remain independent
action inputs. Consumers assert generated-manifest completeness explicitly;
other compact metadata producers retain the exhaustive fallback.

## Prepare

1. Run `go test ./internal/kconfig ./internal/rusttoolchain ./internal/cmd/kconfig ./internal/cmd/kconfig_parse`.
2. Run the Bazel generator, compact-golden, and prebuilt archive tests.
3. Exercise v0.0.12 against maintained Linux 6.12 and 6.18 source trees with
   `CONFIG_RUST=y` on x86_64 and arm64. Verify that the legacy and pin-init Rust
   layouts produce valid v2 profiles, that built-in and generated target
   specifications are selected correctly, and that a modified but compatible
   source tree is accepted.
4. Exercise C sanitizer configs against maintained Linux 6.12 and 6.18 source
   trees. Verify KASAN/KCSAN/UBSAN object opt-outs, test-object opt-ins, arm64
   nVHE defaults, and integer-wrap inputs.
5. Verify that unsupported source layouts, unresolved Make expressions,
   incomplete leaf objects, and unsupported Rust architectures fail with
   actionable errors.
6. Build the six platform archives twice from clean output trees and compare
   their SHA-256 digests.

Each deterministic `.tar.zst` archive must contain exactly two executables at
its root: `kconfig` and `kconfig_parse` on Linux and macOS, or `kconfig.exe` and
`kconfig_parse.exe` on Windows. Both tools remain in the same archive and
version stream.

## Publish

After the release-preparation change is merged, tag that merge commit:

```sh
VERSION=v0.0.15 ./release_kconfig.sh
```

The tag workflow publishes archives for Linux, macOS, and Windows on amd64 and
arm64, plus `SHA256SUMS` and `kconfig_tool_releases.metadata`.

Once all six assets are available, update the rules repository's
`KCONFIG_TOOL_VERSION` and integrity table in a separate change. That consumer
change must request `-compact_schema=v0.0.12`; do not change the generator's
default.
