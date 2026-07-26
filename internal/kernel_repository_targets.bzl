"""Private target graph emitted by linux_image."""

load(":architectures.bzl", "linux_architectures")
load(":kernel_bundle.bzl", "linux_kernel_bundle", "linux_kernel_exports")
load(":linux_modules.bzl", "linux_module_sdk")
load(":linux_objects.bzl", "linux_compressed_image", "linux_resolved_config", "linux_vmlinux")
load(":linux_rust.bzl", "linux_disabled_rust_kernel_sdk", "linux_rust_kernel_sdk")

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
        config_mode,
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
        config_mode = config_mode,
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
        rust_enabled,
        source_repo,
        version,
        visibility):
    rust_sdk = prefix + "_rust_sdk"
    vmlinux = prefix + "_vmlinux"
    module_sdk = prefix + "_module_sdk"
    image = prefix + "_image"
    if rust_enabled:
        rust_sdk_kwargs = {
            "name": rust_sdk,
            "arch": arch.arch,
            "config": config,
            "exec_compatible_with": [
                "@platforms//cpu:x86_64",
                "@platforms//os:linux",
            ],
            "generated_headers": host_tools.generated_headers,
            "source_root": _source_label(source_repo, "Kconfig"),
            "source_tree": _source_tree_inputs(source_repo),
            "srcarch": arch.srcarch,
            "target_compatible_with": [arch.platform],
            "version": version,
            "visibility": visibility,
        }
        if hasattr(host_tools, "objtool"):
            rust_sdk_kwargs["objtool"] = host_tools.objtool
        linux_rust_kernel_sdk(**rust_sdk_kwargs)
    else:
        linux_disabled_rust_kernel_sdk(
            name = rust_sdk,
            target_compatible_with = [arch.platform],
            visibility = visibility,
        )

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
        "pahole": Label("@pahole//:pahole"),
        "resolve_btfids_tool": host_tools.resolve_btfids_tool,
        "rust_sdk": ":" + rust_sdk,
        "sorttable_tool": host_tools.sorttable_tool,
        "source_root": _source_label(source_repo, "Kconfig"),
        "source_tree": _source_tree_inputs(source_repo),
        "srcarch": arch.srcarch,
        "target_compatible_with": [arch.platform],
        "visibility": visibility,
        "version": version,
    }
    if hasattr(host_tools, "objtool"):
        vmlinux_kwargs["objtool"] = host_tools.objtool
    linux_vmlinux(**vmlinux_kwargs)

    linux_module_sdk(
        name = module_sdk,
        arch = arch.arch,
        pahole = Label("@pahole//:pahole"),
        resolve_btfids_tool = host_tools.resolve_btfids_tool,
        target_compatible_with = [arch.platform],
        version = version,
        visibility = visibility,
        vmlinux = ":" + vmlinux,
    )

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
        module_sdk = ":" + module_sdk,
        vmlinux = ":" + vmlinux,
    )

def linux_image_targets(
        name,
        arch,
        version,
        source_repo,
        platform,
        base_config,
        base_rust_enabled,
        graph_image,
        config_mode = "default",
        variant_configs = {},
        variant_graph_images = {},
        variant_rust_enabled = {}):
    """Defines private kernel graphs and the base stable exports."""
    if type(base_rust_enabled) != "bool":
        fail("base_rust_enabled must be a bool")
    if sorted(variant_configs.keys()) != sorted(variant_rust_enabled.keys()):
        fail("variant_rust_enabled must contain exactly the variant config names")
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
        config_mode = config_mode,
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
        rust_enabled = base_rust_enabled,
        source_repo = source_repo,
        version = version,
        visibility = internal_visibility,
    )
    linux_kernel_bundle(
        name = name,
        arch = arch,
        config = ":_base_config",
        image = outputs.image,
        module_sdk = outputs.module_sdk,
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
            config_mode = config_mode,
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
            rust_enabled = variant_rust_enabled[variant],
            source_repo = source_repo,
            version = version,
            visibility = internal_visibility,
        )
        linux_kernel_bundle(
            name = prefix + "_graph",
            arch = arch,
            config = ":" + config_target,
            image = variant_outputs.image,
            module_sdk = variant_outputs.module_sdk,
            version = version,
            vmlinux = variant_outputs.vmlinux,
            visibility = ["//variants/%s:__pkg__" % variant],
        )
