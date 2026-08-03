"""Bazel action wrapper for compact Linux Kconfig/Kbuild generation."""

load("@bazel_skylib//rules:diff_test.bzl", "diff_test")
load(":kconfig.bzl", "KconfigInfo")
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "directory_anchor",
    "path_mapped_run",
)

visibility("//...")

LinuxCompactInfo = provider(
    doc = "Generated compact Linux metadata and BUILD-file artifacts.",
    fields = {
        "buildfile": "Generated combined BUILD file for compact object and image targets.",
        "metadata": "Compact metadata JSON artifact.",
        "object_label_package": "Package label path used by image BUILD files to reference object variants.",
    },
)

def _linux_compact_outputs_impl(ctx):
    metadata = ctx.actions.declare_file(ctx.label.name + ".metadata.json")
    buildfile = ctx.actions.declare_file(ctx.label.name + ".BUILD.bazel")

    configs = []
    config_names = {}
    inputs = [ctx.file.root, ctx.file.kbuild] + ctx.files.srcs
    for target, name in ctx.attr.configs.items():
        if not name:
            fail("compact config names must be non-empty")
        if name in config_names:
            fail("duplicate compact config name %r" % name)
        config_names[name] = True
        file = _config_file(target, "configs")
        configs.append((name, file))
        inputs.append(file)
    if not configs:
        fail("configs must contain at least one compact config")
    if ctx.attr.compact_base_config not in config_names:
        fail(
            "compact_base_config %r is not present in configs %s" %
            (ctx.attr.compact_base_config, sorted(config_names.keys())),
        )
    if not ctx.attr.compile_environment_abi:
        fail("compile_environment_abi must be non-empty")
    header_config_names = sorted(ctx.attr.generated_headers_by_config.keys())
    if header_config_names != sorted(config_names.keys()):
        fail(
            "generated_headers_by_config keys %s do not match compact config names %s" %
            (header_config_names, sorted(config_names.keys())),
        )
    for config_name, label in ctx.attr.generated_headers_by_config.items():
        if not label:
            fail("generated_headers_by_config[%r] must be non-empty" % config_name)
    if not ctx.attr.source_label_package:
        fail("source_label_package must be non-empty")
    if not ctx.attr.source_root_label:
        fail("source_root_label must be non-empty")

    args = ctx.actions.args()
    args.add("-compact_base_config", ctx.attr.compact_base_config)
    args.add("-compile_environment_abi", ctx.attr.compile_environment_abi)
    for config_name, label in sorted(ctx.attr.generated_headers_by_config.items()):
        args.add("-generated_headers_for_config", "%s=%s" % (config_name, label))
    args.add("-root", ctx.file.root)
    args.add("-kbuild", ctx.file.kbuild)
    args.add("-compact_metadata_out", metadata)
    args.add("-compact_buildfile_out", buildfile)
    for exported_file in ctx.attr.buildfile_exports:
        args.add("-compact_buildfile_export", exported_file)
    if ctx.attr.kbuild_tree:
        args.add("-compact_kbuild_tree")
    args.add("-config_mode", ctx.attr.config_mode)
    args.add("-kernel_version", ctx.attr.kernel_version)
    args.add("-object_label_package", ctx.attr.object_label_package)
    args.add("-source_label_package", ctx.attr.source_label_package)
    if ctx.attr.source_asn1_compiler:
        args.add("-source_asn1_compiler", ctx.attr.source_asn1_compiler)
    if ctx.attr.source_genksyms:
        args.add("-source_genksyms", ctx.attr.source_genksyms)
    if ctx.attr.source_objtool:
        args.add("-source_objtool", ctx.attr.source_objtool)
    if ctx.attr.source_relacheck:
        args.add("-source_relacheck", ctx.attr.source_relacheck)
    args.add("-source_root_label", ctx.attr.source_root_label)
    args.add("-linux_objects_load", ctx.attr.linux_objects_load)
    env = dict(ctx.attr.env)
    vars = dict(ctx.attr.vars)
    directory_vars = {}
    if "srctree" not in vars:
        vars["srctree"] = ""
        directory_vars["srctree"] = directory_anchor(ctx.file.root)

    _add_probe_args(args, ctx.attr.allow_shell, vars, env)

    _add_var_args(args, vars, directory_vars)
    for key, value in sorted(env.items()):
        args.add("-env", "%s=%s" % (key, value))
    for visibility in ctx.attr.generated_visibility:
        args.add("-visibility", visibility)

    for name, file in sorted(configs):
        args.add("-config")
        args.add(file, format = name + "=%s")

    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._kconfig_parse,
        inputs = depset(inputs),
        outputs = [
            metadata,
            buildfile,
        ],
        arguments = [args],
        mnemonic = "LinuxCompactKconfig",
        progress_message = "Generating compact Linux metadata for %{label}",
    )

    return [
        DefaultInfo(files = depset([
            metadata,
            buildfile,
        ])),
        LinuxCompactInfo(
            buildfile = buildfile,
            metadata = metadata,
            object_label_package = ctx.attr.object_label_package,
        ),
        OutputGroupInfo(
            buildfile = depset([buildfile]),
            metadata = depset([metadata]),
        ),
    ]

