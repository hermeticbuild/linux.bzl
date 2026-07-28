"""Private target graph emitted by linux_image."""

load(":architectures.bzl", "linux_architectures")
load(":kernel_bundle.bzl", "linux_kernel_bundle", "linux_kernel_exports")
load(":linux_modules.bzl", "linux_module_sdk")
load(":linux_objects.bzl", "linux_compressed_image", "linux_resolved_config", "linux_vmlinux")
load(":linux_rust.bzl", "linux_disabled_rust_kernel_sdk", "linux_rust_kernel_sdk")

visibility("public")

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

def _define_core_outputs(
        prefix,
        arch,
        config,
        compact_image,
        host_tools,
        rust_profile_json,
        rust_enabled,
        source_repo,
        version,
        visibility):
    rust_sdk = prefix + "_rust_sdk"
    vmlinux = prefix + "_vmlinux"
    module_sdk = prefix + "_module_sdk"
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
            "profile_json": rust_profile_json,
            "source_root": _source_label(source_repo, "Kconfig"),
            "source_tree": _source_tree_inputs(source_repo),
            "srcarch": arch.srcarch,
            "target_compatible_with": [arch.platform],
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
    return struct(
        module_sdk = ":" + module_sdk,
        vmlinux = ":" + vmlinux,
    )

def _define_compressed_output(
        prefix,
        arch,
        config,
        host_tools,
        source_repo,
        vmlinux,
        visibility):
    image = prefix + "_image"
    image_kwargs = {
        "name": image,
        "arch": arch.arch,
        "config": config,
        "extension": arch.extension,
        "format": arch.compressed_format,
        "generated_headers": host_tools.generated_headers,
        "image": vmlinux,
        "source_root": _source_label(source_repo, "Kconfig"),
        "source_tree": _source_tree_inputs(source_repo),
        "srcarch": arch.srcarch,
        "target_compatible_with": [arch.platform],
        "visibility": visibility,
    }
    if hasattr(host_tools, "x86_relocs_tool"):
        image_kwargs["x86_relocs_tool"] = host_tools.x86_relocs_tool
    linux_compressed_image(**image_kwargs)
    return ":" + image

def linux_image_targets(
        name,
        arch,
        version,
        source_repo,
        rust_profile_json,
        platform,
        base_config,
        base_rust_enabled,
        graph_image,
        base_header_content_id = "",
        config_mode = "default",
        variant_configs = {},
        variant_core_configs = {},
        variant_graph_images = {},
        variant_header_content_ids = {},
        variant_header_configs = {},
        variant_rust_enabled = {}):
    """Defines private kernel graphs and the base stable exports."""
    if type(base_rust_enabled) != "bool":
        fail("base_rust_enabled must be a bool")
    if type(rust_profile_json) != "string":
        fail("rust_profile_json must be a string")
    if (base_rust_enabled or True in variant_rust_enabled.values()) and not rust_profile_json:
        fail("Rust-enabled Linux targets require a source-derived Rust profile")
    if sorted(variant_configs.keys()) != sorted(variant_rust_enabled.keys()):
        fail("variant_rust_enabled must contain exactly the variant config names")
    if variant_core_configs and sorted(variant_configs.keys()) != sorted(variant_core_configs.keys()):
        fail("variant_core_configs must contain exactly the variant config names when set")
    if variant_header_configs and sorted(variant_configs.keys()) != sorted(variant_header_configs.keys()):
        fail("variant_header_configs must contain exactly the variant config names when set")
    if variant_header_content_ids and sorted(variant_configs.keys()) != sorted(variant_header_content_ids.keys()):
        fail("variant_header_content_ids must contain exactly the variant config names when set")
    if variant_header_content_ids and not base_header_content_id:
        fail("base_header_content_id is required with variant_header_content_ids")
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
        generated_headers_content_id = base_header_content_id,
        visibility = internal_visibility,
    )
    core_outputs = _define_core_outputs(
        prefix = "_base",
        arch = descriptor,
        config = ":_base_config",
        compact_image = graph_image,
        host_tools = host_tools,
        rust_profile_json = rust_profile_json,
        rust_enabled = base_rust_enabled,
        source_repo = source_repo,
        version = version,
        visibility = internal_visibility,
    )
    image = _define_compressed_output(
        prefix = "_base",
        arch = descriptor,
        config = ":_base_config",
        host_tools = host_tools,
        source_repo = source_repo,
        vmlinux = core_outputs.vmlinux,
        visibility = internal_visibility,
    )
    linux_kernel_bundle(
        name = name,
        arch = arch,
        config = ":_base_config",
        image = image,
        module_sdk = core_outputs.module_sdk,
        version = version,
        vmlinux = core_outputs.vmlinux,
        visibility = ["//:__pkg__"],
    )
    linux_kernel_exports(
        name = "kernel",
        graph = ":" + name,
        platform = platform,
        arch = arch,
    )

    host_tools_by_config = {
        arch: host_tools,
    }
    core_outputs_by_config = {
        arch: core_outputs,
    }
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
        header_config = variant_header_configs.get(variant, variant)
        if header_config == variant:
            variant_host_tools = descriptor.configured_host_tools(
                name = prefix,
                config = ":" + config_target,
                shared = host_tools,
                source_repo = source_repo,
                source_root = _source_label(source_repo, "Kconfig"),
                source_tree = _source_tree_inputs(source_repo),
                generated_headers_content_id = variant_header_content_ids.get(variant, ""),
                visibility = internal_visibility,
            )
        elif header_config in host_tools_by_config:
            variant_host_tools = host_tools_by_config[header_config]
        else:
            fail(
                "variant %r generated headers alias unavailable config %r; aliases must point to the base or an earlier variant" %
                (variant, header_config),
            )
        host_tools_by_config[variant] = variant_host_tools
        core_config = variant_core_configs.get(variant, variant)
        if core_config == variant:
            variant_core_outputs = _define_core_outputs(
                prefix = prefix,
                arch = descriptor,
                config = ":" + config_target,
                compact_image = variant_graph_images[variant],
                host_tools = variant_host_tools,
                rust_profile_json = rust_profile_json,
                rust_enabled = variant_rust_enabled[variant],
                source_repo = source_repo,
                version = version,
                visibility = internal_visibility,
            )
        elif core_config in core_outputs_by_config:
            variant_core_outputs = core_outputs_by_config[core_config]
        else:
            fail(
                "variant %r core outputs alias unavailable config %r; aliases must point to the base or an earlier variant" %
                (variant, core_config),
            )
        core_outputs_by_config[variant] = variant_core_outputs
        variant_image = _define_compressed_output(
            prefix = prefix,
            arch = descriptor,
            config = ":" + config_target,
            host_tools = variant_host_tools,
            source_repo = source_repo,
            vmlinux = variant_core_outputs.vmlinux,
            visibility = internal_visibility,
        )
        linux_kernel_bundle(
            name = prefix + "_graph",
            arch = arch,
            config = ":" + config_target,
            image = variant_image,
            module_sdk = variant_core_outputs.module_sdk,
            version = version,
            vmlinux = variant_core_outputs.vmlinux,
            visibility = ["//variants/%s:__pkg__" % variant],
        )
