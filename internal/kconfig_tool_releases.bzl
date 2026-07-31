"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

visibility("//...")

KCONFIG_TOOL_VERSION = "v0.0.21-rc1"

_RELEASE_BASE_URL = "https://github.com/fionera/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-exYEFsQPojFq5fN5+lR7wliGq1CU9AHUmu+URI3g9FY=",
        urls = ["{}/kconfig-darwin-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-WpZ444XqRv+QiVKnIuv7sO5KoKehy5CYLFc+UYtJgDM=",
        urls = ["{}/kconfig-darwin-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-OAgPMQ9jBHNFxy8UYacx0gAR/nslOq5E/ks9qbLC0xA=",
        urls = ["{}/kconfig-linux-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-Eqy1684FEYgKt1GCPFF4dIM4bP6jsAm+9GX/h9u7sJE=",
        urls = ["{}/kconfig-linux-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-ls7vaQVAv5p0ae8A6wjM2P6p9MWUFGaVM4i5BCdBkFo=",
        urls = ["{}/kconfig-windows-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-r1SYAvdOH7x+FNL2OGlIWu2N6BBiTzprFco4Hk91yLk=",
        urls = ["{}/kconfig-windows-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
}
