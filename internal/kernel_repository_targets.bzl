"""Private target graph emitted by linux_image."""

load(":architectures.bzl", "linux_architectures")
load(":kernel_bundle.bzl", "linux_kernel_bundle", "linux_kernel_exports")
load(":linux_objects.bzl", "linux_compressed_image", "linux_resolved_config", "linux_vmlinux")

visibility("//...")

def _architecture(config_name):
    for arch in linux_architectures():
        if arch.config_name == config_name:
            return arch
    fail("unsupported generated Linux architecture %r" % config_name)

def _source_label(source_repo, path):
    return "%s//:%s" % (source_repo, path)

def _source_tree_inputs(source_repo):
    return [
        _source_label(source_repo, "headers"),
        _source_label(source_repo, "source_tree_lookup_files"),
    ]

def _define_config(
        name,
        config,
        arch,
        source_repo,
        version,
        visibility):
    compact_vars = dict(arch.compact_vars)
    compact_vars.update({
        "ARCH": arch.arch,
        "SRCARCH": arch.srcarch,
        "UTS_MACHINE": arch.uts_machine,
    })
    linux_resolved_config(
        name = name,
        config = config,
        config_name = name,
        env = {
            "ARCH": arch.arch,
            "SRCARCH": arch.srcarch,
        },
        root = _source_label(source_repo, "Kconfig"),
        source_root = _source_label(source_repo, "Kconfig"),
        srcs = [_source_label(source_repo, "kconfig_files")],
        vars = compact_vars,
        version = version,
        visibility = visibility,
    )

def _define_outputs(
        prefix,
        arch,
        config,
        compact_image,
        host_tools,
        source_repo,
        visibility):
    vmlinux = prefix + "_vmlinux"
    image = prefix + "_image"
    vmlinux_kwargs = {
        "name": vmlinux,
        "arch": arch.arch,
        "config": config,
        "format": arch.vmlinux_format,
        "generated_headers": host_tools.generated_headers,
        "image": compact_image,
        "kallsyms": "auto",
        "kallsyms_tool": host_tools.kallsyms_tool,
        "linker_script": _source_label(source_repo, arch.vmlinux_linker_script),
        "sorttable_tool": host_tools.sorttable_tool,
        "source_root": _source_label(source_repo, "Kconfig"),
        "source_tree": _source_tree_inputs(source_repo),
        "srcarch": arch.srcarch,
        "target_compatible_with": [arch.platform],
        "visibility": visibility,
    }
    if hasattr(host_tools, "objtool"):
        vmlinux_kwargs["objtool"] = host_tools.objtool
    linux_vmlinux(**vmlinux_kwargs)

    image_kwargs = {
        "name": image,
        "arch": arch.arch,
        "config": config,
        "extension": arch.extension,
        "format": arch.compressed_format,
        "generated_headers": host_tools.generated_headers,
        "image": ":" + vmlinux,
        "source_root": _source_label(source_repo, "Kconfig"),
        "source_tree": _source_tree_inputs(source_repo),
        "srcarch": arch.srcarch,
        "target_compatible_with": [arch.platform],
        "visibility": visibility,
    }
    if hasattr(host_tools, "x86_relocs_tool"):
        image_kwargs["x86_relocs_tool"] = host_tools.x86_relocs_tool
    linux_compressed_image(**image_kwargs)
    return struct(
        image = ":" + image,
        vmlinux = ":" + vmlinux,
    )

def linux_image_targets(
        name,
        arch,
        version,
        source_repo,
        platform,
        base_config,
        graph_image,
        variant_configs = {},
        variant_graph_images = {}):
    """Defines private kernel graphs and the base stable exports."""
    descriptor = _architecture(arch)
    variant_packages = [
        "//variants/%s:__pkg__" % name
        for name in sorted(variant_configs.keys())
    ]
    internal_visibility = [
        "//:__pkg__",
        "//graph:__subpackages__",
    ] + variant_packages

    _define_config(
        name = "_base_config",
        config = base_config,
        arch = descriptor,
        source_repo = source_repo,
        version = version,
        visibility = internal_visibility,
    )
    host_tools = descriptor.host_tools(
        name = "_base_tools",
        config = ":_base_config",
        env = {
            "ARCH": descriptor.arch,
            "SRCARCH": descriptor.srcarch,
        },
        source_repo = source_repo,
        source_root = _source_label(source_repo, "Kconfig"),
        source_tree = _source_tree_inputs(source_repo),
        target_prefix = "_base",
        visibility = internal_visibility,
    )
    outputs = _define_outputs(
        prefix = "_base",
        arch = descriptor,
        config = ":_base_config",
        compact_image = graph_image,
        host_tools = host_tools,
        source_repo = source_repo,
        visibility = internal_visibility,
    )
    linux_kernel_bundle(
        name = name,
        arch = arch,
        config = ":_base_config",
        image = outputs.image,
        version = version,
        vmlinux = outputs.vmlinux,
        visibility = ["//:__pkg__"],
    )
    linux_kernel_exports(
        name = "kernel",
        graph = ":" + name,
        platform = platform,
        arch = arch,
    )

    for variant in sorted(variant_configs.keys()):
        if variant not in variant_graph_images:
            fail("variant %r is missing its generated graph image" % variant)
        prefix = "_variant_" + variant
        config_target = prefix + "_config"
        _define_config(
            name = config_target,
            config = variant_configs[variant],
            arch = descriptor,
            source_repo = source_repo,
            version = version,
            visibility = internal_visibility,
        )
        variant_host_tools = descriptor.host_tools(
            name = prefix + "_tools",
            config = ":" + config_target,
            env = {
                "ARCH": descriptor.arch,
                "SRCARCH": descriptor.srcarch,
            },
            source_repo = source_repo,
            source_root = _source_label(source_repo, "Kconfig"),
            source_tree = _source_tree_inputs(source_repo),
            target_prefix = prefix,
            visibility = internal_visibility,
        )
        variant_outputs = _define_outputs(
            prefix = prefix,
            arch = descriptor,
            config = ":" + config_target,
            compact_image = variant_graph_images[variant],
            host_tools = variant_host_tools,
            source_repo = source_repo,
            visibility = internal_visibility,
        )
        linux_kernel_bundle(
            name = prefix + "_graph",
            arch = arch,
            config = ":" + config_target,
            image = variant_outputs.image,
            version = version,
            vmlinux = variant_outputs.vmlinux,
            visibility = ["//variants/%s:__pkg__" % variant],
        )
