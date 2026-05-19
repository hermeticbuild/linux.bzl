"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

KCONFIG_TOOL_VERSION = "v0.0.4"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-tNRT5Z+sVri0o2l8ev36F11GbE85kuVoGUSmQCD81Zw=",
        urls = ["{}/kconfig-darwin-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-h719yz7wATA5Qj9VHEFIwTU3tfam1PiO/tXsUQ+xPTQ=",
        urls = ["{}/kconfig-darwin-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-hhzptdcVynEaEz3+eocUZOE4Goj0E8F1Gen0tAKp3U8=",
        urls = ["{}/kconfig-linux-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-WmY8caRDlHF3dRPUjQ2y34dsQ0kfhAVlsjuHMzizaEw=",
        urls = ["{}/kconfig-linux-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-Y4oZupu5WHU9BafGNTG1mBy7c+Zr7UxH8u8A48JRcYk=",
        urls = ["{}/kconfig-windows-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-LDf9u8apm9pU2v2rM5JgLo5Bl96XBburzXE10pinzek=",
        urls = ["{}/kconfig-windows-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
}
