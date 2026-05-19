"""Public entry points for Bazel-native Linux kernel builds."""

load("@llvm//:http_bsdtar_archive.bzl", "http_bsdtar_archive")
load("//internal:architectures.bzl", "linux_architectures")
load("//internal:compact_repositories.bzl", "linux_compact_repository")
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
        "bsdtar_extra_args": attr.string_list(doc = "Additional arguments passed to bsdtar while extracting the archive."),
        "canonical_id": attr.string(doc = "Canonical repository cache key for the archive."),
        "excludes": attr.string_list(doc = "Archive paths to exclude while extracting."),
        "includes": attr.string_list(doc = "Archive paths to include while extracting."),
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
        "parse_tool": attr.label(
            allow_single_file = True,
            doc = "Optional host kconfig_parse executable override. Required with local tool overrides when compact repositories are declared.",
        ),
        "tool": attr.label(
            allow_single_file = True,
            doc = "Optional host kconfig executable override. Intended for local development before published prebuilts are available.",
            mandatory = True,
        ),
    },
)

_compact = tag_class(
    attrs = {
        "config": attr.label(
            allow_single_file = True,
            doc = "Linux .config fragment used as the source of truth.",
            mandatory = True,
        ),
        "config_name": attr.string(
            doc = "Linux architecture config name, such as x86_64 or aarch64.",
            mandatory = True,
        ),
        "generated_visibility": attr.string_list(
            default = ["//visibility:public"],
            doc = "Visibility emitted into the generated compact BUILD file.",
        ),
        "kernel_name": attr.string(
            doc = "Name of the linux(...) target consuming this compact repository.",
            mandatory = True,
        ),
        "kernel_package": attr.string(
            doc = "Package containing the linux(...) target. Empty means the root package.",
        ),
        "kbuild_tree": attr.bool(
            default = True,
            doc = "Follow active Kbuild directory descent when generating compact metadata.",
        ),
        "name": attr.string(
            doc = "Generated compact repository name.",
            mandatory = True,
        ),
        "source_repo": attr.string(
            doc = "Generated or external Linux source repository name.",
            mandatory = True,
        ),
    },
)

_KCONFIG_TOOL_REPO = "linux_bzl_kconfig_tool"

_LINUX_ARCHIVE_EXCLUDES = [
    "Build",
    "*/Build",
]

