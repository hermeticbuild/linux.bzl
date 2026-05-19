"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract a host executable named `kconfig`.
"""

KCONFIG_TOOL_VERSION = "v0.0.3"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-xckU1U+RZgueGU8Sm5jEznFTzwuCa+PXigGaDBfC8Xc=",
        urls = ["{}/kconfig-darwin-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-f4g0y6o97cqCoNFKGRfb2ESMtGf1Fs2zku2/7OKGCcA=",
        urls = ["{}/kconfig-darwin-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-c1HLFURtSrevrG5h16hOu0Dl52BK+ZRjmmFDOrib/V0=",
        urls = ["{}/kconfig-linux-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-2hMOMke4FQLnEPlRBTvLc8S98b2jhTPAGIPpojm/TSs=",
        urls = ["{}/kconfig-linux-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-BpKDzcx9K1Lb7U8NhRUujhY09IjitOzJaOVIMgyw+P8=",
        urls = ["{}/kconfig-windows-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-phHMAG8HggtLBRYItRYEvcdzrUpeJEsEk2iZJm8C2zY=",
        urls = ["{}/kconfig-windows-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
}
