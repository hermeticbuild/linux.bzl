"""Hermetic Linux image repository rule."""

load(":config_validation.bzl", "validate_config_features")
load(":kconfig_tool_filename.bzl", "kconfig_tool_filename")
load(":kconfig_tool_releases.bzl", "KCONFIG_TOOL_RELEASES", "KCONFIG_TOOL_VERSION")
load(
    ":repository_utils.bzl",
    _SOURCE_REPOSITORY_PROTOCOL = "LINUX_SOURCE_REPOSITORY_PROTOCOL",
    _linux_makefile_version = "linux_makefile_version",
    _repository_prefix = "repository_prefix",
)

visibility("//...")

_ARCHITECTURES = {
    "aarch64": struct(
        arch = "arm64",
        compact_vars = {
            "ARCH_CORE": "",
            "ARCH_DRIVERS": "",
            "ARCH_LIB": "arch/arm64/lib/ lib/",
            "BITS": "64",
            "CFLAGS_UBSAN_TRAP": "-fsanitize-trap=undefined",
            "CC_FLAGS_FPU": "-ffreestanding -D_LINUX_FPU_COMPILATION_UNIT",
            "CC_FLAGS_FTRACE": "",
            "CC_FLAGS_LTO": "",
            "CC_FLAGS_NO_FPU": "-mgeneral-regs-only",
            "CC_FLAGS_SCS": "",
            "DISABLE_KSTACK_ERASE": "",
            "DISABLE_LATENT_ENTROPY_PLUGIN": "",
            "PROFILING": "",
        },
        srcarch = "arm64",
        uts_machine = "aarch64",
    ),
    "x86_64": struct(
        arch = "x86",
        compact_vars = {
            "ARCH_CORE": "",
            "ARCH_DRIVERS": "arch/x86/pci/ arch/x86/power/",
            "ARCH_LIB": "lib/ arch/x86/lib/",
            "BITS": "64",
            "CFLAGS_UBSAN_TRAP": "-fsanitize-trap=undefined",
            "PROFILING": "",
        },
        srcarch = "x86",
        uts_machine = "x86_64",
    ),
}

_ARCH_CONFIGS = {
    "CONFIG_ARM64": "aarch64",
    "CONFIG_X86_64": "x86_64",
}

_PROBE_ENV = {
    "ARCH": "",
    "AR": "llvm-ar",
    "BINDGEN": "bindgen",
    "CC": "clang",
    "CC_VERSION_TEXT": "clang version 22.1.8None",
    "CLANG_FLAGS": "-fintegrated-as",
    "LD": "ld.lld",
    "NM": "llvm-nm",
    "PAHOLE": "pahole",
    "RUSTC": "rustc",
    "SRCARCH": "",
}

_PROBE_VALUES = {
    "bindgen_version": "bindgen 0.72.1",
    "cc_version": "220108",
    "cc_version_text": "clang version 22.1.8None",
    "ld_version": "220108",
    "pahole_version": "131",
    "rust_available": "true",
    "rust_options": "true",
    "rustc_llvm_version": "220106",
    "rustc_version": "109700",
}

_REPOSITORY_COMPACT_SCHEMA = "v0.0.12"
_CONTENT_COMPACT_SCHEMA = "v0.0.13"
_REPOSITORY_GENERATOR_PROTOCOL = "compact-v3-rust-profile"
_LLVM_VERSION = "22.1.8"
_IMAGE_COMPRESSION_CONFIGS = {
    "CONFIG_KERNEL_GZIP": True,
    "CONFIG_KERNEL_LZ4": True,
}

