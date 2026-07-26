"""Hermetic repository rules behind the public linux.bzl facade."""

load(":config_validation.bzl", "validate_config_features")
load(":kconfig_tool_filename.bzl", "kconfig_tool_filename")
load(":kconfig_tool_releases.bzl", "KCONFIG_TOOL_RELEASES", "KCONFIG_TOOL_VERSION")

visibility("//...")

_KERNEL_RELEASES = {
    "6.12.96": struct(
        integrity = "sha256-fS4bXVqzazoBhW5xeC2tKlTmNPsrN8CkKZje87v5V8E=",
        strip_prefix = "linux-6.12.96",
        urls = [
            "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.96.tar.xz",
        ],
    ),
    "6.18.39": struct(
        integrity = "sha256-p6fj0q6dledBlyI6jU619r56rCG25t4n6WhdABwfjLA=",
        strip_prefix = "linux-6.18.39",
        urls = [
            "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.39.tar.xz",
        ],
    ),
}

_ARCHITECTURES = {
    "aarch64": struct(
        arch = "arm64",
        compact_vars = {
            "ARCH_CORE": "",
            "ARCH_DRIVERS": "",
            "ARCH_LIB": "arch/arm64/lib/ lib/",
            "BITS": "64",
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

_SOURCE_REPOSITORY_PROTOCOL = "linux-source-v1"
_REPOSITORY_GENERATOR_PROTOCOL = "compact-v2-rust-sdk-state"
_LLVM_VERSION = "22.1.8"

def _linux_source_repository_impl(rctx):
    catalog = _KERNEL_RELEASES.get(rctx.attr.version)
    has_urls = len(rctx.attr.urls) != 0
    has_integrity = rctx.attr.integrity != ""
    if has_urls != has_integrity:
        fail("linux_source_repository %s requires urls and integrity together" % rctx.original_name)

    if has_urls:
        urls = rctx.attr.urls
        integrity = rctx.attr.integrity
        strip_prefix = rctx.attr.strip_prefix or (catalog.strip_prefix if catalog != None else "")
    elif catalog != None:
        urls = catalog.urls
        integrity = catalog.integrity
        strip_prefix = rctx.attr.strip_prefix or catalog.strip_prefix
    else:
        fail(
            "Linux %s is not in the maintained source catalog; set both urls and integrity" %
            rctx.attr.version,
        )
    if not integrity.startswith("sha256-"):
        fail("linux_source_repository integrity must be a sha256 SRI digest")

    if rctx.attr.patch_strip < 0:
        fail("patch_strip must be non-negative")

    rctx.download_and_extract(
        url = urls,
        integrity = integrity,
        strip_prefix = strip_prefix,
        canonical_id = "linux.bzl-source-%s-%s" % (rctx.attr.version, integrity),
    )
    for patch in rctx.attr.patches:
        rctx.patch(patch, strip = rctx.attr.patch_strip)

    makefile = rctx.path("Makefile")
    kconfig = rctx.path("Kconfig")
    if not makefile.exists or not kconfig.exists:
        fail(
            "Linux source archive for %s must contain root Makefile and Kconfig files" %
            rctx.attr.version,
        )
    actual_version = _linux_makefile_version(rctx.read(makefile))
    if actual_version != rctx.attr.version:
        fail(
            "Linux source archive version mismatch: requested %s, Makefile reports %s" %
            (rctx.attr.version, actual_version),
        )

    source_build = rctx.read(rctx.attr._source_build_file)
    source_build = source_build.replace(
        "@rules_cc",
        _repository_prefix(rctx.attr._rules_cc_defs),
    )
    source_build = source_build.replace(
        "@platforms",
        _repository_prefix(rctx.attr._platforms_x86_64),
    )
    rctx.file("BUILD.bazel", source_build, executable = False)
    rctx.file(
        ".linux-bzl-source.json",
        json.encode({
            "integrity": integrity,
            "protocol": _SOURCE_REPOSITORY_PROTOCOL,
            "version": actual_version,
        }) + "\n",
        executable = False,
    )
    return rctx.repo_metadata(reproducible = True)

linux_source_repository = repository_rule(
    implementation = _linux_source_repository_impl,
    attrs = {
        "integrity": attr.string(
            doc = "SHA-256 SRI digest required with explicit urls.",
        ),
        "patch_strip": attr.int(
            default = 1,
            doc = "Number of leading path components stripped from patches.",
        ),
        "patches": attr.label_list(
            allow_files = True,
            doc = "Deterministic unified-diff patches applied with Bazel's native patcher.",
        ),
        "strip_prefix": attr.string(
            doc = "Archive directory prefix to remove. Catalog entries provide a default.",
        ),
        "urls": attr.string_list(
            doc = "Explicit mirrors for a source not selected solely from the catalog.",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Exact upstream Linux version.",
        ),
        "_source_build_file": attr.label(
            allow_single_file = True,
            default = Label("//:source_repo.BUILD.bazel"),
        ),
        "_rules_cc_defs": attr.label(
            allow_single_file = True,
            default = Label("@rules_cc//cc:defs.bzl"),
        ),
        "_platforms_x86_64": attr.label(
            default = Label("@platforms//cpu:x86_64"),
        ),
    },
    doc = "Downloads an integrity-pinned, complete upstream Linux source tree.",
)

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
        variant_graph_images[name] = "//graph/%s:%s_image" % (name, sanitized)
        variant_rust_enabled[name] = resolved.get("CONFIG_RUST") == "y"

    _write_configs(rctx, arch, configs, rules_repo)
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
            platform = platform,
            base_config = "//configs:%s" % arch,
            base_rust_enabled = base_rust_enabled,
            config_mode = rctx.attr.config_mode,
            graph_image = "//graph/base:%s_image" % _sanitize_target_name(arch),
            variant_configs = variant_configs,
            variant_graph_images = variant_graph_images,
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
            "protocol": _REPOSITORY_GENERATOR_PROTOCOL,
            "rust_enabled": base_rust_enabled,
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
    module = rctx.read(rctx.path(rctx.attr._llvm_module))
    expected = 'LLVM_VERSION = "%s"' % _LLVM_VERSION
    expected_prebuilt = 'PREBUILT_LLVM_VERSION = "%s"' % _LLVM_VERSION
    if module.find(expected) < 0 or module.find(expected_prebuilt) < 0:
        fail(
            "linux.bzl requires Hermetic LLVM %s to match its deterministic Kconfig probe profile" %
            _LLVM_VERSION,
        )

def _read_config(rctx, label, description):
    path = rctx.path(label)
    rctx.watch(path)
    return _parse_config(rctx.read(path), description)

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
    _add_generator_variables(args, descriptor)
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
        'load("%s//:linux.bzl", kconfig_file = "linux_internal_kconfig_file")' % rules_repo,
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
        rules_repo + "//:linux.bzl",
        "-object_label_package",
        "//" + graph_dir,
        "-source_label_package",
        source_package,
        "-source_root_label",
        str(source),
        "-source_tree_label",
        source_repo + "//:all_files",
        "-generated_headers",
        "//:%s_%s_generated_headers" % (target_prefix, descriptor.arch),
        "-source_asn1_compiler",
        "//:%s_asn1_compiler_tool" % target_prefix,
        "-source_config",
        source_config,
        "-allow_shell",
        "-linux_probe_model",
        "linux_llvm",
        "-visibility",
        "//:__subpackages__",
    ]
    if arch == "aarch64":
        args.extend([
            "-source_relacheck",
            "//:%s_relacheck_tool" % target_prefix,
        ])
    args.extend(_graph_config_args(
        config_name,
        rctx.path(config_path),
        config_mode,
    ))

    _add_generator_variables(args, descriptor)

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
    _upgrade_v011_schema(
        rctx,
        graph_dir,
        source_repo,
        source_root,
        descriptor.srcarch,
        rules_repo,
    )
    _validate_generated_metadata(rctx, graph_dir, config_name, source_root)
    _validate_generated_build(rctx, graph_dir, config_name)

def _graph_config_args(config_name, config_path, config_mode):
    return [
        "-config",
        "%s=%s" % (config_name, config_path),
        "-config_mode",
        config_mode,
    ]

def _upgrade_v011_schema(
        rctx,
        graph_dir,
        source_repo,
        source_root,
        srcarch,
        rules_repo):
    build_path = graph_dir + "/BUILD.bazel"
    content = rctx.read(build_path)
    legacy = '    srcs = ["%s//:all_files"],\n' % source_repo
    parts = content.split(legacy)
    if len(parts) != 2:
        fail(
            "Linux graph generator %s emitted an incompatible source-tree schema; " %
            KCONFIG_TOOL_VERSION +
            "expected exactly one legacy srcs declaration",
        )
    replacement = "\n".join([
        '    arch_headers = ["%s//:arch_headers"],' % source_repo,
        '    dtb_sources = ["%s//:dtb_sources"],' % source_repo,
        '    global_headers = ["%s//:global_headers"],' % source_repo,
        '    headers = ["%s//:headers"],' % source_repo,
        '    kbuild_files = ["%s//:kbuild_files"],' % source_repo,
        '    local_include_files = ["%s//:local_include_files"],' % source_repo,
        '    lookup_files = ["%s//:source_tree_lookup_files"],' % source_repo,
        '    scripts_headers = ["%s//:scripts_headers"],' % source_repo,
        '    uapi_headers = ["%s//:uapi_headers"],' % source_repo,
        "",
    ])
    content = parts[0] + replacement + parts[1]

    legacy_require_real = "    require_real = True,\n"
    parts = content.split(legacy_require_real)
    if len(parts) != 2:
        fail(
            "Linux graph generator %s emitted an incompatible image schema; " %
            KCONFIG_TOOL_VERSION +
            "expected exactly one legacy require_real declaration",
        )
    content = parts[0] + parts[1]
    metadata = json.decode(rctx.read(graph_dir + "/metadata.json"))
    content, metadata = _remove_v011_rust_sdk_objects(
        content,
        metadata,
        graph_dir,
    )
    rctx.file(
        graph_dir + "/metadata.json",
        json.encode(metadata) + "\n",
        executable = False,
    )
    content = _inject_v011_source_includes(
        rctx,
        content,
        metadata,
        source_repo,
        source_root,
        srcarch,
    )
    content = _inject_v011_module_objects(
        content,
        metadata,
        graph_dir,
    )
    content = _alias_generated_rule_loads(content, rules_repo)
    rctx.file(build_path, content, executable = False)

def _remove_generated_rule(content, target):
    for rule_name in [
        "linux_arm64_nvhe_object",
        "linux_composite_object",
        "linux_object",
    ]:
        marker = '\n%s(\n    name = "%s",\n' % (rule_name, target)
        parts = content.split(marker)
        if len(parts) == 1:
            continue
        if len(parts) != 2:
            fail("Linux graph generator emitted duplicate target %s" % target)
        end = parts[1].find("\n)\n")
        if end < 0:
            fail("Linux graph generator emitted malformed target %s" % target)
        return parts[0] + "\n" + parts[1][end + len("\n)\n"):]
    fail("Linux graph generator metadata references missing target %s" % target)

def _remove_v011_rust_sdk_objects(content, metadata, graph_dir):
    """Removes Rust runtime objects delegated to linux_rust_kernel_sdk."""
    rust_targets = {}
    kept_variants = []
    for variant in metadata.get("object_variants", []):
        object_path = _clean_source_tree_path(variant.get("object", ""))
        if object_path != None and object_path.startswith("rust/"):
            target = variant.get("target", "")
            if target:
                rust_targets[target] = True
            continue
        kept_variants.append(variant)
    if not rust_targets:
        return content, metadata

    for target in sorted(rust_targets.keys()):
        content = _remove_generated_rule(content, target)
        for label in [
            "//%s:%s" % (graph_dir, target),
            ":" + target,
        ]:
            content = content.replace('"%s",' % label, "")

    metadata["object_variants"] = kept_variants
    for config in metadata.get("configs", []):
        config["object_targets"] = [
            target
            for target in config.get("object_targets", [])
            if target not in rust_targets
        ]
        config["module_object_targets"] = [
            target
            for target in config.get("module_object_targets", [])
            if target not in rust_targets
        ]
    for package in metadata.get("object_packages", []):
        package["object_targets"] = [
            target
            for target in package.get("object_targets", [])
            if target not in rust_targets
        ]
    return content, metadata

def _clean_source_tree_path(path):
    """Normalizes a source-tree path and rejects paths escaping the tree."""
    parts = []
    for raw_part in path.replace("\\", "/").split("/"):
        part = raw_part.strip()
        if not part or part == ".":
            continue
        if part == "..":
            if not parts:
                return None
            parts.pop()
        else:
            parts.append(part)
    return "/".join(parts)

def _source_dir(path):
    parts = path.rsplit("/", 1)
    return parts[0] if len(parts) == 2 else ""

def _join_source_path(root, path):
    if not root:
        return _clean_source_tree_path(path)
    return _clean_source_tree_path(root + "/" + path)

def _parse_source_include(line):
    line = line.strip()
    if not line.startswith("#"):
        return None
    line = line[1:].strip()
    if not line.startswith("include"):
        return None
    line = line[len("include"):].strip()
    if len(line) < 3:
        return None
    opener = line[0]
    if opener == "\"":
        closer = "\""
    elif opener == "<":
        closer = ">"
    else:
        return None
    end = line.find(closer, 1)
    if end <= 1:
        return None
    return line[1:end]

def _source_include_candidates(include):
    candidates = [include]
    if include.startswith("asm/"):
        candidates.append("asm-generic/" + include[len("asm/"):])
    elif include.startswith("uapi/asm/"):
        candidates.append("uapi/asm-generic/" + include[len("uapi/asm/"):])
    return candidates

def _source_include_dirs(flags):
    dirs = []
    next_is_path = False
    for flag in flags:
        path = ""
        if next_is_path:
            path = flag
            next_is_path = False
        elif flag == "-I":
            next_is_path = True
            continue
        elif flag.startswith("-I"):
            path = flag[len("-I"):]
        path = path.strip()
        if path == "$(srctree)":
            dirs.append("")
        elif path.startswith("$(srctree)/"):
            normalized = _clean_source_tree_path(path[len("$(srctree)/"):])
            if normalized != None:
                dirs.append(normalized)
    return dirs

def _is_source_like_include(path):
    return (
        path.endswith(".c") or
        path.endswith(".S") or
        path.endswith(".s") or
        path.endswith(".inc")
    )

def _existing_source_path(source_root, path):
    if path == None:
        return None
    normalized = _clean_source_tree_path(path)
    if normalized == None:
        return None
    return normalized if source_root.get_child(normalized).exists else None

def _resolve_source_include(source_root, from_path, include, include_roots):
    resolved = {}
    local = _existing_source_path(
        source_root,
        _join_source_path(_source_dir(from_path), include),
    )
    if local != None:
        resolved[local] = True
    for candidate in _source_include_candidates(include):
        for root in include_roots:
            path = _existing_source_path(
                source_root,
                _join_source_path(root, candidate),
            )
            if path != None:
                resolved[path] = True
    return sorted(resolved.keys())

def _scan_source_includes(rctx, source_root, path, scan_cache):
    raw_includes = scan_cache.get(path)
    if raw_includes != None:
        return raw_includes
    raw_includes = []
    for line in rctx.read(source_root.get_child(path)).splitlines():
        include = _parse_source_include(line)
        if include != None:
            raw_includes.append(include)
    scan_cache[path] = raw_includes
    return raw_includes

def _propagate_source_include_closures(edges, direct_includes, sources):
    closures = {}
    for path in edges.keys():
        closures[path] = dict(direct_includes.get(path, {}))
    for _ in range(1000):
        changed = False
        for path in sorted(edges.keys()):
            closure = closures[path]
            for child in edges[path]:
                for include in closures.get(child, {}).keys():
                    if include not in closure:
                        closure[include] = True
                        changed = True
        if not changed:
            return {
                source: sorted([
                    include
                    for include in closures.get(source, {}).keys()
                    if include != source
                ])
                for source in sources
            }
    fail("source include closure propagation did not converge")

def _source_include_closures(
        rctx,
        source_root,
        sources,
        include_roots,
        scan_cache):
    pending = {
        source: True
        for source in sources
        if _existing_source_path(source_root, source) != None
    }
    edges = {}
    direct_includes = {}
    for _ in range(1000):
        if not pending:
            return _propagate_source_include_closures(
                edges,
                direct_includes,
                sources,
            )
        current = sorted(pending.keys())
        pending = {}
        for path in current:
            if path in edges:
                continue
            children = {}
            direct = {}
            for include in _scan_source_includes(
                rctx,
                source_root,
                path,
                scan_cache,
            ):
                for resolved in _resolve_source_include(
                    source_root,
                    path,
                    include,
                    include_roots,
                ):
                    children[resolved] = True
                    if _is_source_like_include(resolved):
                        direct[resolved] = True
                    if resolved not in edges:
                        pending[resolved] = True
            edges[path] = sorted(children.keys())
            direct_includes[path] = direct
    fail("source include graph exceeds 1000 levels")

def _inject_v011_source_includes(
        rctx,
        content,
        metadata,
        source_repo,
        source_root,
        srcarch):
    """Adds exact recursive source-like include closures missing from v0.0.11."""
    default_roots = [
        "include",
        "include/uapi",
        "arch/%s/include" % srcarch,
        "arch/%s/include/uapi" % srcarch,
    ]
    scan_cache = {}
    groups = {}
    for variant in metadata.get("object_variants", []):
        if variant.get("members", []):
            continue
        target = variant.get("target", "")
        source = variant.get("source", "")
        if not target or not source:
            continue
        include_roots = {}
        for root in default_roots + _source_include_dirs(variant.get("flags", [])):
            include_roots[root] = True
        roots = sorted(include_roots.keys())
        key = "\n".join(roots)
        group = groups.get(key)
        if group == None:
            group = struct(
                include_roots = roots,
                sources = {},
                variants = [],
            )
            groups[key] = group
        group.sources[source] = True
        group.variants.append(variant)

    for key in sorted(groups.keys()):
        group = groups[key]
        closures = _source_include_closures(
            rctx,
            source_root,
            sorted(group.sources.keys()),
            group.include_roots,
            scan_cache,
        )
        for variant in group.variants:
            target = variant.get("target", "")
            source = variant.get("source", "")
            marker = 'linux_object(\n    name = "%s",\n' % target
            if len(content.split(marker)) != 2:
                fail(
                    "Linux graph generator %s emitted an incompatible object schema for %s" %
                    (KCONFIG_TOOL_VERSION, target),
                )
            includes = closures.get(source, [])
            rendered = "    source_includes_complete = True,\n"
            if includes:
                labels = [
                    "%s//:%s" % (source_repo, include)
                    for include in includes
                ]
                rendered += "    source_includes = %r,\n" % labels
            content = content.replace(marker, marker + rendered)
    return content

def _inject_v011_module_objects(content, metadata, graph_dir):
    """Adds module roots carried by v0.0.11 metadata to its image rules."""
    for config in metadata.get("configs", []):
        targets = config.get("module_object_targets", [])
        if not targets:
            continue
        image_target = config.get("image_target", "")
        if not image_target:
            fail("Linux graph generator emitted module roots without an image target")
        marker = 'linux_compact_image(\n    name = "%s",\n' % image_target
        if len(content.split(marker)) != 2:
            fail(
                "Linux graph generator %s emitted an incompatible module image schema for %s" %
                (KCONFIG_TOOL_VERSION, image_target),
            )
        labels = [
            "//%s:%s" % (graph_dir, target)
            for target in targets
        ]
        rendered = "    module_objects = %r,\n" % labels
        content = content.replace(marker, marker + rendered)
    return content

def _alias_generated_rule_loads(content, rules_repo):
    parts = content.split("\npackage(")
    if len(parts) != 2:
        fail("Linux graph generator emitted an incompatible BUILD file package declaration")
    header = parts[0]
    load_label = '"%s//:linux.bzl"' % rules_repo
    if len(header.split(load_label)) != 2:
        fail("Linux graph generator emitted an incompatible linux.bzl load declaration")
    generated_symbols = {
        "linux_arm64_nvhe_object": "linux_internal_arm64_nvhe_object",
        "linux_compact_image": "linux_internal_compact_image",
        "linux_composite_object": "linux_internal_composite_object",
        "linux_config": "linux_internal_config",
        "linux_object": "linux_internal_object",
        "linux_source_tree": "linux_internal_source_tree",
    }
    for symbol in sorted(generated_symbols.keys()):
        header = header.replace(
            '"%s"' % symbol,
            '%s = "%s"' % (symbol, generated_symbols[symbol]),
        )
    return header + "\npackage(" + parts[1]

def _add_generator_variables(args, descriptor):
    variables = dict(descriptor.compact_vars)
    variables.update({
        "ARCH": descriptor.arch,
        "SRCARCH": descriptor.srcarch,
        "UTS_MACHINE": descriptor.uts_machine,
    })
    for key in sorted(variables.keys()):
        args.extend(["-var", "%s=%s" % (key, variables[key])])
    probe_env = dict(_PROBE_ENV)
    probe_env["ARCH"] = descriptor.arch
    probe_env["SRCARCH"] = descriptor.srcarch
    for key in sorted(probe_env.keys()):
        args.extend(["-env", "%s=%s" % (key, probe_env[key])])
    for key in sorted(_PROBE_VALUES.keys()):
        args.extend(["-linux_probe_value", "%s=%s" % (key, _PROBE_VALUES[key])])

def _validate_generated_metadata(rctx, graph_dir, config_name, source_root):
    metadata = json.decode(rctx.read(graph_dir + "/metadata.json"))
    if type(metadata) != "dict":
        fail("Linux graph generator wrote invalid metadata")
    generated_configs = metadata.get("configs", [])
    names = sorted([config.get("name", "") for config in generated_configs])
    if names != [config_name]:
        fail("Linux graph generator emitted configs %s, expected [%r]" % (names, config_name))

    variants = metadata.get("object_variants", [])
    if type(variants) != "list" or not variants:
        fail("Linux graph generator produced no object variants")
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
                "Linux graph for config %s selects object %s with invalid Kbuild mode %r" %
                (config_name, object_path, mode),
            )
        if variant.get("members", []):
            continue
        source = variant.get("source", "")
        if type(source) != "string" or not source:
            fail(
                "Linux graph for config %s cannot resolve a concrete source for leaf object %s" %
                (config_name, object_path),
            )
        if not source_root.get_child(source).exists:
            fail(
                "Linux graph for config %s resolved object %s to missing source %s" %
                (config_name, object_path, source),
            )

def _validate_generated_build(rctx, graph_dir, config_name):
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
        if "\n    src = " in "\n" + block:
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
                "Linux graph for config %s emitted leaf object %s without a buildable src; " +
                "its Kbuild source or flag expressions are not implemented"
            ) %
            (config_name, name),
        )

