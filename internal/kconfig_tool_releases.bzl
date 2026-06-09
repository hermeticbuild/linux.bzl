"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

KCONFIG_TOOL_VERSION = "v0.0.9"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-RljQJDhEtpMJmbDkCI0veXnaeMP9IgPLWpFEpoLj9oc=",
        urls = ["{}/kconfig-darwin-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-hgtApLWr2AWS9NSvYojRCpRJn1rbwNQy8w6FSAVeMUY=",
        urls = ["{}/kconfig-darwin-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-+tM23nyxqGQh0Z2tCCgky2NNmAQDJ0PqKx1V87NbeO4=",
        urls = ["{}/kconfig-linux-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-sRn5WekSNN0nX+hP+bOfEEeWgNDfoXNjy9MU0KDSnhM=",
        urls = ["{}/kconfig-linux-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-LyUA6v/f+Hz+WtM6ecgwl06VmKRhW76qbuZu2gIYupw=",
        urls = ["{}/kconfig-windows-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-sBBLDaJkDIPKBQ/4r7/7Mf1Zme5UBDOeMnGGnDZ76fk=",
        urls = ["{}/kconfig-windows-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
}