_linux_compact_outputs = rule(
    implementation = _linux_compact_outputs_impl,
    attrs = {
        "allow_shell": attr.bool(
            doc = "Allow $(shell,...) expansion while parsing Kconfig files.",
        ),
        "buildfile_exports": attr.string_list(
            doc = "Source filenames exported by the generated compact BUILD file.",
        ),
        "config_mode": attr.string(
            default = "default",
            doc = "Config resolver mode passed to kconfig_parse. Supported: default, allnoconfig.",
            values = [
                "default",
                "allnoconfig",
            ],
        ),
        "compact_base_config": attr.string(
            mandatory = True,
            doc = "Base config name used to emit content-addressed delta image targets.",
        ),
        "compile_environment_abi": attr.string(
            mandatory = True,
            doc = "Toolchain and action ABI identity bound into content-addressed compile environments.",
        ),
        "configs": attr.label_keyed_string_dict(
            allow_files = True,
            mandatory = True,
            doc = "Map of .config file labels to generated config names.",
        ),
        "env": attr.string_dict(
            doc = "Hermetic Kconfig preprocessor environment values.",
        ),
        "generated_visibility": attr.string_list(
            default = ["//visibility:public"],
            doc = "Default visibility emitted into generated compact BUILD files.",
        ),
        "generated_headers_by_config": attr.string_dict(
            mandatory = True,
            doc = "Map of compact config names to generated-header labels.",
        ),
        "kbuild": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Kbuild/Makefile input for compact object metadata.",
        ),
        "kbuild_tree": attr.bool(
            doc = "Follow Kbuild directory descent from the kbuild root when generating compact metadata.",
        ),
        "kernel_version": attr.string(
            default = "6.18.2",
            doc = "Base kernel release used when materializing indexed config payloads.",
        ),
        "linux_objects_load": attr.string(
            default = "@linux.bzl//internal:linux_objects.bzl",
            doc = "Load label emitted for linux_object/linux_compact_image rules.",
        ),
        "object_label_package": attr.string(
            doc = "Package label path used by generated image BUILD files to reference generated object targets.",
        ),
        "root": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Root Kconfig input.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Additional Kconfig source files read through source statements.",
        ),
        "source_label_package": attr.string(
            mandatory = True,
            doc = "Package label path used by generated object BUILD files to reference Linux source files.",
        ),
        "source_asn1_compiler": attr.string(
            doc = "Label for a scripts/asn1_compiler executable emitted into source-backed compact object rules.",
        ),
        "source_genksyms": attr.string(
            doc = "Label for a scripts/genksyms executable emitted into source-backed compact object rules.",
        ),
        "source_objtool": attr.string(
            doc = "Label for objtool emitted into x86 source-backed compact object rules.",
        ),
        "source_relacheck": attr.string(
            doc = "Label for arch/arm64/kernel/pi/relacheck emitted into arm64 .pi.o object rules.",
        ),
        "source_root_label": attr.string(
            mandatory = True,
            doc = "Label for a file in the Linux source root emitted into source-backed compact object rules.",
        ),
        "vars": attr.string_dict(
            doc = "Kconfig preprocessor variables.",
        ),
        "_kconfig_parse": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kconfig_parse:kconfig_parse"),
            executable = True,
        ),
    },
)

