"""Public API for hermetic, Bazel-native Linux kernel builds.

All supported public symbols are exported from this file. Files below
//internal are implementation details and may change without notice.
"""

load("//internal:initramfs.bzl", _initramfs = "initramfs")
load("//internal:kconfig.bzl", _linux_internal_kconfig_file = "kconfig_file")
load("//internal:kernel_bundle.bzl", _linux_internal_kernel_exports = "linux_kernel_exports")
load("//internal:kernel_repository_targets.bzl", _linux_internal_image_targets = "linux_image_targets")
load(
    "//internal:linux_objects.bzl",
    _linux_internal_arm64_nvhe_object = "linux_arm64_nvhe_object",
    _linux_internal_compact_image = "linux_compact_image",
    _linux_internal_composite_object = "linux_composite_object",
    _linux_internal_config = "linux_config",
    _linux_internal_object = "linux_object",
    _linux_internal_source_tree = "linux_source_tree",
)
load("//internal:providers.bzl", _LinuxKernelInfo = "LinuxKernelInfo")
load("//internal:repositories.bzl", _linux_image = "linux_image", _linux_source_repository = "linux_source_repository")

visibility("public")

LinuxKernelInfo = _LinuxKernelInfo
initramfs = _initramfs
linux_image = _linux_image
linux_source_repository = _linux_source_repository

# These aliases are an implementation protocol used only by BUILD files emitted
# into generated kernel repositories. They are not part of the supported API.
linux_internal_arm64_nvhe_object = _linux_internal_arm64_nvhe_object
linux_internal_compact_image = _linux_internal_compact_image
linux_internal_composite_object = _linux_internal_composite_object
linux_internal_config = _linux_internal_config
linux_internal_image_targets = _linux_internal_image_targets
linux_internal_kernel_exports = _linux_internal_kernel_exports
linux_internal_kconfig_file = _linux_internal_kconfig_file
linux_internal_object = _linux_internal_object
linux_internal_source_tree = _linux_internal_source_tree