def _linux_image_impl(rctx):
    source = rctx.attr.source
    if source.name != "Kconfig" or source.package != "":
        fail("source must be the root Kconfig label from linux_source_repository")
    if source.repo_name == "":
        fail("source must be in a dedicated external repository")

    source_root = rctx.path(source).dirname
    rctx.watch_tree(source_root)
    makefile = source_root.get_child("Makefile")
    kbuild = source_root.get_child("Kbuild")
    marker = source_root.get_child(".linux-bzl-source.json")
    if not makefile.exists or not kbuild.exists or not marker.exists:
        fail(
            "source must come from linux_source_repository and contain its " +
            ".linux-bzl-source.json marker",
        )
    version = _linux_makefile_version(rctx.read(makefile))
    source_metadata = json.decode(rctx.read(marker))
    source_integrity = source_metadata.get("integrity", "") if type(source_metadata) == "dict" else ""
    if (
        type(source_metadata) != "dict" or
        source_metadata.get("protocol") != _SOURCE_REPOSITORY_PROTOCOL or
        source_metadata.get("version") != version or
        type(source_integrity) != "string" or
        not source_integrity.startswith("sha256-")
    ):
        fail("source repository has invalid or incompatible linux.bzl metadata")
    rules_repo = _repository_prefix(rctx.attr._self_linux_bzl)
    _validate_llvm_profile(rctx)

    base_input = _read_config(rctx, rctx.attr.config, "base config")
    arch = _platform_arch(rctx)
    config_arch = _config_arch(base_input, "base config")
    if config_arch != arch:
        fail(
            "base config selects %s, but platform %s selects %s" %
            (config_arch, rctx.attr.platform, arch),
        )
    validate_config_features(base_input, "base config")
    tool = _download_generator(rctx)
    base = _resolve_config(
        rctx = rctx,
        tool = tool,
        source_root = source_root,
        arch = arch,
        version = version,
        name = arch,
        raw = base_input,
        config_mode = rctx.attr.config_mode,
    )
    if _config_arch(base, "resolved base config") != arch:
        fail("Kconfig resolution changed the selected Linux architecture")
    validate_config_features(base, "resolved base config")
    _validate_image_compression(base, arch, "resolved base config")

    configs = {
        arch: base,
    }
    base_rust_enabled = base.get("CONFIG_RUST") == "y"
    variant_configs = {}
    variant_graph_images = {}
    variant_rust_enabled = {}
    sanitized_names = {
        _sanitize_target_name(arch): arch,
    }
    for name in sorted(rctx.attr.overlays.keys()):
        _validate_variant_name(name)
        overlay = _read_config(rctx, rctx.attr.overlays[name], "overlay %s" % name)
        _validate_overlay_arch(name, base_input, overlay)
        merged = dict(base_input)
        merged.update(overlay)
        if _config_arch(merged, "overlay %s" % name) != arch:
            fail("overlay %s changes the configured Linux architecture" % name)
        validate_config_features(merged, "overlay %s" % name)
        resolved = _resolve_config(
            rctx = rctx,
            tool = tool,
            source_root = source_root,
            arch = arch,
            version = version,
            name = name,
            raw = merged,
            config_mode = rctx.attr.config_mode,
        )
        if _config_arch(resolved, "resolved overlay %s" % name) != arch:
            fail("Kconfig resolution changed the architecture for overlay %s" % name)
        validate_config_features(resolved, "resolved overlay %s" % name)
        _validate_image_compression(resolved, arch, "resolved overlay %s" % name)
        sanitized = _sanitize_target_name(name)
        if sanitized in sanitized_names:
            fail(
                "overlay names %r and %r produce the same generated target name %r" %
                (sanitized_names[sanitized], name, sanitized),
            )
        sanitized_names[sanitized] = name
        configs[name] = resolved
        variant_configs[name] = "//configs:%s" % name
        if _REPOSITORY_COMPACT_SCHEMA == _CONTENT_COMPACT_SCHEMA:
            variant_graph_images[name] = "//graph:%s_image" % sanitized
        else:
            variant_graph_images[name] = "//graph/%s:%s_image" % (name, sanitized)
        variant_rust_enabled[name] = resolved.get("CONFIG_RUST") == "y"

    rust_profile_json = ""
    if base_rust_enabled or True in variant_rust_enabled.values():
        rust_profile_json = _generate_rust_profile(
            rctx,
            tool,
            source_root,
            _ARCHITECTURES[arch],
        )

    _write_configs(rctx, arch, configs, rules_repo)
    graph_stats = {}
    base_header_family_dependencies = {}
    base_header_family_ids = {}
    variant_core_configs = {}
    variant_header_family_dependencies = {}
    variant_header_family_ids = {}
    variant_header_configs = {}
    if _REPOSITORY_COMPACT_SCHEMA == _CONTENT_COMPACT_SCHEMA:
        config_paths = {
            arch: "configs/base.config",
        }
        generated_headers = {
            arch: "//:_base_%s_generated_headers" % _ARCHITECTURES[arch].arch,
        }
        for name in sorted(variant_configs.keys()):
            config_paths[name] = "configs/%s.config" % name
            generated_headers[name] = "//:_variant_%s_%s_generated_headers" % (
                name,
                _ARCHITECTURES[arch].arch,
            )
        content_graph = _generate_content_graph(
            rctx = rctx,
            tool = tool,
            source = source,
            source_root = source_root,
            arch = arch,
            base_config = arch,
            config_paths = config_paths,
            config_mode = rctx.attr.config_mode,
            generated_headers = generated_headers,
            rules_repo = rules_repo,
            version = version,
        )
        graph_stats = content_graph.stats
        base_header_family_dependencies = content_graph.header_family_dependencies[arch]
        base_header_family_ids = content_graph.header_family_ids[arch]
        variant_header_family_dependencies = {
            name: content_graph.header_family_dependencies[name]
            for name in variant_configs.keys()
        }
        variant_header_family_ids = {
            name: content_graph.header_family_ids[name]
            for name in variant_configs.keys()
        }
        variant_header_configs = content_graph.variant_header_configs
        rust_enabled = dict(variant_rust_enabled)
        rust_enabled[arch] = base_rust_enabled
        core_configs = _content_core_config_aliases(
            content_graph.metadata,
            configs,
            rust_enabled,
            content_graph.header_configs,
            arch,
        )
        variant_core_configs = {
            name: core_configs[name]
            for name in variant_configs.keys()
        }
        graph_image = "//graph:%s_image" % _sanitize_target_name(arch)
    else:
        _generate_config_graph(
            rctx = rctx,
            tool = tool,
            source = source,
            source_root = source_root,
            arch = arch,
            config_name = arch,
            config_path = "configs/base.config",
            config_mode = rctx.attr.config_mode,
            graph_dir = "graph/base",
            source_config = "//:_base_config",
            target_prefix = "_base",
            rules_repo = rules_repo,
        )
        for name in sorted(variant_configs.keys()):
            _generate_config_graph(
                rctx = rctx,
                tool = tool,
                source = source,
                source_root = source_root,
                arch = arch,
                config_name = name,
                config_path = "configs/%s.config" % name,
                config_mode = rctx.attr.config_mode,
                graph_dir = "graph/%s" % name,
                source_config = "//:_variant_%s_config" % name,
                target_prefix = "_variant_%s" % name,
                rules_repo = rules_repo,
            )
        graph_image = "//graph/base:%s_image" % _sanitize_target_name(arch)
    rctx.delete(".linux_bzl_tools")
    rctx.delete(".linux_bzl_resolve")

    source_repo = _repository_prefix(source)
    platform = str(rctx.attr.platform)
    rctx.file(
        "BUILD.bazel",
        _kernel_root_build(
            arch = arch,
            version = version,
            source_repo = source_repo,
            rust_profile_json = rust_profile_json,
            platform = platform,
            base_config = "//configs:%s" % arch,
            base_header_family_dependencies = base_header_family_dependencies,
            base_header_family_ids = base_header_family_ids,
            base_rust_enabled = base_rust_enabled,
            config_mode = rctx.attr.config_mode,
            graph_image = graph_image,
            variant_configs = variant_configs,
            variant_core_configs = variant_core_configs,
            variant_graph_images = variant_graph_images,
            variant_header_family_dependencies = variant_header_family_dependencies,
            variant_header_family_ids = variant_header_family_ids,
            variant_header_configs = variant_header_configs,
            variant_rust_enabled = variant_rust_enabled,
            rules_repo = rules_repo,
        ),
        executable = False,
    )
    for name in sorted(variant_configs.keys()):
        rctx.file(
            "variants/%s/BUILD.bazel" % name,
            _variant_build(
                arch = arch,
                graph = "//:_variant_%s_graph" % name,
                platform = platform,
                rules_repo = rules_repo,
            ),
            executable = False,
        )
    rctx.file(
        ".linux-bzl-generator.json",
        json.encode({
            "architecture": arch,
            "compact_schema": _REPOSITORY_COMPACT_SCHEMA,
            "graph_stats": graph_stats,
            "protocol": _REPOSITORY_GENERATOR_PROTOCOL,
            "rust_enabled": base_rust_enabled,
            "rust_profile_schema": "linux-rust-profile-v1" if rust_profile_json else "",
            "tool_version": KCONFIG_TOOL_VERSION,
            "variant_rust_enabled": variant_rust_enabled,
            "version": version,
        }) + "\n",
        executable = False,
    )
    return rctx.repo_metadata(reproducible = True)

linux_image = repository_rule(
    implementation = _linux_image_impl,
    attrs = {
        "config": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Base Linux Kconfig fragment. Its architecture selection must match platform.",
        ),
        "config_mode": attr.string(
            default = "default",
            values = ["allnoconfig", "default"],
            doc = "Kconfig baseline used while resolving config and overlays.",
        ),
        "overlays": attr.string_keyed_label_dict(
            allow_files = True,
            doc = "Named config overlay fragments.",
        ),
        "platform": attr.label(
            mandatory = True,
            doc = "Supported Hermetic LLVM Linux platform applied once at the public kernel gateway.",
        ),
        "source": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Root Kconfig label from linux_source_repository.",
        ),
        "_self_linux_bzl": attr.label(
            default = Label("//:linux.bzl"),
        ),
        "_llvm_linux_aarch64": attr.label(
            default = Label("@llvm//platforms:linux_aarch64"),
        ),
        "_llvm_linux_arm64": attr.label(
            default = Label("@llvm//platforms:linux_arm64"),
        ),
        "_llvm_linux_x86_64": attr.label(
            default = Label("@llvm//platforms:linux_x86_64"),
        ),
        "_llvm_module": attr.label(
            allow_single_file = True,
            default = Label("@llvm//:MODULE.bazel"),
        ),
    },
    doc = "Generates a config-specific, per-object Bazel Linux kernel graph.",
)

def _platform_arch(rctx):
    platforms = {
        str(rctx.attr._llvm_linux_aarch64): "aarch64",
        str(rctx.attr._llvm_linux_arm64): "aarch64",
        str(rctx.attr._llvm_linux_x86_64): "x86_64",
    }
    arch = platforms.get(str(rctx.attr.platform))
    if arch == None:
        fail(
            "platform must be one of the supported Hermetic LLVM Linux platforms: %s" %
            sorted(platforms.keys()),
        )
    return arch

def _validate_llvm_profile(rctx):
    module = rctx.read(rctx.attr._llvm_module)
    expected = 'LLVM_VERSION = "%s"' % _LLVM_VERSION
    expected_prebuilt = 'PREBUILT_LLVM_VERSION = "%s"' % _LLVM_VERSION
    if module.find(expected) < 0 or module.find(expected_prebuilt) < 0:
        fail(
            "linux.bzl requires Hermetic LLVM %s to match its deterministic Kconfig probe profile" %
            _LLVM_VERSION,
        )