def _linux_parser_validation_impl(ctx):
    if not ctx.attr.kconfigs and not ctx.attr.kbuilds:
        fail("at least one of kconfigs or kbuilds must be set")

    env = dict(ctx.attr.env)
    vars = dict(ctx.attr.vars)
    source_root = ctx.file.source_root.dirname
    source_root_anchor = directory_anchor(ctx.file.source_root, source_root)
    directory_vars = {}
    if "srctree" not in vars:
        vars["srctree"] = ""
        directory_vars["srctree"] = source_root_anchor

    outputs = []
    seen_names = {}

    kconfigs = []
    for target, name in ctx.attr.kconfigs.items():
        if name in seen_names:
            fail("duplicate parser validation name %r" % name)
        seen_names[name] = True
        kconfigs.append((name, _single_file(target, "kconfigs")))
    for name, file in sorted(kconfigs):
        out = ctx.actions.declare_file("%s.%s.kconfig.json" % (ctx.label.name, _sanitize_output_fragment(name)))
        inputs = [file, ctx.file.source_root] + ctx.files.srcs
        root = _source_relative_path(file, source_root)
        if root != "scripts/Kconfig.include":
            root_file = ctx.actions.declare_file("%s.%s.root.Kconfig" % (ctx.label.name, _sanitize_output_fragment(name)))
            ctx.actions.write(
                output = root_file,
                content = """mainmenu "Linux parser validation"
source "scripts/Kconfig.include"
source "%s"
""" % root,
            )
            root = root_file
            inputs.append(root_file)
        args = ctx.actions.args()
        args.add("-root", root)
        args.add("-srctree")
        add_directory_arg(args, source_root_anchor)
        _add_probe_args(args, ctx.attr.allow_shell, vars, env)
        _add_var_args(args, vars, directory_vars)
        _add_env_args(args, env)
        args.add("-out", out)
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._kconfig_parse,
            inputs = depset(inputs),
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxKconfigParseValidation",
            progress_message = "Validating Linux Kconfig parser for %s" % file.short_path,
        )
        outputs.append(out)

    kbuilds = []
    for target, name in ctx.attr.kbuilds.items():
        if name in seen_names:
            fail("duplicate parser validation name %r" % name)
        seen_names[name] = True
        kbuilds.append((name, _single_file(target, "kbuilds")))
    for name, file in sorted(kbuilds):
        out = ctx.actions.declare_file("%s.%s.kbuild.json" % (ctx.label.name, _sanitize_output_fragment(name)))
        args = ctx.actions.args()
        args.add("-kbuild", file)
        if ctx.attr.kbuild_recursive:
            args.add("-kbuild_recursive")
            args.add("-kbuild_srctree")
            add_directory_arg(args, source_root_anchor)
            _add_var_args(args, vars, directory_vars)
        args.add("-kbuild_out", out)
        inputs = [file]
        if ctx.attr.kbuild_recursive:
            inputs = [file, ctx.file.source_root] + ctx.files.srcs
        path_mapped_run(
            ctx.actions,
            executable = ctx.executable._kconfig_parse,
            inputs = depset(inputs),
            outputs = [out],
            arguments = [args],
            mnemonic = "LinuxKbuildParseValidation",
            progress_message = "Validating Linux Kbuild parser for %s" % file.short_path,
        )
        outputs.append(out)

    return [DefaultInfo(files = depset(outputs))]

_linux_parser_validation = rule(
    implementation = _linux_parser_validation_impl,
    attrs = {
        "allow_shell": attr.bool(
            doc = "Allow $(shell,...) expansion while parsing Kconfig files.",
        ),
        "env": attr.string_dict(
            doc = "Hermetic Kconfig preprocessor environment values.",
        ),
        "kbuilds": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "Map of Kbuild/Makefile labels to stable validation output names.",
        ),
        "kbuild_recursive": attr.bool(
            doc = "Follow static Kbuild include directives while validating Kbuild files.",
        ),
        "kconfigs": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "Map of Kconfig labels to stable validation output names.",
        ),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "A file in the Linux source root; its directory is used as srctree.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Additional Kconfig source files read through source statements.",
        ),
        "vars": attr.string_dict(
            doc = "Kconfig preprocessor variables.",
        ),
        "_kconfig_parse": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kconfig_parse:kconfig_parse"),
            executable = True,
        ),
    },
)