def _linux_kernel_impl(module_ctx):
    archives = {}
    compacts = {}
    kconfigs = {}
    kconfig_tool = None
    kconfig_parse_tool = None
    for module in module_ctx.modules:
        for archive in module.tags.archive:
            if archive.name in archives:
                fail("duplicate Linux archive repo %q" % archive.name)
            if archive.name in compacts or archive.name in kconfigs:
                fail("duplicate generated repo %q" % archive.name)
            archives[archive.name] = archive
        for compact in module.tags.compact:
            if compact.name in archives or compact.name in compacts or compact.name in kconfigs:
                fail("duplicate generated repo %q" % compact.name)
            if compact.name == _KCONFIG_TOOL_REPO:
                fail("compact repo name %q is reserved for the internal tool repository" % compact.name)
            compacts[compact.name] = compact
        for kconfig in module.tags.kconfig:
            if kconfig.name in archives or kconfig.name in compacts or kconfig.name in kconfigs:
                fail("duplicate generated repo %q" % kconfig.name)
            if kconfig.name == _KCONFIG_TOOL_REPO:
                fail("kconfig repo name %q is reserved for the internal tool repository" % kconfig.name)
            kconfigs[kconfig.name] = kconfig
        for tool in module.tags.kconfig_tool:
            if kconfig_tool != None:
                fail("linux_kernel.kconfig_tool may only be declared once")
            kconfig_tool = tool.tool
            kconfig_parse_tool = tool.parse_tool

    for repo in sorted(archives.keys()):
        archive = archives[repo]
        kwargs = {
            "build_file": Label("//:source_repo.BUILD.bazel"),
            "excludes": _linux_archive_excludes(archive.excludes),
            "name": archive.name,
            "urls": archive.urls,
        }
        if archive.integrity:
            kwargs["integrity"] = archive.integrity
        if archive.canonical_id:
            kwargs["canonical_id"] = archive.canonical_id
        if archive.add_prefix:
            kwargs["add_prefix"] = archive.add_prefix
        if archive.bsdtar_extra_args:
            kwargs["bsdtar_extra_args"] = archive.bsdtar_extra_args
        if archive.includes:
            kwargs["includes"] = archive.includes
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
        http_bsdtar_archive(**kwargs)

    if (kconfigs or compacts) and kconfig_tool == None:
        tools = ["kconfig"]
        if compacts:
            tools.append("kconfig_parse")
        kconfig_tool_repository(
            name = _KCONFIG_TOOL_REPO,
            tools = tools,
        )
        kconfig_tool = "@%s//:kconfig" % _KCONFIG_TOOL_REPO
        if compacts:
            kconfig_parse_tool = "@%s//:kconfig_parse" % _KCONFIG_TOOL_REPO
    elif compacts and kconfig_parse_tool == None:
        fail("linux_kernel.kconfig_tool(parse_tool = ...) is required when compact repositories are declared with a local kconfig tool override")

    for repo in sorted(kconfigs.keys()):
        kconfig = kconfigs[repo]
        kconfig_repository(
            name = repo,
            config = kconfig.config,
            generated_visibility = kconfig.generated_visibility,
            kconfig_name = kconfig.kconfig_name,
            kconfig_tool = kconfig_tool,
        )

    for repo in sorted(compacts.keys()):
        compact = compacts[repo]
        arch = _architecture(compact.config_name)
        source_repo = _normalize_repo(compact.source_repo)
        prefix = compact.kernel_name + "_" + arch.config_name
        compact_vars = dict(arch.compact_vars)
        compact_vars.update({
            "ARCH": arch.arch,
            "SRCARCH": arch.srcarch,
            "UTS_MACHINE": arch.uts_machine,
        })
        env = {
            "ARCH": arch.arch,
            "SRCARCH": arch.srcarch,
        }
        label_repo = compact.config.repo_name
        linux_compact_repository(
            name = compact.name,
            config = compact.config,
            config_name = arch.config_name,
            env = env,
            generated_headers = _package_label(label_repo, compact.kernel_package, prefix + "_" + arch.arch + "_generated_headers"),
            generated_visibility = compact.generated_visibility,
            kbuild = "%s//:Kbuild" % source_repo,
            kbuild_tree = compact.kbuild_tree,
            kconfig_parse_tool = kconfig_parse_tool,
            root = "%s//:Kconfig" % source_repo,
            source_asn1_compiler = _package_label(label_repo, compact.kernel_package, prefix + "_asn1_compiler_tool"),
            source_config = _package_label(label_repo, compact.kernel_package, prefix + "_config"),
            source_label_package = source_repo + "//",
            source_root_label = "%s//:Kconfig" % source_repo,
            source_tree_labels = ["%s//:all" % source_repo],
            vars = compact_vars,
        )

linux_kernel = module_extension(
    implementation = _linux_kernel_impl,
    tag_classes = {
        "archive": _archive,
        "compact": _compact,
        "kconfig": _kconfig,
        "kconfig_tool": _kconfig_tool,
    },
)

def _architecture(config_name):
    for arch in linux_architectures():
        if arch.config_name == config_name:
            return arch
    fail("unsupported Linux compact config_name %q" % config_name)

def _normalize_repo(repo):
    if repo.startswith("@"):
        return repo
    return "@" + repo

def _linux_archive_excludes(excludes):
    seen = {}
    merged = []
    for exclude in list(excludes) + _LINUX_ARCHIVE_EXCLUDES:
        if exclude in seen:
            continue
        seen[exclude] = True
        merged.append(exclude)
    return merged

def _package_label(repo_name, package, name):
    repo = "@@%s" % repo_name if repo_name else "@@"
    if package:
        return "%s//%s:%s" % (repo, package, name)
    return "%s//:%s" % (repo, name)