def _read_config(rctx, label, description):
    return _parse_config(rctx.read(label), description)

def _parse_config(content, description):
    values = {}
    for line_number, raw_line in enumerate(content.splitlines()):
        line = raw_line.strip()
        if not line:
            continue
        if line.startswith("#"):
            prefix = "# CONFIG_"
            suffix = " is not set"
            if line.startswith(prefix) and line.endswith(suffix):
                key = line[2:-len(suffix)].strip()
                _set_config_value(values, key, "n", description, line_number + 1)
            continue
        separator = line.find("=")
        if separator < 0:
            fail("%s line %d: expected CONFIG_* assignment" % (description, line_number + 1))
        key = line[:separator].strip()
        value = line[separator + 1:].strip()
        _set_config_value(values, key, value, description, line_number + 1)
    return values

def _resolve_config(rctx, tool, source_root, arch, version, name, raw, config_mode):
    descriptor = _ARCHITECTURES[arch]
    directory = ".linux_bzl_resolve/" + name
    input_path = directory + "/input.config"
    config_path = directory + "/resolved.config"
    auto_conf_path = directory + "/auto.conf"
    auto_conf_cmd_path = directory + "/auto.conf.cmd"
    autoconf_path = directory + "/autoconf.h"
    rustc_cfg_path = directory + "/rustc_cfg"
    kernel_release_path = directory + "/kernel.release"
    rctx.file(input_path, _render_config(raw), executable = False)
    for path in [
        config_path,
        auto_conf_path,
        auto_conf_cmd_path,
        autoconf_path,
        rustc_cfg_path,
        kernel_release_path,
    ]:
        rctx.file(path, "", executable = False)

    args = [
        str(tool),
        "-root",
        str(source_root.get_child("Kconfig")),
        "-srctree",
        str(source_root),
        "-resolve_config",
        "%s=%s" % (name, rctx.path(input_path)),
        "-config_mode",
        config_mode,
        "-resolved_config_out",
        str(rctx.path(config_path)),
        "-resolved_auto_conf_out",
        str(rctx.path(auto_conf_path)),
        "-resolved_auto_conf_cmd_out",
        str(rctx.path(auto_conf_cmd_path)),
        "-resolved_autoconf_out",
        str(rctx.path(autoconf_path)),
        "-resolved_rustc_cfg_out",
        str(rctx.path(rustc_cfg_path)),
        "-resolved_kernel_release_out",
        str(rctx.path(kernel_release_path)),
        "-kernel_version",
        version,
        "-allow_shell",
        "-linux_probe_model",
        "linux_llvm",
    ]
    _add_generator_variables(args, descriptor, source_root)
    result = rctx.execute(
        args,
        environment = {
            "LANG": "C",
            "LC_ALL": "C",
            "TZ": "UTC",
        },
        quiet = False,
        timeout = 1200,
    )
    if result.return_code != 0:
        fail(
            "Linux config resolution failed for %s config %s\nstdout:\n%s\nstderr:\n%s" %
            (rctx.original_name, name, result.stdout, result.stderr),
        )

    resolved = _parse_config(rctx.read(config_path), "resolved config %s" % name)

    # kconfig_parse omits disabled values from its materialized .config.
    # Preserve deliberate unsets so resolving the checked-in generated target
    # cannot re-enable a default-y symbol.
    for key, value in raw.items():
        if value == "n":
            resolved[key] = value
    return resolved

def _set_config_value(values, key, value, description, line_number):
    if not key.startswith("CONFIG_") or len(key) == len("CONFIG_"):
        fail("%s line %d: invalid config key %r" % (description, line_number, key))
    if key in values:
        fail("%s line %d: duplicate config key %s" % (description, line_number, key))
    values[key] = value

def _config_arch(config, description):
    selected = [
        arch
        for symbol, arch in _ARCH_CONFIGS.items()
        if config.get(symbol, "n") == "y"
    ]
    if len(selected) != 1:
        fail(
            "%s must select exactly one supported architecture with CONFIG_X86_64=y or CONFIG_ARM64=y; got %s" %
            (description, selected),
        )
    return selected[0]

def _validate_overlay_arch(name, base, overlay):
    for symbol in sorted(_ARCH_CONFIGS.keys()):
        if symbol in overlay and overlay[symbol] != base.get(symbol, "n"):
            fail(
                "overlay %s cannot change %s from %s to %s" %
                (name, symbol, base.get(symbol, "n"), overlay[symbol]),
            )

def _validate_image_compression(config, arch, description):
    if arch != "x86_64":
        return
    selected = [
        symbol
        for symbol in [
            "CONFIG_KERNEL_GZIP",
            "CONFIG_KERNEL_LZ4",
        ]
        if config.get(symbol, "n") == "y"
    ]
    if len(selected) != 1:
        fail(
            (
                "%s must select exactly one supported x86 kernel compression mode: " +
                "CONFIG_KERNEL_GZIP=y or CONFIG_KERNEL_LZ4=y; got %s"
            ) %
            (description, selected),
        )

def _validate_variant_name(name):
    if not name or name == "base":
        fail("invalid overlay name %r" % name)
    if name in [
        "aux",
        "con",
        "nul",
        "prn",
    ] or (
        len(name) == 4 and
        name[:3] in ["com", "lpt"] and
        name[3] >= "1" and
        name[3] <= "9"
    ):
        fail("overlay name %r is reserved on Windows" % name)
    for index in range(len(name)):
        char = name[index]
        if not (
            (char >= "a" and char <= "z") or
            (char >= "0" and char <= "9") or
            char in ["_", "-"]
        ):
            fail(
                "overlay name %r contains invalid character %r; use lowercase ASCII letters, digits, '_' or '-'" %
                (name, char),
            )

def _sanitize_target_name(name):
    out = ""
    for index in range(len(name)):
        char = name[index]
        if (
            (char >= "a" and char <= "z") or
            (char >= "A" and char <= "Z") or
            (char >= "0" and char <= "9")
        ):
            out += char
        else:
            out += "_"
    return out or "unnamed"

def _render_config(config):
    return "\n".join([
        "%s=%s" % (key, config[key])
        for key in sorted(config.keys())
    ]) + "\n"

def _write_configs(rctx, arch, configs, rules_repo):
    rules = [
        'load("%s//internal:kconfig.bzl", "kconfig_file")' % rules_repo,
        "",
        'package(default_visibility = ["//:__subpackages__"])',
        "",
    ]
    for name in sorted(configs.keys()):
        path = "configs/%s.config" % ("base" if name == arch else name)
        rctx.file(path, _render_config(configs[name]), executable = False)
        rules.extend([
            "kconfig_file(",
            "    name = %r," % name,
            "    config = %r," % (":base.config" if name == arch else ":%s.config" % name),
            "    config_flags = %s," % _starlark_dict(configs[name], indent = "        "),
            ")",
            "",
        ])
    rctx.file("configs/BUILD.bazel", "\n".join(rules), executable = False)

def _initialize_generator_outputs(rctx, graph_dir):
    rctx.file(graph_dir + "/BUILD.bazel", "", executable = False)
    rctx.file(graph_dir + "/metadata.json", "{}\n", executable = False)

def _host_platform(rctx):
    os_name = rctx.os.name.lower()
    if os_name.startswith("linux"):
        os = "linux"
    elif os_name.startswith("mac os"):
        os = "darwin"
    elif "windows" in os_name:
        os = "windows"
    else:
        fail("no linux.bzl graph generator for host operating system %r" % rctx.os.name)

    arch = {
        "aarch64": "arm64",
        "amd64": "amd64",
        "arm64": "arm64",
        "x86_64": "amd64",
    }.get(rctx.os.arch.lower())
    if arch == None:
        fail("no linux.bzl graph generator for host architecture %r" % rctx.os.arch)
    return "%s_%s" % (os, arch)