def _linux_kbuild_tree_validation_impl(ctx):
    source_root = ctx.file.source_root.dirname
    source_root_anchor = directory_anchor(ctx.file.source_root, source_root)
    vars = dict(ctx.attr.vars)
    directory_vars = {}
    if "srctree" not in vars:
        vars["srctree"] = ""
        directory_vars["srctree"] = source_root_anchor

    out = ctx.actions.declare_file(ctx.label.name + ".kbuild_tree.json")
    args = ctx.actions.args()
    args.add("-kbuild_tree_root")
    add_directory_arg(args, source_root_anchor)
    args.add("-kbuild_tree_out", out)
    if ctx.attr.min_files:
        args.add("-kbuild_tree_min_count", ctx.attr.min_files)
    _add_var_args(args, vars, directory_vars)
    for exclude in ctx.attr.excludes:
        args.add("-kbuild_tree_exclude", exclude)

    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._kconfig_parse,
        inputs = depset([ctx.file.source_root] + ctx.files.srcs),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxKbuildTreeParseValidation",
        progress_message = "Validating Linux Kbuild/Makefile files for %{label}",
    )

    return [DefaultInfo(files = depset([out]))]

_linux_kbuild_tree_validation = rule(
    implementation = _linux_kbuild_tree_validation_impl,
    attrs = {
        "excludes": attr.string_list(
            default = [
                ".git",
                "Documentation",
                "tools",
            ],
            doc = "Source-root-relative subtrees skipped by recursive Kbuild validation.",
        ),
        "min_files": attr.int(
            default = 1,
            doc = "Minimum number of Kbuild-like files that must parse for validation to pass.",
        ),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "A file in the Linux source root; its directory is walked for Kbuild validation.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Linux source files available to the recursive Kbuild validation action.",
        ),
        "vars": attr.string_dict(
            doc = "Hermetic Make variables passed to each standalone Kbuild parse.",
        ),
        "_kconfig_parse": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kconfig_parse:kconfig_parse"),
            executable = True,
        ),
    },
)

def linux_compact_buildfiles(
        name,
        root,
        kbuild,
        configs,
        compact_base_config,
        compile_environment_abi,
        generated_headers_by_config,
        source_label_package,
        source_root_label,
        config_mode = "default",
        srcs = [],
        object_label_package = None,
        source_asn1_compiler = "",
        source_genksyms = "",
        source_objtool = "",
        source_relacheck = "",
        linux_objects_load = "@linux.bzl//internal:linux_objects.bzl",
        kbuild_tree = False,
        kernel_version = "6.18.2",
        out_buildfile = None,
        out_metadata = None,
        generated_visibility = ["//visibility:public"],
        allow_shell = False,
        vars = {},
        env = {},
        target_compatible_with = None,
        visibility = None,
        tags = None):
    """Generate and optionally golden-test compact Linux metadata BUILD files."""
    if object_label_package == None:
        object_label_package = native.package_name()

    generated = name + "_generated"
    buildfile_exports = []
    if out_buildfile != None and out_metadata != None and _source_directory(out_buildfile) == _source_directory(out_metadata):
        buildfile_exports.append(_basename(out_metadata))
    _linux_compact_outputs(
        name = generated,
        allow_shell = allow_shell,
        buildfile_exports = buildfile_exports,
        config_mode = config_mode,
        compact_base_config = compact_base_config,
        compile_environment_abi = compile_environment_abi,
        configs = configs,
        env = env,
        generated_visibility = generated_visibility,
        generated_headers_by_config = generated_headers_by_config,
        kbuild = kbuild,
        kbuild_tree = kbuild_tree,
        kernel_version = kernel_version,
        linux_objects_load = linux_objects_load,
        object_label_package = object_label_package,
        root = root,
        source_label_package = source_label_package,
        source_asn1_compiler = source_asn1_compiler,
        source_genksyms = source_genksyms,
        source_objtool = source_objtool,
        source_relacheck = source_relacheck,
        source_root_label = source_root_label,
        srcs = srcs,
        tags = tags,
        target_compatible_with = target_compatible_with,
        vars = vars,
        visibility = visibility,
    )

    metadata = name + "_metadata"
    buildfile = name + "_buildfile"
    native.filegroup(
        name = buildfile,
        output_group = "buildfile",
        srcs = [":" + generated],
        tags = tags,
        target_compatible_with = target_compatible_with,
        visibility = visibility,
    )
    native.filegroup(
        name = metadata,
        output_group = "metadata",
        srcs = [":" + generated],
        tags = tags,
        target_compatible_with = target_compatible_with,
        visibility = visibility,
    )

    if out_buildfile != None:
        diff_test(
            name = name + "_buildfile_test",
            file1 = ":" + buildfile,
            file2 = _source_file_label(out_buildfile),
            target_compatible_with = target_compatible_with,
            tags = tags,
        )
    if out_metadata != None:
        diff_test(
            name = name + "_metadata_test",
            file1 = ":" + metadata,
            file2 = _source_file_label(out_metadata),
            target_compatible_with = target_compatible_with,
            tags = tags,
        )
    return struct(
        buildfile = ":" + buildfile,
        metadata = ":" + metadata,
    )

