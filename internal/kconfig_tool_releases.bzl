"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

visibility("//...")

KCONFIG_TOOL_VERSION = "v0.0.13"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-VbTy013fU6sDaSG7so6GlfhxJsh1nBUhgs4CoG6oRs4=",
        urls = ["{}/kconfig-darwin-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-SPyePCQ4eRSw9tC6wFgZwsoUc9gqOg4h8V117JbnhFw=",
        urls = ["{}/kconfig-darwin-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-gVgzWUoPMTm8bnHBZED3LNQcndMU7+4VTQb3Lp0AQtE=",
        urls = ["{}/kconfig-linux-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-RzFvFB/l2F+sg5DYNZm+3l7krPl7eIi9c0Rb9oBx9o8=",
        urls = ["{}/kconfig-linux-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-N7ncGu2gSMkl9xs4Rm2XvtCsjrZotOysvmm91nRb87s=",
        urls = ["{}/kconfig-windows-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-+Mm6T+T8LOFa9tBHlM24BiIJQdwqLrjIbKrOkGfRmzw=",
        urls = ["{}/kconfig-windows-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
}
