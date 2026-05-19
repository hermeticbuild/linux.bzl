"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

KCONFIG_TOOL_VERSION = "v0.0.6"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-1fwOVHEgYWmhGcbB0Jd1iuOjpF85+jfXIHQUMvC/O1E=",
        urls = ["{}/kconfig-darwin-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-AkH/1dnZBydQ+BIT7hwEzVUIwy5a8HMxbc2PAZ/DA9Q=",
        urls = ["{}/kconfig-darwin-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-xhVDkfIC+c/qQ9Lfl+Kfy2St+c4v3MJrnGxAwKSwQWE=",
        urls = ["{}/kconfig-linux-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-NKfe4Lvf1/GWjdyBpSD5yRW26iLNgiR1N+viAcNg1U0=",
        urls = ["{}/kconfig-linux-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-wnivZ3wcXjSErrZfwgrNzd7pfkGUOREoGNAVDtBqxn0=",
        urls = ["{}/kconfig-windows-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-GgNpdQ3A85jiVy26rct2JF9xpBJrp4dtd6ClxBJ4x4M=",
        urls = ["{}/kconfig-windows-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
}
