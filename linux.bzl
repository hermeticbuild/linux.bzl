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
        "config_mode": attr.string(
            default = "default",
            doc = "Config resolver mode. Use allnoconfig for KCONFIG_ALLCONFIG-on-allnoconfig semantics.",
            values = [
                "default",
                "allnoconfig",
            ],
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
        "probe_values": attr.string_dict(
            doc = "Overrides for the selected Linux Kconfig probe model, for example cc_version or pahole_version.",
        ),
        "kbuild_tree": attr.bool(
            default = True,
            doc = "Follow active Kbuild directory descent when generating compact metadata.",
        ),
        "extra_sources": attr.string_list(
            doc = "Names of linux_kernel.extra_source tags to include in this compact source view.",
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

_extra_source = tag_class(
    attrs = {
        "kbuild": attr.label(
            allow_single_file = True,
            doc = "Kbuild or Makefile entry point for this extra source root.",
        ),
        "kconfig": attr.label(
            allow_single_file = True,
            doc = "Kconfig entry point for this extra source root.",
        ),
        "name": attr.string(
            doc = "Extra source name referenced by compact extra_sources.",
            mandatory = True,
        ),
        "source_dir": attr.string(
            doc = "Virtual Linux source directory, for example fs/actiondfs.",
            mandatory = True,
        ),
        "source_label_package": attr.string(
            doc = "Bazel label package for generated source labels. Defaults to the package of kbuild, kconfig, or the first src.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Files belonging to this extra source root.",
        ),
    },
)

_KCONFIG_TOOL_REPO = "linux_bzl_kconfig_tool"

_LINUX_ARCHIVE_EXCLUDES = [
    "Build",
    "Documentation",
    "Documentation/",
    "Documentation/*",
    "Documentation/**",
    "samples",
    "samples/",
    "samples/*",
    "samples/**",
    "*/Build",
]

_LINUX_ARCHIVE_PATCH_CMDS = [
    "rm -rf Documentation samples",
    "mkdir -p Documentation && : > Documentation/Kconfig",
    "mkdir -p samples && : > samples/Kconfig",
]

def _linux_kernel_impl(mctx):
    archives = {}
    compacts = {}
    extras = {}
    kconfigs = {}
    kconfig_tool = None
    kconfig_parse_tool = None
    for module in mctx.modules:
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
        for extra in module.tags.extra_source:
            if extra.name in extras:
                fail("duplicate Linux extra source %q" % extra.name)
            extras[extra.name] = extra
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
        patch_cmds = list(archive.patch_cmds) + _LINUX_ARCHIVE_PATCH_CMDS
        if patch_cmds:
            kwargs["patch_cmds"] = patch_cmds
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
        compact_vars = arch.compact_vars | {
            "ARCH": arch.arch,
            "SRCARCH": arch.srcarch,
            "UTS_MACHINE": arch.uts_machine,
        }
        env = {
            "ARCH": arch.arch,
            "SRCARCH": arch.srcarch,
        }
        label_repo = compact.config.repo_name
        extra_kconfigs = {}
        extra_kbuilds = {}
        extra_source_label_packages = {}
        extra_source_tree_labels = []
        for extra_name in compact.extra_sources:
            if extra_name not in extras:
                fail("compact repo %q references unknown Linux extra source %q" % (compact.name, extra_name))
            extra = extras[extra_name]
            prefix_dir = _source_dir(extra.source_dir)
            if extra.kconfig:
                extra_kconfigs[extra.kconfig] = prefix_dir
            if extra.kbuild:
                extra_kbuilds[extra.kbuild] = prefix_dir
            label_package = extra.source_label_package
            if not label_package:
                label_package = _label_package_for_extra(extra)
            if label_package:
                extra_source_label_packages[prefix_dir] = label_package
            for src in extra.srcs:
                extra_source_tree_labels.append(_label_string(src))
        linux_compact_repository(
            name = compact.name,
            config = compact.config,
            config_name = arch.config_name,
            config_mode = compact.config_mode,
            env = env,
            extra_kbuilds = extra_kbuilds,
            extra_kconfigs = extra_kconfigs,
            extra_source_label_packages = extra_source_label_packages,
            generated_headers = _package_label(label_repo, compact.kernel_package, prefix + "_" + arch.arch + "_generated_headers"),
            generated_visibility = compact.generated_visibility,
            kbuild = source_repo + "//:Kbuild",
            kbuild_tree = compact.kbuild_tree,
            kconfig_parse_tool = kconfig_parse_tool,
            probe_values = compact.probe_values,
            root = source_repo + "//:Kconfig",
            source_asn1_compiler = _package_label(label_repo, compact.kernel_package, prefix + "_asn1_compiler_tool"),
            source_relacheck = _package_label(label_repo, compact.kernel_package, prefix + "_relacheck_tool") if arch.config_name == "aarch64" else "",
            source_config = _package_label(label_repo, compact.kernel_package, prefix + "_config"),
            source_label_package = source_repo + "//",
            source_root_label = source_repo + "//:Kconfig",
            source_tree_labels = [source_repo + "//:all_files"] + extra_source_tree_labels,
            vars = compact_vars,
        )
    return mctx.extension_metadata(reproducible = True)

linux_kernel = module_extension(
    implementation = _linux_kernel_impl,
    tag_classes = {
        "archive": _archive,
        "compact": _compact,
        "extra_source": _extra_source,
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

def _source_dir(path):
    path = path.strip("/")
    if not path or path == ".":
        fail("Linux extra source_dir must not be empty")
    return path

def _label_string(label):
    repo = "@@%s" % label.repo_name if label.repo_name else "@@"
    if label.package:
        return "%s//%s:%s" % (repo, label.package, label.name)
    return "%s//:%s" % (repo, label.name)

def _label_package_for_extra(extra):
    labels = []
    if extra.kbuild:
        labels.append(extra.kbuild)
    if extra.kconfig:
        labels.append(extra.kconfig)
    labels.extend(extra.srcs)
    if not labels:
        return ""
    label = labels[0]
    repo = "@@%s" % label.repo_name if label.repo_name else "@@"
    if label.package:
        return "%s//%s" % (repo, label.package)
    return "%s//" % repo
