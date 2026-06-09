"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

KCONFIG_TOOL_VERSION = "v0.0.10"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-79ZvPXUUgjYt5L/8TXcsuGJHyx/TMcbxeMJVXfPOAjU=",
        urls = ["{}/kconfig-darwin-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-t4uginHgtBk8xRld/HEmEvTbmAcVXy92q0LOdQZvCWE=",
        urls = ["{}/kconfig-darwin-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-onhDptEXE1NBfxKb3Gbqcq2b1xaQORBrgGMqFT5gFJo=",
        urls = ["{}/kconfig-linux-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-oAKuoyC/DETWedDXAfjLIbcyuPCb2Na+qZOexnnOWBo=",
        urls = ["{}/kconfig-linux-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-pCL6m6i+E0GxmAKf4kacGOB/xADj1JTUSUWpABygf9c=",
        urls = ["{}/kconfig-windows-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-OTTOMUHndHsbaXp0zSo87ysCGrE7TmEF9MjMMnKigEA=",
        urls = ["{}/kconfig-windows-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
}