def _download_generator(rctx):
    platform = _host_platform(rctx)
    release = KCONFIG_TOOL_RELEASES.get(platform)
    if release == None or not release.integrity:
        fail(
            "no integrity-pinned linux.bzl graph generator for host platform %s at %s" %
            (platform, KCONFIG_TOOL_VERSION),
        )
    rctx.download_and_extract(
        url = release.urls,
        integrity = release.integrity,
        output = ".linux_bzl_tools",
        canonical_id = "linux.bzl-generator-%s-%s" % (KCONFIG_TOOL_VERSION, platform),
    )
    tool = rctx.path(".linux_bzl_tools").get_child(kconfig_tool_filename(platform, "kconfig_parse"))
    if not tool.exists:
        fail("linux.bzl generator archive for %s does not contain kconfig_parse" % platform)
    return tool

def _generate_rust_profile(rctx, tool, source_root, descriptor):
    output = ".linux_bzl_rust_profile.json"
    rctx.file(output, "", executable = False)
    args = [
        str(tool),
        "-compact_schema=v0.0.12",
        "-rust_profile_out",
        str(rctx.path(output)),
        "-srctree",
        str(source_root),
        "-var",
        "ARCH=" + descriptor.arch,
    ]
    result = rctx.execute(
        args,
        environment = {
            "LANG": "C",
            "LC_ALL": "C",
            "TZ": "UTC",
        },
        quiet = False,
        timeout = 1200,
    )
    if result.return_code != 0:
        fail(
            "Linux Rust profile generation failed for %s\nstdout:\n%s\nstderr:\n%s" %
            (rctx.original_name, result.stdout, result.stderr),
        )
    content = rctx.read(output)
    profile = json.decode(content)
    if (
        type(profile) != "dict" or
        profile.get("schema") != "linux-rust-profile-v1" or
        profile.get("architecture") != "x86_64"
    ):
        fail("Linux graph generator emitted an invalid Rust profile")
    rctx.delete(output)
    return content

def _generate_config_graph(
        rctx,
        tool,
        source,
        source_root,
        arch,
        config_name,
        config_path,
        config_mode,
        graph_dir,
        source_config,
        target_prefix,
        rules_repo):
    _initialize_generator_outputs(rctx, graph_dir)
    descriptor = _ARCHITECTURES[arch]
    source_package = str(source).rsplit(":", 1)[0]
    source_repo = _repository_prefix(source)
    args = [
        str(tool),
        "-compact_schema=v0.0.12",
        "-root",
        str(source_root.get_child("Kconfig")),
        "-srctree",
        str(source_root),
        "-kbuild",
        str(source_root.get_child("Kbuild")),
        "-compact_kbuild_tree",
        "-compact_buildfile_out",
        str(rctx.path(graph_dir + "/BUILD.bazel")),
        "-compact_metadata_out",
        str(rctx.path(graph_dir + "/metadata.json")),
        "-compact_buildfile_export",
        "metadata.json",
        "-linux_objects_load",
        rules_repo + "//internal:linux_objects.bzl",
        "-object_label_package",
        "//" + graph_dir,
        "-source_label_package",
        source_package,
        "-source_root_label",
        str(source),
        "-source_tree_arch_headers_label",
        source_repo + "//:arch_headers",
        "-source_tree_dtb_sources_label",
        source_repo + "//:dtb_sources",
        "-source_tree_global_headers_label",
        source_repo + "//:global_headers",
        "-source_tree_headers_label",
        source_repo + "//:headers",
        "-source_tree_kbuild_files_label",
        source_repo + "//:kbuild_files",
        "-source_tree_local_include_files_label",
        source_repo + "//:local_include_files",
        "-source_tree_lookup_files_label",
        source_repo + "//:source_tree_lookup_files",
        "-source_tree_scripts_headers_label",
        source_repo + "//:scripts_headers",
        "-source_tree_uapi_headers_label",
        source_repo + "//:uapi_headers",
        "-generated_headers",
        "//:%s_%s_generated_headers" % (target_prefix, descriptor.arch),
        "-source_config",
        source_config,
        "-allow_shell",
        "-linux_probe_model",
        "linux_llvm",
        "-visibility",
        "//:__subpackages__",
    ]
    args.extend(_graph_host_tool_args(arch))
    args.extend(_graph_config_args(
        config_name,
        rctx.path(config_path),
        config_mode,
    ))

    _add_generator_variables(args, descriptor, source_root)

    result = rctx.execute(
        args,
        environment = {
            "LANG": "C",
            "LC_ALL": "C",
            "TZ": "UTC",
        },
        quiet = False,
        timeout = 1200,
    )
    if result.return_code != 0:
        fail(
            "Linux graph generation failed for %s config %s\nstdout:\n%s\nstderr:\n%s" %
            (rctx.original_name, config_name, result.stdout, result.stderr),
        )
    _validate_generated_metadata(rctx, graph_dir, config_name, source_root)
    _validate_generated_build(rctx, graph_dir, config_name)

def _generate_content_graph(
        rctx,
        tool,
        source,
        source_root,
        arch,
        base_config,
        config_paths,
        config_mode,
        generated_headers,
        rules_repo,
        version):
    graph_dir = "graph"
    _initialize_generator_outputs(rctx, graph_dir)
    descriptor = _ARCHITECTURES[arch]
    compile_environment_abi = "linux.bzl/compact-v5/llvm-%s/%s/%s" % (
        _LLVM_VERSION,
        descriptor.arch,
        descriptor.srcarch,
    )
    source_package = str(source).rsplit(":", 1)[0]
    source_repo = _repository_prefix(source)
    args = [
        str(tool),
        "-compact_schema=" + _CONTENT_COMPACT_SCHEMA,
        "-compact_base_config",
        base_config,
        "-compile_environment_abi",
        compile_environment_abi,
        "-kernel_version",
        version,
        "-root",
        str(source_root.get_child("Kconfig")),
        "-srctree",
        str(source_root),
        "-kbuild",
        str(source_root.get_child("Kbuild")),
        "-compact_kbuild_tree",
        "-compact_buildfile_out",
        str(rctx.path(graph_dir + "/BUILD.bazel")),
        "-compact_metadata_out",
        str(rctx.path(graph_dir + "/metadata.json")),
        "-compact_buildfile_export",
        "metadata.json",
        "-linux_objects_load",
        rules_repo + "//internal:linux_objects.bzl",
        "-object_label_package",
        "//" + graph_dir,
        "-source_label_package",
        source_package,
        "-source_root_label",
        str(source),
        "-source_tree_arch_headers_label",
        source_repo + "//:arch_headers",
        "-source_tree_dtb_sources_label",
        source_repo + "//:dtb_sources",
        "-source_tree_global_headers_label",
        source_repo + "//:global_headers",
        "-source_tree_headers_label",
        source_repo + "//:headers",
        "-source_tree_kbuild_files_label",
        source_repo + "//:kbuild_files",
        "-source_tree_local_include_files_label",
        source_repo + "//:local_include_files",
        "-source_tree_lookup_files_label",
        source_repo + "//:source_tree_lookup_files",
        "-source_tree_scripts_headers_label",
        source_repo + "//:scripts_headers",
        "-source_tree_uapi_headers_label",
        source_repo + "//:uapi_headers",
        "-source_asn1_compiler",
        "//:_base_asn1_compiler_tool",
        "-allow_shell",
        "-linux_probe_model",
        "linux_llvm",
        "-visibility",
        "//:__subpackages__",
    ]
    if arch == "aarch64":
        args.extend([
            "-source_relacheck",
            "//:_base_relacheck_tool",
        ])
    args.extend(_graph_configs_args({
        name: rctx.path(config_paths[name])
        for name in config_paths.keys()
    }, config_mode))
    for name in sorted(generated_headers.keys()):
        args.extend([
            "-generated_headers_for_config",
            "%s=%s" % (name, generated_headers[name]),
        ])
    _add_generator_variables(args, descriptor, source_root)

    result = rctx.execute(
        args,
        environment = {
            "LANG": "C",
            "LC_ALL": "C",
            "TZ": "UTC",
        },
        quiet = False,
        timeout = 1200,
    )
    if result.return_code != 0:
        fail(
            "Linux content graph generation failed for %s configs %s\nstdout:\n%s\nstderr:\n%s" %
            (rctx.original_name, sorted(config_paths.keys()), result.stdout, result.stderr),
        )
    validated = _validate_generated_metadata(
        rctx,
        graph_dir,
        sorted(config_paths.keys()),
        source_root,
        schema = _CONTENT_COMPACT_SCHEMA,
        expected_compile_environment_abi = compile_environment_abi,
        expected_srcarch = descriptor.srcarch,
    )
    _validate_generated_build(
        rctx,
        graph_dir,
        "content-addressed configs",
        schema = _CONTENT_COMPACT_SCHEMA,
    )
    metadata = validated.metadata
    header_index = _content_generated_header_config_index(
        metadata,
        generated_headers,
        base_config,
    )
    return struct(
        header_configs = header_index.aliases,
        header_family_dependencies = header_index.family_dependencies,
        header_family_ids = header_index.family_ids,
        metadata = metadata,
        stats = validated.stats,
        variant_header_configs = {
            name: header_index.aliases[name]
            for name in header_index.aliases.keys()
            if name != base_config
        },
    )

