"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

KCONFIG_TOOL_VERSION = "v0.0.8"

_RELEASE_BASE_URL = "https://github.com/hermeticbuild/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-A6wTqg+Lyket2TFycSQFvCSGVGz3zL9Sixa1rpeWPb0=",
        urls = ["{}/kconfig-darwin-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-zH2qTlEzuPhsIpRqJNsVw/v8/YJIs7vWzZjMovWp6LY=",
        urls = ["{}/kconfig-darwin-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-XQAlqx2gc81FSFKmXRtrBS3SNu5+drEEM60CHsAGYLI=",
        urls = ["{}/kconfig-linux-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-4jKDk4ARnCcUL/hKbwizNQEVFn9+1hAOgebDt6Mu8Tk=",
        urls = ["{}/kconfig-linux-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-Lr/ZSmGCbIIXKxtRNmQnR5EJoW9a/PdstWUM7UhsT2s=",
        urls = ["{}/kconfig-windows-amd64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-dLZf7BHDvNzp9XsRv5WMBqoR+n7oRP95aCxKJOe/S9A=",
        urls = ["{}/kconfig-windows-arm64.tar.gz".format(_RELEASE_BASE_URL)],
    ),
}
