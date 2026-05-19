"""Public entry points for Bazel-native Linux kernel builds."""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")
load("//internal:kconfig_repositories.bzl", "kconfig_repository", "kconfig_tool_repository")
load("//internal:linux.bzl", _linux = "linux")
load("//internal:linux_objects.bzl", _linux_compressed_image = "linux_compressed_image", _linux_generated_file = "linux_generated_file", _linux_module = "linux_module", _linux_vmlinux = "linux_vmlinux")
load("//internal:qemu_boot.bzl", _linux_qemu_boot_test = "linux_qemu_boot_test")

linux = _linux
linux_compressed_image = _linux_compressed_image
linux_generated_file = _linux_generated_file
linux_module = _linux_module
linux_qemu_boot_test = _linux_qemu_boot_test
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

_kconfig = tag_class(
    attrs = {
        "config": attr.label(
            allow_single_file = True,
            doc = "Linux .config file used as the source of truth.",
            mandatory = True,
        ),
        "generated_visibility": attr.string_list(
            default = ["//visibility:public"],
            doc = "Visibility emitted on the generated kconfig_file target.",
        ),
        "kconfig_name": attr.string(
            default = "kconfig",
            doc = "Name of the generated kconfig_file target.",
        ),
        "name": attr.string(
            doc = "Generated repository name.",
            mandatory = True,
        ),
    },
)

_kconfig_tool = tag_class(
    attrs = {
        "tool": attr.label(
            allow_single_file = True,
            doc = "Optional host kconfig executable override. Intended for local development before published prebuilts are available.",
            mandatory = True,
        ),
    },
)

_KCONFIG_TOOL_REPO = "linux_bzl_kconfig_tool"

def _linux_kernel_impl(module_ctx):
    archives = {}
    kconfigs = {}
    kconfig_tool = None
    for module in module_ctx.modules:
        for archive in module.tags.archive:
            if archive.name in archives:
                fail("duplicate Linux archive repo %q" % archive.name)
            if archive.name in kconfigs:
                fail("duplicate generated repo %q" % archive.name)
            archives[archive.name] = archive
        for kconfig in module.tags.kconfig:
            if kconfig.name in archives or kconfig.name in kconfigs:
                fail("duplicate generated repo %q" % kconfig.name)
            if kconfig.name == _KCONFIG_TOOL_REPO:
                fail("kconfig repo name %q is reserved for the internal tool repository" % kconfig.name)
            kconfigs[kconfig.name] = kconfig
        for tool in module.tags.kconfig_tool:
            if kconfig_tool != None:
                fail("linux_kernel.kconfig_tool may only be declared once")
            kconfig_tool = tool.tool

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

    if kconfigs and kconfig_tool == None:
        kconfig_tool_repository(name = _KCONFIG_TOOL_REPO)
        kconfig_tool = "@%s//:kconfig" % _KCONFIG_TOOL_REPO

    for repo in sorted(kconfigs.keys()):
        kconfig = kconfigs[repo]
        kconfig_repository(
            name = repo,
            config = kconfig.config,
            generated_visibility = kconfig.generated_visibility,
            kconfig_name = kconfig.kconfig_name,
            kconfig_tool = kconfig_tool,
        )

linux_kernel = module_extension(
    implementation = _linux_kernel_impl,
    tag_classes = {
        "archive": _archive,
        "kconfig": _kconfig,
        "kconfig_tool": _kconfig_tool,
    },
)