def _graph_config_args(config_name, config_path, config_mode):
    return [
        "-config",
        "%s=%s" % (config_name, config_path),
        "-config_mode",
        config_mode,
    ]

def _graph_host_tool_args(arch):
    args = [
        "-source_asn1_compiler",
        "//:_base_asn1_compiler_tool",
    ]
    if arch == "aarch64":
        args.extend([
            "-source_relacheck",
            "//:_base_relacheck_tool",
        ])
    return args

def _graph_configs_args(config_paths, config_mode):
    args = []
    for name in sorted(config_paths.keys()):
        args.extend([
            "-config",
            "%s=%s" % (name, config_paths[name]),
        ])
    args.extend([
        "-config_mode",
        config_mode,
    ])
    return args

def _content_generated_header_config_index(metadata, generated_headers, base_config):
    if base_config not in generated_headers:
        fail("Linux content graph base generated-header config %r is absent" % base_config)
    config_by_label = {}
    for name, label in generated_headers.items():
        if label in config_by_label:
            fail(
                "Linux content graph configs %s and %s use the same generated-header label %s" %
                (config_by_label[label], name, label),
            )
        config_by_label[label] = name

    family_ids = {
        name: {}
        for name in generated_headers.keys()
    }
    family_by_id = {}
    for family in metadata.get("generated_header_families", []):
        labels = family.get("labels", []) if type(family) == "dict" else []
        family_name = family.get("name", "") if type(family) == "dict" else ""
        content_id = family.get("id", "") if type(family) == "dict" else ""
        dependencies = family.get("dependencies", []) if type(family) == "dict" else []
        if not _is_content_id(content_id):
            fail("Linux content graph generated-header family has invalid content ID %r" % content_id)
        if content_id in family_by_id:
            fail("Linux content graph repeats generated-header family content ID %s" % content_id)
        if type(family_name) != "string" or not family_name:
            fail("Linux content graph generated-header family %s has invalid name %r" % (content_id, family_name))
        if type(dependencies) != "list":
            fail("Linux content graph generated-header family %s has invalid dependencies" % content_id)
        family_by_id[content_id] = struct(
            dependencies = list(dependencies),
            name = family_name,
        )
        for label in labels:
            if label not in config_by_label:
                fail("Linux content graph generated-header family references unknown label %s" % label)
            config_name = config_by_label[label]
            if family_name in family_ids[config_name]:
                fail(
                    "Linux content graph repeats generated-header family %s for config %s" %
                    (family_name, config_name),
                )
            family_ids[config_name][family_name] = content_id

    for name in sorted(family_ids.keys()):
        if not family_ids[name]:
            fail("Linux content graph has no generated-header families for config %s" % name)
    expected_family_names = sorted(family_ids[base_config].keys())
    if "all" not in family_ids[base_config]:
        fail("Linux content graph generated-header config %s has no all family" % base_config)
    for name in sorted(family_ids.keys()):
        if sorted(family_ids[name].keys()) != expected_family_names:
            fail(
                "Linux content graph generated-header config %s has families %s, expected %s" %
                (name, sorted(family_ids[name].keys()), expected_family_names),
            )

    family_dependencies = {}
    for config_name in sorted(family_ids.keys()):
        selected_ids = family_ids[config_name]
        config_dependencies = {}
        for family_name in expected_family_names:
            family_id = selected_ids[family_name]
            dependencies = {}
            for dependency_id in family_by_id[family_id].dependencies:
                if not _is_content_id(dependency_id) or dependency_id not in family_by_id:
                    fail(
                        "Linux content graph generated-header family %s references unknown dependency %s" %
                        (family_id, dependency_id),
                    )
                dependency_name = family_by_id[dependency_id].name
                if dependency_name in dependencies:
                    fail(
                        "Linux content graph generated-header family %s repeats dependency family %s" %
                        (family_id, dependency_name),
                    )
                if selected_ids.get(dependency_name) != dependency_id:
                    fail(
                        "Linux content graph generated-header family %s config %s dependency %s does not match selected family ID %s" %
                        (family_id, config_name, dependency_name, selected_ids.get(dependency_name)),
                    )
                dependencies[dependency_name] = dependency_id
            config_dependencies[family_name] = dependencies
        family_dependencies[config_name] = config_dependencies

    ordered_names = [base_config] + [
        name
        for name in sorted(generated_headers.keys())
        if name != base_config
    ]
    aliases = {}
    canonical_names = []
    for name in ordered_names:
        canonical = name
        for candidate in canonical_names:
            if family_ids[name] == family_ids[candidate]:
                canonical = candidate
                break
        aliases[name] = canonical
        if canonical == name:
            canonical_names.append(name)
    return struct(
        aliases = aliases,
        family_dependencies = family_dependencies,
        family_ids = family_ids,
    )

def _config_without_image_compression(config):
    return {
        key: config[key]
        for key in config.keys()
        if key not in _IMAGE_COMPRESSION_CONFIGS
    }

