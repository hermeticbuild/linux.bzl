"""Release metadata for prebuilt kconfig repository-rule tools.

Each archive must extract the requested host executables at its root.
"""

visibility("//...")

KCONFIG_TOOL_VERSION = "v0.0.15-candidate.5"

_RELEASE_BASE_URL = "https://github.com/fionera/linux.bzl/releases/download/kconfig-{version}".format(
    version = KCONFIG_TOOL_VERSION,
)

KCONFIG_TOOL_RELEASES = {
    "darwin_amd64": struct(
        integrity = "sha256-Ew1rsCUyIw8qS5T3wa3meSTZcc0ab/anloo0IGA66jY=",
        urls = ["{}/kconfig-darwin-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "darwin_arm64": struct(
        integrity = "sha256-QwOij0PkaXfskJ4eGWwqO7SXg7qJCsLdGyKOFekULUk=",
        urls = ["{}/kconfig-darwin-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_amd64": struct(
        integrity = "sha256-LGL2ezoZ5wYHD6tJusXzfATlzEfHPL3PGLWgvXTs7oY=",
        urls = ["{}/kconfig-linux-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "linux_arm64": struct(
        integrity = "sha256-Yd2dq11ivTN6BX/te9KPuXB291/MMbIvbmPFGCJEIuw=",
        urls = ["{}/kconfig-linux-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_amd64": struct(
        integrity = "sha256-C3b8MvOEAqFfiT00Tff6eKrIiaEhdUg5b9nb2iCLUQM=",
        urls = ["{}/kconfig-windows-amd64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
    "windows_arm64": struct(
        integrity = "sha256-Mo4UelgHApq/OiVwb4IVy3W9fj0DO4rs+sx0BGURMuo=",
        urls = ["{}/kconfig-windows-arm64.tar.zst".format(_RELEASE_BASE_URL)],
    ),
}
