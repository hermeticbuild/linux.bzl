"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

visibility("//...")

KCONFIG_TOOL_VERSION = "v0.0.19"

_RELEASE_BASE_URL = "https://github.com/fionera/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-5IGOdxsCqDrJsf2qezdDvzESRdJzLa+dW6uaKFkStBU=",
        urls = ["{}/kconfig-darwin-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-rWn1uxOyoUU+ivP+joYSgE9tZH9ruV1haarM8BubUSA=",
        urls = ["{}/kconfig-darwin-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-tM+CyiakiZLasGuMZYzfZO8Lgc8RHqjZJxFFStFGiqI=",
        urls = ["{}/kconfig-linux-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-avglG6l8d/tJr1ZTJZxxCTa/cyTVw+95EmM/JobL53k=",
        urls = ["{}/kconfig-linux-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-Ye3lTjbo8lh5tCB5cdqjf8SfxJ0Q4wInzuOCZLf6rxQ=",
        urls = ["{}/kconfig-windows-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-odY7hrYomBeKUAzv57qQEIgYFz9+U13B7xMIDFl00Zc=",
        urls = ["{}/kconfig-windows-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
}
