"""High-level Linux kernel image macro."""

load(":architectures.bzl", "linux_architectures")
load(":compact_generator.bzl", "linux_compact_buildfiles")
load(":linux_objects.bzl", "linux_compressed_image", "linux_resolved_config", "linux_vmlinux")

def linux(
        name,
        config,
        source_repo = "@linux_6_18_2",
        image_format = "auto",
        generated_dir = "generated",
        visibility = None,
        tags = None):
    """Build a Linux kernel image for the active Bazel platform.

    The public target is a platform-selected alias. Each supported architecture
    gets its own resolved config, generated Kbuild BUILD files, host tools and
    final image target so Bazel can cache each architecture independently. The
    config label may itself be a platform-selected alias.
    """
    source_repo = _normalize_repo(source_repo)
    source_root = _repo_label(source_repo, "Kconfig")
    source_tree = [_repo_label(source_repo, "all")]
    package_visibility = _package_visibility()
    actuals = {}

    for arch in linux_architectures():
        actuals[arch.platform] = _define_arch_kernel(
            name = name,
            arch = arch,
            config = config,
            generated_dir = generated_dir,
            image_format = image_format,
            package_visibility = package_visibility,
            source_repo = source_repo,
            source_root = source_root,
            source_tree = source_tree,
            tags = tags,
        )

    native.alias(
        name = name,
        actual = select(actuals),
        visibility = visibility,
    )

def _normalize_repo(repo):
    if repo.startswith("@"):
        return repo
    return "@" + repo

def _repo_label(repo, name):
    return "%s//:%s" % (repo, name)

def _define_arch_kernel(
        name,
        arch,
        config,
        generated_dir,
        image_format,
        package_visibility,
        source_repo,
        source_root,
        source_tree,
        tags):
    prefix = name + "_" + arch.config_name
    config_target = prefix + "_config"
    tools_target = prefix + "_tools"
    buildfiles_target = prefix + "_buildfiles"
    vmlinux_target = prefix + "_vmlinux"
    image_target = prefix + "_" + arch.final_suffix
    arch_generated_dir = _join_path(generated_dir, arch.config_name)
    compact_package = _join_package(native.package_name(), _join_path(arch_generated_dir, "build"))
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

    linux_resolved_config(
        name = config_target,
        srcs = source_tree,
        config = config,
        env = env,
        root = source_root,
        source_root = source_root,
        target_compatible_with = [arch.platform],
        vars = compact_vars,
        visibility = package_visibility,
    )

    host_tools = arch.host_tools(
        name = tools_target,
        config = ":" + config_target,
        env = env,
        source_repo = source_repo,
        source_root = source_root,
        source_tree = source_tree,
        target_prefix = prefix,
        visibility = package_visibility,
    )

    linux_compact_buildfiles(
        name = buildfiles_target,
        config = config,
        config_name = arch.config_name,
        generated_headers = host_tools.generated_headers,
        generated_visibility = package_visibility,
        kbuild = _repo_label(source_repo, "Kbuild"),
        kbuild_tree = True,
        object_label_package = compact_package,
        out_buildfile = _join_path(arch_generated_dir, "build/BUILD.bazel"),
        out_metadata = _join_path(arch_generated_dir, "build/metadata.json"),
        probe_config = host_tools.probe_config,
        root = source_root,
        source_asn1_compiler = host_tools.source_asn1_compiler,
        source_config = _package_label(config_target),
        source_label_package = host_tools.source_label_package,
        source_root_label = host_tools.source_root,
        source_tree_labels = host_tools.source_tree,
        srcs = source_tree,
        target_compatible_with = [arch.platform],
        tags = tags,
        vars = compact_vars,
        visibility = package_visibility,
    )

    vmlinux_kwargs = {
        "name": vmlinux_target,
        "arch": arch.arch,
        "config": ":" + config_target,
        "format": arch.vmlinux_format,
        "generated_headers": host_tools.generated_headers,
        "image": "//%s:%s_image" % (compact_package, arch.config_name),
        "kallsyms_tool": host_tools.kallsyms_tool,
        "linker_script": _repo_label(source_repo, arch.vmlinux_linker_script),
        "source_root": source_root,
        "source_tree": source_tree,
        "srcarch": arch.srcarch,
        "visibility": package_visibility,
    }
    if hasattr(host_tools, "objtool"):
        vmlinux_kwargs["objtool"] = host_tools.objtool
    linux_vmlinux(**vmlinux_kwargs)

    image_kwargs = {
        "name": image_target,
        "arch": arch.arch,
        "config": ":" + config_target,
        "extension": arch.extension,
        "format": arch.compressed_format,
        "generated_headers": host_tools.generated_headers,
        "image": ":" + vmlinux_target,
        "source_root": source_root,
        "source_tree": source_tree,
        "srcarch": arch.srcarch,
        "visibility": package_visibility,
    }
    if hasattr(host_tools, "x86_relocs_tool"):
        image_kwargs["x86_relocs_tool"] = host_tools.x86_relocs_tool
    linux_compressed_image(**image_kwargs)

    return _image_actual(
        arch = arch,
        image_format = image_format,
        image_target = image_target,
        name = prefix + "_unsupported_image_format",
        vmlinux_target = vmlinux_target,
    )

def _image_actual(name, arch, image_format, image_target, vmlinux_target):
    if image_format in ["auto", "compressed", arch.extension, arch.final_suffix, arch.compressed_format]:
        return ":" + image_target
    if image_format == "vmlinux":
        return ":" + vmlinux_target

    _unsupported_image_format(
        name = name,
        allowed = [
            "auto",
            "compressed",
            "vmlinux",
            arch.extension,
            arch.final_suffix,
            arch.compressed_format,
        ],
        arch = arch.config_name,
        requested = image_format,
    )
    return ":" + name

def _unsupported_image_format_impl(ctx):
    fail("linux image_format %q is not supported for %s; allowed values: %s" % (
        ctx.attr.requested,
        ctx.attr.arch,
        ", ".join(ctx.attr.allowed),
    ))

_unsupported_image_format = rule(
    implementation = _unsupported_image_format_impl,
    attrs = {
        "allowed": attr.string_list(mandatory = True),
        "arch": attr.string(mandatory = True),
        "requested": attr.string(mandatory = True),
    },
)

def _package_label(name):
    package = native.package_name()
    if package:
        return "//%s:%s" % (package, name)
    return "//:%s" % name

def _package_visibility():
    package = native.package_name()
    if package:
        return [
            "//%s:__pkg__" % package,
            "//%s:__subpackages__" % package,
        ]
    return [
        "//:__pkg__",
        "//:__subpackages__",
    ]

def _join_package(parent, child):
    if not parent:
        return child
    if not child:
        return parent
    return parent + "/" + child

def _join_path(parent, child):
    if not parent:
        return child
    if not child:
        return parent
    return parent.rstrip("/") + "/" + child.lstrip("/")