def _kernel_root_build(
        arch,
        version,
        source_repo,
        platform,
        base_config,
        base_rust_enabled,
        config_mode,
        graph_image,
        variant_configs,
        variant_graph_images,
        variant_rust_enabled,
        rules_repo):
    return """load("{rules_repo}//:linux.bzl", linux_image_targets = "linux_internal_image_targets")

package(default_visibility = ["//visibility:private"])

linux_image_targets(
    name = "_kernel_graph",
    arch = {arch},
    version = {version},
    source_repo = {source_repo},
    platform = {platform},
    base_config = {base_config},
    base_rust_enabled = {base_rust_enabled},
    config_mode = {config_mode},
    graph_image = {graph_image},
    variant_configs = {variant_configs},
    variant_graph_images = {variant_graph_images},
    variant_rust_enabled = {variant_rust_enabled},
)
""".format(
        arch = repr(arch),
        version = repr(version),
        source_repo = repr(source_repo),
        platform = repr(platform),
        base_config = repr(base_config),
        base_rust_enabled = repr(base_rust_enabled),
        config_mode = repr(config_mode),
        graph_image = repr(graph_image),
        variant_configs = _starlark_dict(variant_configs, indent = "        "),
        variant_graph_images = _starlark_dict(variant_graph_images, indent = "        "),
        variant_rust_enabled = _starlark_dict(variant_rust_enabled, indent = "        "),
        rules_repo = rules_repo,
    )

repositories_test_helpers = struct(
    graph_config_args = _graph_config_args,
    generator_protocol = _REPOSITORY_GENERATOR_PROTOCOL,
    kernel_root_build = _kernel_root_build,
)

def _variant_build(arch, graph, platform, rules_repo):
    return """load("{rules_repo}//:linux.bzl", linux_kernel_exports = "linux_internal_kernel_exports")

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

def _repository_prefix(label):
    return "@@" + label.repo_name

def _linux_makefile_version(content):
    values = {}
    for raw_line in content.splitlines():
        line = raw_line.strip()
        for key in ["VERSION", "PATCHLEVEL", "SUBLEVEL", "EXTRAVERSION"]:
            prefix = key + " ="
            if line.startswith(prefix):
                values[key] = line[len(prefix):].strip()
    missing = [
        key
        for key in ["VERSION", "PATCHLEVEL", "SUBLEVEL"]
        if key not in values
    ]
    if missing:
        fail("Linux Makefile is missing version fields: %s" % missing)
    return "%s.%s.%s%s" % (
        values["VERSION"],
        values["PATCHLEVEL"],
        values["SUBLEVEL"],
        values.get("EXTRAVERSION", ""),
    )
