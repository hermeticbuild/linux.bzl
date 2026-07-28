# Releasing the Kconfig graph generator

The `kconfig` and `kconfig_parse` binaries share an independent release stream
because repository rules download them before the normal Bazel analysis phase.
Generator releases are tagged as `kconfig-vX.Y.Z`.

## Compatibility

`-compact_schema` defaults to `v0.0.11`. Existing in-tree consumers and
goldens must remain byte-compatible until the rules repository explicitly
requests another schema.

Schema `v0.0.12` is opt-in. It emits classified source-tree inputs, exact
source-like include closures, module object roots, and fail-closed source
actions. Rust-owned objects are excluded from the ordinary object graph.
`-rust_profile_out` additionally emits the source-derived
`linux-rust-profile-v1` JSON profile and is valid with compact schema `v0.0.12`
or `v0.0.13`.

Generator `v0.0.13` keeps schema `v0.0.12` and adds config-sensitive
KASAN/KCSAN/UBSAN instrumentation flags, including Kbuild per-object and
per-directory overrides.

Generator `v0.0.14` introduces opt-in schema `v0.0.13`. It emits one
content-addressed graph for a base config and all overlays, exact source-input
digests, indexed config payloads and generated-header families, and canonical
base/delta image rules. The rules repository must remain on schema `v0.0.12`
until all six `v0.0.14` host archives have been published and pinned.

## Prepare

1. Run `go test ./internal/kconfig ./internal/cmd/kconfig
   ./internal/cmd/kconfig_parse ./internal/cmd/compact_metadata_check`.
2. Run the Bazel generator, compact-golden, and prebuilt archive tests.
3. Exercise v0.0.12 against maintained Linux 6.12 and 6.18 source trees with
   `CONFIG_RUST=y`. Verify that the legacy and pin-init Rust layouts produce
   valid profiles and that a modified but compatible source tree is accepted.
4. Exercise v0.0.13 with the base, debug/BTF, and compression overlays in one
   invocation. Verify that identical object actions and generated-header
   family identities have one content target, changed actions have distinct targets,
   and unresolved potentially active includes fail closed.
5. Build the v0.0.12 and v0.0.13 graphs from identical configs and compare
   kernel image, `vmlinux`, `System.map`, config, and module outputs bit for
   bit before activating the new schema. After building the candidate
   archives, run `.github/workflows/kconfig_schema_diff.sh
   dist/kconfig-linux-amd64.tar.zst`. Tag and manual prebuilt workflows run
   this gate before publishing.
6. Exercise C sanitizer configs against maintained Linux 6.12 and 6.18 source
   trees. Verify KASAN/KCSAN/UBSAN object opt-outs, test-object opt-ins, arm64
   nVHE defaults, and integer-wrap inputs.
7. Verify that unsupported source layouts, unresolved Make expressions,
   incomplete leaf objects, and non-x86_64 Rust profiles fail with actionable
   errors.
8. Build the six platform archives twice from clean output trees and compare
   their SHA-256 digests.

Each deterministic `.tar.zst` archive must contain exactly two executables at
its root: `kconfig` and `kconfig_parse` on Linux and macOS, or `kconfig.exe` and
`kconfig_parse.exe` on Windows. Both tools remain in the same archive and
version stream.

## Publish

After the release-preparation change is merged, tag that merge commit:

```sh
VERSION=v0.0.14 ./release_kconfig.sh
```

The tag workflow publishes archives for Linux, macOS, and Windows on amd64 and
arm64, plus `SHA256SUMS` and `kconfig_tool_releases.metadata`.

Once all six assets are available, update the rules repository's
`KCONFIG_TOOL_VERSION` and integrity table in a separate change. That consumer
change may switch the repository graph to `-compact_schema=v0.0.13` only after
the differential gates pass; do not change the generator's default.