def _content_core_config_aliases(metadata, configs, rust_enabled, header_configs, base_config):
    expected_names = sorted(configs.keys())
    if base_config not in configs:
        fail("Linux content graph base config %r is absent from resolved configs" % base_config)
    if sorted(rust_enabled.keys()) != expected_names:
        fail(
            "Linux content graph Rust configs %s do not match resolved configs %s" %
            (sorted(rust_enabled.keys()), expected_names),
        )
    if sorted(header_configs.keys()) != expected_names:
        fail(
            "Linux content graph header configs %s do not match resolved configs %s" %
            (sorted(header_configs.keys()), expected_names),
        )

    graph_configs = {}
    for config in metadata.get("configs", []):
        if type(config) != "dict":
            fail("Linux content graph emitted an invalid config while deriving core outputs")
        name = config.get("name", "")
        object_targets = config.get("object_targets", [])
        module_object_targets = config.get("module_object_targets", [])
        if type(name) != "string" or not name:
            fail("Linux content graph emitted an unnamed config while deriving core outputs")
        if name in graph_configs:
            fail("Linux content graph repeated config %r while deriving core outputs" % name)
        if type(object_targets) != "list" or type(module_object_targets) != "list":
            fail("Linux content graph config %s has invalid object roots" % name)
        graph_configs[name] = struct(
            module_object_targets = list(module_object_targets),
            object_targets = list(object_targets),
        )
    if sorted(graph_configs.keys()) != expected_names:
        fail(
            "Linux content graph configs %s do not match resolved configs %s while deriving core outputs" %
            (sorted(graph_configs.keys()), expected_names),
        )

    ordered_names = [base_config] + [
        name
        for name in expected_names
        if name != base_config
    ]
    aliases = {}
    canonical_names = []
    for name in ordered_names:
        graph = graph_configs[name]
        config = _config_without_image_compression(configs[name])
        canonical = name
        for candidate in canonical_names:
            candidate_graph = graph_configs[candidate]
            if (
                graph.object_targets == candidate_graph.object_targets and
                graph.module_object_targets == candidate_graph.module_object_targets and
                config == _config_without_image_compression(configs[candidate]) and
                rust_enabled[name] == rust_enabled[candidate] and
                header_configs[name] == header_configs[candidate]
            ):
                canonical = candidate
                break
        aliases[name] = canonical
        if canonical == name:
            canonical_names.append(name)
    return aliases

def _generator_variable_args(variables, source_root):
    variables = dict(variables)
    variables["srctree"] = str(source_root).replace("\\", "/")
    result = []
    for key in sorted(variables.keys()):
        result.extend(["-var", "%s=%s" % (key, variables[key])])
    return result

def _add_generator_variables(args, descriptor, source_root):
    variables = dict(descriptor.compact_vars)
    variables.update({
        "ARCH": descriptor.arch,
        "SRCARCH": descriptor.srcarch,
        "UTS_MACHINE": descriptor.uts_machine,
    })
    args.extend(_generator_variable_args(variables, source_root))
    probe_env = dict(_PROBE_ENV)
    probe_env["ARCH"] = descriptor.arch
    probe_env["SRCARCH"] = descriptor.srcarch
    for key in sorted(probe_env.keys()):
        args.extend(["-env", "%s=%s" % (key, probe_env[key])])
    for key in sorted(_PROBE_VALUES.keys()):
        args.extend(["-linux_probe_value", "%s=%s" % (key, _PROBE_VALUES[key])])

def _metadata_positive_decimal(value, context):
    if not value or (len(value) > 1 and value.startswith("0")):
        fail("%s has invalid positive decimal %r" % (context, value))
    result = 0
    for i in range(len(value)):
        char = value[i]
        if char < "0" or char > "9":
            fail("%s has invalid positive decimal %r" % (context, value))
        result = result * 10 + int(char)
    if result <= 0:
        fail("%s has invalid positive decimal %r" % (context, value))
    return result

def _metadata_source_input_index(metadata):
    files = metadata.get("source_files", [])
    groups = metadata.get("source_input_groups", [])
    if type(files) != "list" or not files or type(groups) != "list" or not groups:
        fail("Linux content graph requires non-empty source_files and source_input_groups")
    paths = []
    previous_path = ""
    for index in range(len(files)):
        source_file = files[index]
        path = source_file.get("path", "") if type(source_file) == "dict" else ""
        digest = source_file.get("digest", "") if type(source_file) == "dict" else ""
        if (
            type(path) != "string" or
            not path or
            not _is_content_id(digest) or
            (previous_path and previous_path >= path)
        ):
            fail("Linux content graph source file %d is invalid or non-canonical" % (index + 1))
        paths.append(path)
        previous_path = path
    decoded_groups = []
    previous_group = ""
    for group_index in range(len(groups)):
        encoded = groups[group_index]
        if type(encoded) != "string" or not encoded or (previous_group and previous_group >= encoded):
            fail("Linux content graph source input group %d is invalid or non-canonical" % (group_index + 1))
        group_paths = {}
        previous_file = 0
        for value in encoded.split(","):
            file_index = _metadata_positive_decimal(
                value,
                "Linux content graph source input group %d" % (group_index + 1),
            )
            if file_index <= previous_file or file_index > len(paths):
                fail(
                    "Linux content graph source input group %d has duplicate or out-of-range file index %d" %
                    (group_index + 1, file_index),
                )
            group_paths[paths[file_index - 1]] = True
            previous_file = file_index
        decoded_groups.append(group_paths)
        previous_group = encoded
    return struct(
        files = files,
        groups = decoded_groups,
    )

def _metadata_source_input_group(source_index, group, context):
    if type(group) != "int" or group <= 0 or group > len(source_index.groups):
        fail(
            "%s source_input_group %r is out of range 1..%d" %
            (context, group, len(source_index.groups)),
        )
    return source_index.groups[group - 1]

