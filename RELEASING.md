# Releasing linux.bzl

Each rules release pins both its supported Linux source catalog and the
platform-specific Kconfig graph generator archives. The generator has its own
version and release assets; changing it is an explicit compatibility update to
the rules module.

## Prepare

1. Choose a pre-1 semantic version and update the module version and release
   notes.
2. Run `bazel test //...`, the standalone examples, and the Linux 6.12 and 6.18
   compatibility builds for x86_64 and aarch64.
3. Build a named overlay and verify its kernel release and fixed output
   contract.
4. Test a consumer that renames the `linux.bzl` module repository and loads
   `linux_image` only from the root `linux.bzl` entry point.
5. Audit the pinned source URLs and integrities in the catalog.
6. Audit `KCONFIG_TOOL_VERSION` and all six entries in
   `internal/kconfig_tool_releases.bzl`.

When updating the generator, first publish deterministic
`kconfig-<version>` archives for Linux, macOS, and Windows on amd64 and arm64.
Each archive must contain `kconfig_parse` at its root. Compute the Subresource
Integrity values from the published assets, update the checked-in table, and
run repository-generation smoke tests for every host archive. Rebuild the
archives once from a clean checkout and compare digests.

After verifying the generator commit, create and push its release tag with:

```sh
VERSION=vX.Y.Z ./release_kconfig.sh
```

## Publish

1. Tag the verified release commit as `vX.Y.Z`.
2. Publish the module source archive and verify it against the release
   integrity.
3. Test a clean consumer module using the tag, without `local_path_override` or
   a locally built generator.
4. Submit the release to the Bazel Central Registry using the files in `.bcr/`.

## Support checks

A release is ready only when:

- Bazel 8.x and 9.x pass the root test suite.
- Linux 6.12 and 6.18 build for x86_64 and aarch64 with the registered Bazel C++
  toolchains.
- Repository generation is smoke-tested with all six host generator archives.
- The base and named-overlay repositories produce the documented fixed outputs.
- Unsupported Kbuild constructs and toolchain/config mismatches fail with
  actionable diagnostics.

Version 0.1 intentionally gives named variants independent object graphs.
Cross-variant action sharing is not a release requirement until the config
input model can prove it correct.

Signing kernel images or modules is outside the current release contract.

For version `X.Y.Z` and tag `vX.Y.Z`, create the BCR-compatible source asset
from the verified release commit:

```sh
git archive \
  --format=tar.gz \
  --prefix=linux.bzl-X.Y.Z/ \
  --output=linux.bzl-vX.Y.Z.tar.gz \
  vX.Y.Z
openssl dgst -sha256 -binary linux.bzl-vX.Y.Z.tar.gz | openssl base64 -A
```

Attach `linux.bzl-vX.Y.Z.tar.gz` to the matching GitHub release. Its name and
top-level prefix match `.bcr/source.template.json`.
