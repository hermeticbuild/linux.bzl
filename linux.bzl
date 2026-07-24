"""Public API for hermetic, Bazel-native Linux kernel builds.

All supported public symbols are exported from this file. Files below
//internal are implementation details and may change without notice.
"""

load("//internal:providers.bzl", _LinuxKernelInfo = "LinuxKernelInfo")
load("//internal:repositories.bzl", _linux_image = "linux_image", _linux_source_repository = "linux_source_repository")

LinuxKernelInfo = _LinuxKernelInfo
linux_image = _linux_image
linux_source_repository = _linux_source_repository