def _validate_generated_metadata(
        rctx,
        graph_dir,
        config_names,
        source_root,
        schema = "v0.0.12",
        expected_compile_environment_abi = "",
        expected_srcarch = ""):
    metadata = json.decode(rctx.read(graph_dir + "/metadata.json"))
    if type(metadata) != "dict":
        fail("Linux graph generator wrote invalid metadata")
    if type(config_names) == "string":
        config_names = [config_names]
    generated_configs = metadata.get("configs", [])
    names = sorted([config.get("name", "") for config in generated_configs])
    expected_names = sorted(config_names)
    if names != expected_names:
        fail("Linux graph generator emitted configs %s, expected %s" % (names, expected_names))
    source_index = None
    if schema == _CONTENT_COMPACT_SCHEMA:
        if metadata.get("schema") != _CONTENT_COMPACT_SCHEMA:
            fail("Linux content graph generator emitted schema %r" % metadata.get("schema"))
        source_index = _metadata_source_input_index(metadata)

    variants = metadata.get("object_variants", [])
    if type(variants) != "list" or not variants:
        fail("Linux graph generator produced no object variants")
    variants_by_target = {}
    content_ids = {}
    for variant in variants:
        if type(variant) != "dict":
            fail("Linux graph generator emitted invalid object metadata")
        target = variant.get("target", "")
        object_path = variant.get("object", "")
        mode = variant.get("mode", "")
        if type(target) != "string" or not target or type(object_path) != "string" or not object_path:
            fail("Linux graph generator emitted an unnamed object variant")
        if mode not in ["y", "m"]:
            fail(
                "Linux graph for configs %s selects object %s with invalid Kbuild mode %r" %
                (expected_names, object_path, mode),
            )
        if target in variants_by_target:
            fail("Linux graph generator repeated object target %s" % target)
        variants_by_target[target] = variant
        if schema == _CONTENT_COMPACT_SCHEMA and "config_fragment" in variant:
            fail("Linux content graph object %s retains redundant config_fragment" % object_path)
        if variant.get("members", []):
            continue
        source = variant.get("source", "")
        if type(source) != "string" or not source:
            fail(
                "Linux graph for configs %s cannot resolve a concrete source for leaf object %s" %
                (expected_names, object_path),
            )
        if not source_root.get_child(source).exists:
            fail(
                "Linux graph for configs %s resolved object %s to missing source %s" %
                (expected_names, object_path, source),
            )
        if schema == _CONTENT_COMPACT_SCHEMA:
            if variant.get("source_inputs", []):
                fail("Linux content graph object %s retains inline source_inputs" % object_path)
            source_paths = _metadata_source_input_group(
                source_index,
                variant.get("source_input_group", 0),
                "Linux content graph object %s" % object_path,
            )
            if source not in source_paths:
                fail("Linux content graph object %s exact inputs omit %s" % (object_path, source))

    if schema != _CONTENT_COMPACT_SCHEMA:
        return struct(
            metadata = metadata,
            stats = {
                "config_count": len(generated_configs),
                "object_definitions": len(variants),
            },
        )
    if not expected_compile_environment_abi:
        fail("Linux content graph validation requires an expected compile environment ABI")

    payload_ids = {}
    for payload in metadata.get("config_payloads", []):
        payload_id = payload.get("id", "") if type(payload) == "dict" else ""
        if not _is_content_id(payload_id) or payload_id in payload_ids:
            fail("Linux content graph has invalid or duplicate config payload ID %r" % payload_id)
        if "fragment" in payload or type(payload.get("content")) != "string":
            fail("Linux content graph config payload %s is not normalized" % payload_id)
        payload_ids[payload_id] = True

    family_by_id = {}
    for family in metadata.get("generated_header_families", []):
        family_id = family.get("id", "") if type(family) == "dict" else ""
        name = family.get("name", "") if type(family) == "dict" else ""
        payload_id = family.get("config_payload", "") if type(family) == "dict" else ""
        labels = family.get("labels", []) if type(family) == "dict" else []
        srcarch = family.get("srcarch", "") if type(family) == "dict" else ""
        dependencies = family.get("dependencies", []) if type(family) == "dict" else []
        source_input_group = family.get("source_input_group", 0) if type(family) == "dict" else 0
        source_inputs = family.get("source_inputs", []) if type(family) == "dict" else []
        if not _is_content_id(family_id) or family_id in family_by_id:
            fail("Linux content graph has invalid or duplicate generated-header family ID %r" % family_id)
        if (
            type(name) != "string" or
            not name or
            payload_id not in payload_ids or
            type(labels) != "list" or
            not labels or
            type(srcarch) != "string" or
            not srcarch or
            (expected_srcarch and srcarch != expected_srcarch) or
            type(dependencies) != "list" or
            type(source_input_group) != "int" or
            source_input_group < 0 or
            type(source_inputs) != "list" or
            source_inputs
        ):
            fail("Linux content graph generated-header family %s is invalid" % family_id)
        for label in labels:
            if type(label) != "string" or not label:
                fail("Linux content graph generated-header family %s has invalid label %r" % (family_id, label))
        if source_input_group:
            _metadata_source_input_group(
                source_index,
                source_input_group,
                "Linux content graph generated-header family %s" % family_id,
            )
        family_by_id[family_id] = family

    for family_id, family in family_by_id.items():
        seen_dependencies = {}
        for dependency_id in family.get("dependencies", []):
            if type(dependency_id) != "string" or dependency_id not in family_by_id:
                fail(
                    "Linux content graph generated-header family %s references unknown dependency %s" %
                    (family_id, dependency_id),
                )
            if dependency_id == family_id or dependency_id in seen_dependencies:
                fail(
                    "Linux content graph generated-header family %s has duplicate or self dependency %s" %
                    (family_id, dependency_id),
                )
            seen_dependencies[dependency_id] = True
    resolved_family_ids = {}
    for _ in range(len(family_by_id)):
        for family_id, family in family_by_id.items():
            if family_id in resolved_family_ids:
                continue
            if all([
                dependency_id in resolved_family_ids
                for dependency_id in family.get("dependencies", [])
            ]):
                resolved_family_ids[family_id] = True
    if len(resolved_family_ids) != len(family_by_id):
        fail(
            "Linux content graph generated-header families contain a dependency cycle involving %s" %
            sorted([
                family_id
                for family_id in family_by_id.keys()
                if family_id not in resolved_family_ids
            ]),
        )

    environment_ids = {}
    for environment in metadata.get("compile_environments", []):
        environment_id = environment.get("id", "") if type(environment) == "dict" else ""
        payload_id = environment.get("config_payload", "") if type(environment) == "dict" else ""
        abi = environment.get("abi", "") if type(environment) == "dict" else ""
        family_ids = environment.get("generated_header_families", []) if type(environment) == "dict" else []
        if not _is_content_id(environment_id) or environment_id in environment_ids:
            fail("Linux content graph has invalid or duplicate compile environment ID %r" % environment_id)
        if payload_id not in payload_ids or type(abi) != "string" or not abi or type(family_ids) != "list":
            fail("Linux content graph compile environment %s is invalid" % environment_id)
        _validate_compile_environment_abi(
            abi,
            expected_compile_environment_abi,
            environment_id,
        )
        family_names = {}
        for family_id in family_ids:
            if type(family_id) != "string" or family_id not in family_by_id:
                fail(
                    "Linux content graph compile environment %s references unknown generated-header family %s" %
                    (environment_id, family_id),
                )
            family_name = family_by_id[family_id]["name"]
            if family_name in family_names:
                fail(
                    "Linux content graph compile environment %s repeats generated-header family %s" %
                    (environment_id, family_name),
                )
            family_names[family_name] = True
        if "all" in family_names and len(family_names) != 1:
            fail("Linux content graph compile environment %s mixes all with precise generated-header families" % environment_id)
        environment_ids[environment_id] = True

    for variant in variants:
        target = variant["target"]
        content_id = variant.get("content_id", "")
        if not _is_content_id(content_id):
            fail("Linux content graph target %s has invalid content ID %r" % (target, content_id))
        if content_id in content_ids:
            fail("Linux content graph targets %s and %s duplicate content ID %s" % (content_ids[content_id], target, content_id))
        content_ids[content_id] = target
        if not target.endswith("__" + content_id[:24]):
            fail("Linux content graph target %s does not use its collision-checked content ID" % target)
        for dependency in variant.get("deps", []) + variant.get("members", []):
            if dependency not in variants_by_target:
                fail("Linux content graph target %s references unknown object target %s" % (target, dependency))
        is_nvhe = variant.get("object", "") == "arch/arm64/kvm/hyp/nvhe/kvm_nvhe.o"
        if variant.get("source", "") or is_nvhe:
            environment_id = variant.get("compile_environment", "")
            if environment_id not in environment_ids:
                fail("Linux content graph target %s references unknown compile environment %s" % (target, environment_id))
        if is_nvhe:
            source_paths = _metadata_source_input_group(
                source_index,
                variant.get("source_input_group", 0),
                "Linux content graph nVHE object %s" % target,
            )
            if "arch/arm64/kvm/hyp/nvhe/hyp.lds.S" not in source_paths:
                fail("Linux content graph nVHE object %s omits hyp.lds.S" % target)
        elif variant.get("members", []) and variant.get("source_input_group", 0):
            fail("Linux content graph composite object %s unexpectedly has source inputs" % target)

    memberships = 0
    selected_targets = {}
    for config in generated_configs:
        payload_id = config.get("config_payload", "")
        if payload_id not in payload_ids:
            fail("Linux content graph config %s references unknown config payload %s" % (config.get("name", ""), payload_id))
        pending = config.get("object_targets", []) + config.get("module_object_targets", [])
        selected_for_config = {}
        for _ in variants:
            next_pending = []
            for target in pending:
                if target not in variants_by_target:
                    fail("Linux content graph config %s references unknown object target %s" % (config.get("name", ""), target))
                if target in selected_for_config:
                    continue
                selected_for_config[target] = True
                selected_targets[target] = True
                variant = variants_by_target[target]
                next_pending.extend(variant.get("deps", []))
                next_pending.extend(variant.get("members", []))
            pending = next_pending
            if not pending:
                break
        memberships += len(selected_for_config)
    duplicate_memberships = memberships - len(selected_targets)
    if duplicate_memberships < 0:
        duplicate_memberships = 0
    return struct(
        metadata = metadata,
        stats = {
            "compile_environments": len(environment_ids),
            "config_count": len(generated_configs),
            "config_payloads": len(payload_ids),
            "duplicate_memberships": duplicate_memberships,
            "generated_header_families": len(family_by_id),
            "object_definitions": len(variants),
            "object_memberships": memberships,
            "selected_object_variants": len(selected_targets),
        },
    )

