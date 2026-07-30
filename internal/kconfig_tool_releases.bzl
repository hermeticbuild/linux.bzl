"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

visibility("//...")

KCONFIG_TOOL_VERSION = "v0.0.20"

_RELEASE_BASE_URL = "https://github.com/fionera/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-4a9XxbGk1UuJ9STpmc6QzeDAXabaqLBaWKesuoufABk=",
        urls = ["{}/kconfig-darwin-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-+MjTu9AhSrcE9ivRLeuKXJ3sdkn+6EpjGncFwhwRjag=",
        urls = ["{}/kconfig-darwin-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-+24xsB4RTcS4GIDGYbL527AIeBmZCcOktqt4IMb7Jc4=",
        urls = ["{}/kconfig-linux-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-l+w2S8Kn1FYdGo5N1dbBZzPSl9y9qPR2MxB2MaUtqaA=",
        urls = ["{}/kconfig-linux-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-3hME3J123pADposTnrlkH4/v94yDjGg805+Z0aA6wzs=",
        urls = ["{}/kconfig-windows-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-7QwTpD8rF5tpsm0A7PQZWqJgAvXnaY94hTK/1ab3cAU=",
        urls = ["{}/kconfig-windows-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
}
