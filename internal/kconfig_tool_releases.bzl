"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

visibility("//...")

KCONFIG_TOOL_VERSION = "v0.0.16"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-bhGwY/vJ5V/tqVWL1WY3g0K3TPvXHJ0Sg1PqdLoETGM=",
        urls = ["{}/kconfig-darwin-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-Gr24z3A1Z1RphmiO6zYleXHcOPdWclRo44uqzoTd6dM=",
        urls = ["{}/kconfig-darwin-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-sdWfxiLJsbD0VDxy/pVa8+JsOHwgwo/ApEN4tcpoByM=",
        urls = ["{}/kconfig-linux-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-RzI5IcRZtNeRv/NtLyRe4RChZ4t0drOdxLk5dktj+us=",
        urls = ["{}/kconfig-linux-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-wm64DAVXj+iekzs1aq3RA1UK3BP6SXXA/XV5WhFiRVs=",
        urls = ["{}/kconfig-windows-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-z9T8SKjgok47vQo+o3iOFQjkfDYwiwj2pR+sROaov6o=",
        urls = ["{}/kconfig-windows-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
}