def _is_content_id(value):
    if type(value) != "string" or len(value) != 64:
        return False
    for index in range(len(value)):
        if value[index] not in "0123456789abcdef":
            return False
    return True

def _validate_compile_environment_abi(actual, expected, environment_id):
    if actual != expected:
        fail(
            "Linux content graph compile environment %s ABI %r does not match expected ABI %r" %
            (environment_id, actual, expected),
        )

def _generated_object_block_has_buildable_inputs(block, schema):
    block_with_prefix = "\n" + block
    if schema == _CONTENT_COMPACT_SCHEMA:
        return (
            "\n    source_input_file = " in block_with_prefix and
            "\n    source_input_group = " in block_with_prefix and
            "\n    source_input_index = " in block_with_prefix
        )
    return "\n    src = " in block_with_prefix

def _validate_generated_build(
        rctx,
        graph_dir,
        config_name,
        schema = "v0.0.12"):
    content = rctx.read(graph_dir + "/BUILD.bazel")
    unsupported_rules = [
        "linux_dtb",
        "linux_generated_file",
        "linux_install",
        "linux_modpost",
        "linux_module",
    ]
    for rule_name in unsupported_rules:
        if "\n%s(\n" % rule_name in content:
            fail(
                "Linux graph for config %s requires unsupported generated rule %s" %
                (config_name, rule_name),
            )

    blocks = content.split("\nlinux_object(\n")
    for raw_block in blocks[1:]:
        end = raw_block.find("\n)\n")
        if end < 0:
            fail("Linux graph generator emitted a malformed linux_object rule")
        block = raw_block[:end]
        if _generated_object_block_has_buildable_inputs(block, schema):
            continue
        name = "<unknown>"
        name_prefix = '    name = "'
        name_start = block.find(name_prefix)
        if name_start >= 0:
            name_rest = block[name_start + len(name_prefix):]
            name_end = name_rest.find('"')
            if name_end >= 0:
                name = name_rest[:name_end]
        fail(
            (
                "Linux graph for config %s emitted leaf object %s without buildable %s inputs; " +
                "its Kbuild source or flag expressions are not implemented"
            ) %
            (
                config_name,
                name,
                "indexed" if schema == _CONTENT_COMPACT_SCHEMA else "source",
            ),
        )

def _kernel_root_build(
        arch,
        version,
        source_repo,
        rust_profile_json,
        platform,
        base_config,
        base_header_family_dependencies,
        base_header_family_ids,
        base_rust_enabled,
        config_mode,
        graph_image,
        variant_configs,
        variant_graph_images,
        variant_rust_enabled,
        rules_repo,
        variant_core_configs = {},
        variant_header_family_dependencies = {},
        variant_header_family_ids = {},
        variant_header_configs = {}):
    return """load("{rules_repo}//internal:kernel_repository_targets.bzl", "linux_image_targets")

package(default_visibility = ["//visibility:private"])

linux_image_targets(
    name = "_kernel_graph",
    arch = {arch},
    version = {version},
    source_repo = {source_repo},
    rust_profile_json = {rust_profile_json},
    platform = {platform},
    base_config = {base_config},
    base_header_family_dependencies = {base_header_family_dependencies},
    base_header_family_ids = {base_header_family_ids},
    base_rust_enabled = {base_rust_enabled},
    config_mode = {config_mode},
    graph_image = {graph_image},
    variant_configs = {variant_configs},
    variant_core_configs = {variant_core_configs},
    variant_graph_images = {variant_graph_images},
    variant_header_family_dependencies = {variant_header_family_dependencies},
    variant_header_family_ids = {variant_header_family_ids},
    variant_header_configs = {variant_header_configs},
    variant_rust_enabled = {variant_rust_enabled},
)
""".format(
        arch = repr(arch),
        version = repr(version),
        source_repo = repr(source_repo),
        rust_profile_json = repr(rust_profile_json),
        platform = repr(platform),
        base_config = repr(base_config),
        base_header_family_dependencies = _starlark_nested_dict(base_header_family_dependencies, indent = "        "),
        base_header_family_ids = _starlark_dict(base_header_family_ids, indent = "        "),
        base_rust_enabled = repr(base_rust_enabled),
        config_mode = repr(config_mode),
        graph_image = repr(graph_image),
        variant_configs = _starlark_dict(variant_configs, indent = "        "),
        variant_core_configs = _starlark_dict(variant_core_configs, indent = "        "),
        variant_graph_images = _starlark_dict(variant_graph_images, indent = "        "),
        variant_header_family_dependencies = _starlark_triple_nested_dict(variant_header_family_dependencies, indent = "        "),
        variant_header_family_ids = _starlark_nested_dict(variant_header_family_ids, indent = "        "),
        variant_header_configs = _starlark_dict(variant_header_configs, indent = "        "),
        variant_rust_enabled = _starlark_dict(variant_rust_enabled, indent = "        "),
        rules_repo = rules_repo,
    )

repositories_test_helpers = struct(
    validate_compile_environment_abi = _validate_compile_environment_abi,
    content_schema = _CONTENT_COMPACT_SCHEMA,
    core_config_aliases = _content_core_config_aliases,
    generated_object_block_has_buildable_inputs = _generated_object_block_has_buildable_inputs,
    graph_config_args = _graph_config_args,
    graph_configs_args = _graph_configs_args,
    graph_host_tool_args = _graph_host_tool_args,
    generator_variable_args = _generator_variable_args,
    generated_header_config_index = _content_generated_header_config_index,
    generator_protocol = _REPOSITORY_GENERATOR_PROTOCOL,
    kernel_root_build = _kernel_root_build,
)

def _variant_build(arch, graph, platform, rules_repo):
    return """load("{rules_repo}//internal:kernel_bundle.bzl", "linux_kernel_exports")

package(default_visibility = ["//visibility:private"])

linux_kernel_exports(
    name = "kernel",
    graph = {graph},
    platform = {platform},
    arch = {arch},
)
""".format(
        arch = repr(arch),
        graph = repr(graph),
        platform = repr(platform),
        rules_repo = rules_repo,
    )

def _starlark_dict(values, indent = "    "):
    if not values:
        return "{}"
    lines = ["{"]
    for key in sorted(values.keys()):
        lines.append("%s%r: %r," % (indent, key, values[key]))
    lines.append(indent[:-4] + "}")
    return "\n".join(lines)

def _starlark_nested_dict(values, indent = "    "):
    if not values:
        return "{}"
    lines = ["{"]
    for key in sorted(values.keys()):
        lines.append(
            "%s%r: %s," %
            (indent, key, _starlark_dict(values[key], indent = indent + "    ")),
        )
    lines.append(indent[:-4] + "}")
    return "\n".join(lines)

def _starlark_triple_nested_dict(values, indent = "    "):
    if not values:
        return "{}"
    lines = ["{"]
    for key in sorted(values.keys()):
        lines.append(
            "%s%r: %s," %
            (indent, key, _starlark_nested_dict(values[key], indent = indent + "    ")),
        )
    lines.append(indent[:-4] + "}")
    return "\n".join(lines)
