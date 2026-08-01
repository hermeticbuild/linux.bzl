"""Public build API for hermetic, Bazel-native Linux kernel builds.

Configured image repositories use //:extensions.bzl. Files below //internal
are implementation details and may change without notice.
"""

load("//internal:initramfs.bzl", _initramfs = "initramfs")
load("//internal:linux_modules.bzl", _linux_cc_module = "linux_cc_module", _linux_module = "linux_module")
load("//internal:linux_source_repository.bzl", _linux_source_repository = "linux_source_repository")
load("//internal:providers.bzl", _LinuxKernelInfo = "LinuxKernelInfo")

visibility("public")

LinuxKernelInfo = _LinuxKernelInfo
initramfs = _initramfs
linux_cc_module = _linux_cc_module
linux_module = _linux_module
linux_source_repository = _linux_source_repository