def linux_parser_validation(
        name,
        source_root,
        kconfigs = {},
        kbuilds = {},
        kbuild_recursive = False,
        srcs = [],
        allow_shell = False,
        vars = {},
        env = {},
        visibility = None,
        tags = None):
    """Validate that representative Linux Kconfig/Kbuild files parse in Bazel."""
    _linux_parser_validation(
        name = name,
        allow_shell = allow_shell,
        env = env,
        kbuilds = kbuilds,
        kbuild_recursive = kbuild_recursive,
        kconfigs = kconfigs,
        source_root = source_root,
        srcs = srcs,
        tags = tags,
        vars = vars,
        visibility = visibility,
    )

def linux_kbuild_tree_validation(
        name,
        source_root,
        srcs = [],
        excludes = [
            ".git",
            "Documentation",
            "tools",
        ],
        min_files = 1,
        vars = {},
        visibility = None,
        tags = None):
    """Validate that every Kbuild-like file in a Linux source tree parses."""
    _linux_kbuild_tree_validation(
        name = name,
        excludes = excludes,
        min_files = min_files,
        source_root = source_root,
        srcs = srcs,
        tags = tags,
        vars = vars,
        visibility = visibility,
    )

def _single_file(target, attr_name):
    files = target.files.to_list()
    if len(files) != 1:
        fail("%s entry %s must provide exactly one file, got %d" % (attr_name, target.label, len(files)))
    return files[0]

def _source_file_label(path):
    if "/" in path:
        directory, filename = path.rsplit("/", 1)
        return "//%s:%s" % (_join_package(native.package_name(), directory), filename)
    return ":" + path

def _source_directory(path):
    if "/" not in path:
        return ""
    return path.rsplit("/", 1)[0]

def _basename(path):
    if "/" not in path:
        return path
    return path.rsplit("/", 1)[1]

def _join_package(parent, child):
    if not parent:
        return child
    if not child:
        return parent
    return parent + "/" + child

def _config_file(target, attr_name):
    if KconfigInfo in target:
        return target[KconfigInfo].config
    return _single_file(target, attr_name)

def _add_var_args(args, vars, directory_vars = {}):
    for key, value in sorted(vars.items()):
        args.add("-var")
        anchor = directory_vars.get(key)
        if anchor == None:
            args.add("%s=%s" % (key, value))
        else:
            add_directory_arg(args, anchor, format = key + "=%s")

def _add_env_args(args, env):
    for key, value in sorted(env.items()):
        args.add("-env", "%s=%s" % (key, value))

def _add_probe_args(args, allow_shell, vars, env):
    if not allow_shell:
        return
    vars_arch = vars.get("ARCH", "")
    env_arch = env.get("ARCH", "")
    if vars_arch and env_arch and vars_arch != env_arch:
        fail("Linux probe ARCH differs between vars (%r) and env (%r)" % (vars_arch, env_arch))
    architecture = env_arch or vars_arch
    if not architecture:
        fail("allow_shell requires ARCH in vars or env for the fixed Linux probe policy")
    args.add("-allow_shell")
    args.add("-linux_probe_arch", architecture)

def _sanitize_output_fragment(value):
    out = ""
    allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"
    for i in range(len(value)):
        c = value[i]
        if c in allowed:
            out += c
        else:
            out += "_"
    if not out:
        return "unnamed"
    return out

def _source_relative_path(file, source_root):
    prefix = source_root + "/"
    path = file.path
    if path.startswith(prefix):
        return path[len(prefix):]
    return path
