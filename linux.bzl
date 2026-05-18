"""Public entry points for Bazel-native Linux kernel builds."""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")
load("//internal:kconfig.bzl", _kconfig_buildfile = "kconfig_buildfile")
load("//internal:linux.bzl", _linux = "linux")
load("//internal:linux_objects.bzl", _linux_compressed_image = "linux_compressed_image", _linux_generated_file = "linux_generated_file", _linux_module = "linux_module", _linux_vmlinux = "linux_vmlinux")

kconfig_buildfile = _kconfig_buildfile
linux = _linux
linux_compressed_image = _linux_compressed_image
linux_generated_file = _linux_generated_file
linux_module = _linux_module
linux_vmlinux = _linux_vmlinux

_archive = tag_class(
    attrs = {
        "add_prefix": attr.string(doc = "Directory prefix to add to extracted files."),
        "canonical_id": attr.string(doc = "Canonical repository cache key for the archive."),
        "integrity": attr.string(doc = "Expected Subresource Integrity metadata for the archive."),
        "name": attr.string(mandatory = True, doc = "Generated source repository name."),
        "patch_args": attr.string_list(doc = "Arguments for the patch tool."),
        "patch_cmds": attr.string_list(doc = "Commands to run after extracting and patching."),
        "patch_strip": attr.int(default = -1, doc = "archive_override-style patch strip level. Mutually exclusive with patch_args."),
        "patch_tool": attr.string(doc = "Patch tool to use."),
        "patches": attr.label_list(allow_files = True, doc = "Patch files to apply after extracting the archive."),
        "sha256": attr.string(doc = "Expected SHA-256 digest of the archive."),
        "strip_prefix": attr.string(doc = "Directory prefix to strip after extracting the archive."),
        "type": attr.string(doc = "Archive type override."),
        "urls": attr.string_list(mandatory = True, doc = "Archive URLs."),
    },
)

def _linux_kernel_impl(module_ctx):
    archives = {}
    for module in module_ctx.modules:
        for archive in module.tags.archive:
            if archive.name in archives:
                fail("duplicate Linux archive repo %q" % archive.name)
            archives[archive.name] = archive

    for repo in sorted(archives.keys()):
        archive = archives[repo]
        kwargs = {
            "build_file": Label("//:source_repo.BUILD.bazel"),
            "name": archive.name,
            "urls": archive.urls,
        }
        if archive.integrity:
            kwargs["integrity"] = archive.integrity
        if archive.canonical_id:
            kwargs["canonical_id"] = archive.canonical_id
        if archive.add_prefix:
            kwargs["add_prefix"] = archive.add_prefix
        if archive.type:
            kwargs["type"] = archive.type
        if archive.sha256:
            kwargs["sha256"] = archive.sha256
        if archive.strip_prefix:
            kwargs["strip_prefix"] = archive.strip_prefix
        if archive.patches:
            kwargs["patches"] = archive.patches
        if archive.patch_strip >= 0:
            if archive.patch_args:
                fail("Linux archive repo %q sets both patch_strip and patch_args" % archive.name)
            kwargs["patch_args"] = ["-p%d" % archive.patch_strip]
        elif archive.patch_args:
            kwargs["patch_args"] = archive.patch_args
        if archive.patch_cmds:
            kwargs["patch_cmds"] = archive.patch_cmds
        if archive.patch_tool:
            kwargs["patch_tool"] = archive.patch_tool
        http_archive(**kwargs)

linux_kernel = module_extension(
    implementation = _linux_kernel_impl,
    tag_classes = {
        "archive": _archive,
    },
)
