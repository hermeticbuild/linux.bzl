"""Bazel action wrapper for compact Linux Kconfig/Kbuild generation."""

load("@bazel_skylib//rules:diff_test.bzl", "diff_test")
load(":kconfig.bzl", "KconfigInfo")

LinuxProbeInfo = provider(
    doc = "Explicit Linux Kconfig probe values supplied by Bazel configuration/toolchain wrapper targets.",
    fields = {
        "allow_shell": "Whether parser actions may use the deterministic probe shell.",
        "env": "Hermetic environment values used by Kconfig preprocessing.",
        "model": "Named built-in probe model.",
        "values": "Probe value overrides passed to kconfig_parse.",
    },
)

LinuxCompactInfo = provider(
    doc = "Generated compact Linux metadata and BUILD-file artifacts.",
    fields = {
        "buildfile": "Generated combined BUILD file for compact object and image targets.",
        "metadata": "Compact metadata JSON artifact.",
        "object_label_package": "Package label path used by image BUILD files to reference object variants.",
    },
)

def _linux_probe_config_impl(ctx):
    return [LinuxProbeInfo(
        allow_shell = ctx.attr.allow_shell,
        env = dict(ctx.attr.env),
        model = ctx.attr.model,
        values = dict(ctx.attr.values),
    )]

linux_probe_config = rule(
    implementation = _linux_probe_config_impl,
    attrs = {
        "allow_shell": attr.bool(
            default = True,
            doc = "Allow parser actions to use the deterministic Linux probe shell.",
        ),
        "env": attr.string_dict(
            doc = "Hermetic Kconfig preprocessor environment values.",
        ),
        "model": attr.string(
            default = "linux_llvm",
            doc = "Named built-in Linux probe model.",
        ),
        "values": attr.string_dict(
            doc = "Probe value overrides, for example cc_version or pahole_version.",
        ),
    },
    doc = "Bazel provider wrapper for Linux Kconfig toolchain/configuration probe values.",
)

def _linux_compact_outputs_impl(ctx):
    metadata = ctx.actions.declare_file(ctx.label.name + ".metadata.json")
    buildfile = ctx.actions.declare_file(ctx.label.name + ".BUILD.bazel")

    args = ctx.actions.args()
    args.add("-root", ctx.file.root)
    args.add("-kbuild", ctx.file.kbuild)
    args.add("-compact_metadata_out", metadata)
    args.add("-compact_buildfile_out", buildfile)
    for exported_file in ctx.attr.buildfile_exports:
        args.add("-compact_buildfile_export", exported_file)
    if ctx.attr.generated_headers:
        args.add("-generated_headers", ctx.attr.generated_headers)
    if ctx.attr.kbuild_tree:
        args.add("-compact_kbuild_tree")
    args.add("-object_label_package", ctx.attr.object_label_package)
    if ctx.attr.source_label_package:
        args.add("-source_label_package", ctx.attr.source_label_package)
    if ctx.attr.source_asn1_compiler:
        args.add("-source_asn1_compiler", ctx.attr.source_asn1_compiler)
    if ctx.attr.source_config:
        args.add("-source_config", ctx.attr.source_config)
    if ctx.attr.source_root_label:
        args.add("-source_root_label", ctx.attr.source_root_label)
    for label in ctx.attr.source_tree_labels:
        args.add("-source_tree_label", label)
    args.add("-linux_objects_load", ctx.attr.linux_objects_load)
    env = dict(ctx.attr.env)
    vars = dict(ctx.attr.vars)
    if "srctree" not in vars:
        vars["srctree"] = ctx.file.root.dirname

    probe = _probe_settings(ctx, env)
    _add_probe_args(args, probe.allow_shell, probe.model, probe.values)

    for key, value in sorted(vars.items()):
        args.add("-var", "%s=%s" % (key, value))
    for key, value in sorted(env.items()):
        args.add("-env", "%s=%s" % (key, value))
    for visibility in ctx.attr.generated_visibility:
        args.add("-visibility", visibility)

    configs = []
    inputs = [ctx.file.root, ctx.file.kbuild] + ctx.files.srcs
    seen_config_names = {}
    if ctx.attr.config:
        if not ctx.attr.config_name:
            fail("config_name must be set when config is set")
        file = _config_file(ctx.attr.config, "config")
        configs.append((ctx.attr.config_name, file))
        inputs.append(file)
        seen_config_names[ctx.attr.config_name] = True
    for target, name in ctx.attr.configs.items():
        if name in seen_config_names:
            fail("duplicate compact config name %q" % name)
        seen_config_names[name] = True
        file = _config_file(target, "configs")
        configs.append((name, file))
        inputs.append(file)
    if not configs:
        fail("at least one compact config must be provided")
    for name, file in sorted(configs):
        args.add("-config", "%s=%s" % (name, file.path))

    ctx.actions.run(
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
        "config": attr.label(
            allow_files = True,
            doc = "Single .config file label for the generated config name.",
        ),
        "config_name": attr.string(
            doc = "Generated config name for the single config attr.",
        ),
        "configs": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "Map of .config file labels to generated config names.",
        ),
        "env": attr.string_dict(
            doc = "Hermetic Kconfig preprocessor environment values.",
        ),
        "generated_visibility": attr.string_list(
            default = ["//visibility:public"],
            doc = "Default visibility emitted into generated compact BUILD files.",
        ),
        "generated_headers": attr.string(
            doc = "Label for generated Linux headers emitted into source-backed compact object rules.",
        ),
        "kbuild": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Kbuild/Makefile input for compact object metadata.",
        ),
        "kbuild_tree": attr.bool(
            doc = "Follow Kbuild directory descent from the kbuild root when generating compact metadata.",
        ),
        "linux_objects_load": attr.string(
            default = "@linux.bzl//internal:linux_objects.bzl",
            doc = "Load label emitted for linux_object/linux_compact_image placeholder rules.",
        ),
        "object_label_package": attr.string(
            doc = "Package label path used by generated image BUILD files to reference generated object targets.",
        ),
        "probe_model": attr.string(
            default = "linux_llvm",
            doc = "Hermetic Linux Kconfig probe model used when allow_shell is set. Set empty to use only the explicit shell environment.",
        ),
        "probe_config": attr.label(
            providers = [LinuxProbeInfo],
            doc = "Optional provider carrying Linux probe values derived from Bazel configuration/toolchain wrapper targets.",
        ),
        "probe_values": attr.string_dict(
            doc = "Overrides for the selected Linux Kconfig probe model, for example cc_version or pahole_version.",
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
            doc = "Package label path used by generated object BUILD files to reference Linux source files.",
        ),
        "source_asn1_compiler": attr.string(
            doc = "Label for a scripts/asn1_compiler executable emitted into source-backed compact object rules.",
        ),
        "source_config": attr.string(
            doc = "Label for a full linux_config target emitted into source-backed compact object rules.",
        ),
        "source_root_label": attr.string(
            doc = "Label for a file in the Linux source root emitted into source-backed compact object rules.",
        ),
        "source_tree_labels": attr.string_list(
            doc = "Labels for source tree inputs emitted into source-backed compact object rules.",
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

def _linux_compact_update_source_files_impl(ctx):
    entries = []
    runfiles = []
    for target, destination in ctx.attr.files.items():
        file = _single_file(target, "files")
        entries.append((destination, file.short_path))
        runfiles.append(file)

    entries = sorted(entries)

    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    lines = [
        "#!/usr/bin/env bash",
        "set -euo pipefail",
        "",
        "if [[ -z \"${BUILD_WORKSPACE_DIRECTORY:-}\" ]]; then",
        "  echo \"This updater must be run with bazel run so BUILD_WORKSPACE_DIRECTORY is set.\" >&2",
        "  exit 1",
        "fi",
        "",
        "runfiles_dir=\"${RUNFILES_DIR:-$0.runfiles}\"",
        "workspace_runfiles=\"${runfiles_dir}/_main\"",
        "if [[ ! -d \"${workspace_runfiles}\" ]]; then",
        "  workspace_runfiles=\"${runfiles_dir}\"",
        "fi",
        "workspace_path_prefix=" + _sh_quote(ctx.attr.workspace_path_prefix),
        "",
        "copy_one() {",
        "  local src_rel=\"$1\"",
        "  local dest_rel=\"$2\"",
        "  local src=\"${workspace_runfiles}/${src_rel}\"",
        "  local dest=\"${BUILD_WORKSPACE_DIRECTORY}/${workspace_path_prefix}/${dest_rel}\"",
        "  if [[ ! -f \"${src}\" ]]; then",
        "    echo \"missing generated runfile: ${src}\" >&2",
        "    exit 1",
        "  fi",
        "  mkdir -p \"$(dirname \"${dest}\")\"",
        "  cp -f \"${src}\" \"${dest}\"",
        "  echo \"updated ${dest_rel}\"",
        "}",
        "",
    ]
    for destination, short_path in entries:
        lines.append("copy_one %s %s" % (_sh_quote(short_path), _sh_quote(destination)))

    ctx.actions.write(
        output = script,
        content = "\n".join(lines) + "\n",
        is_executable = True,
    )

    return [DefaultInfo(
        executable = script,
        runfiles = ctx.runfiles(files = runfiles),
    )]

_linux_compact_update_source_files = rule(
    implementation = _linux_compact_update_source_files_impl,
    attrs = {
        "files": attr.label_keyed_string_dict(
            allow_files = True,
            mandatory = True,
            doc = "Map of generated file labels to workspace-relative source paths to update.",
        ),
        "workspace_path_prefix": attr.string(
            doc = "Optional workspace-relative prefix prepended to every update destination.",
        ),
    },
    executable = True,
    doc = "Updates generated compact Linux source files from Bazel outputs.",
)

def _linux_parser_validation_impl(ctx):
    if not ctx.attr.kconfigs and not ctx.attr.kbuilds:
        fail("at least one of kconfigs or kbuilds must be set")

    env = dict(ctx.attr.env)
    vars = dict(ctx.attr.vars)
    source_root = ctx.file.source_root.dirname
    if "srctree" not in vars:
        vars["srctree"] = source_root

    probe = _probe_settings(ctx, env)

    outputs = []
    seen_names = {}

    kconfigs = []
    for target, name in ctx.attr.kconfigs.items():
        if name in seen_names:
            fail("duplicate parser validation name %q" % name)
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
            root = root_file.path
            inputs.append(root_file)
        args = ctx.actions.args()
        args.add("-root", root)
        args.add("-srctree", source_root)
        _add_probe_args(args, probe.allow_shell, probe.model, probe.values)
        _add_var_args(args, vars)
        _add_env_args(args, env)
        args.add("-out", out)
        ctx.actions.run(
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
            fail("duplicate parser validation name %q" % name)
        seen_names[name] = True
        kbuilds.append((name, _single_file(target, "kbuilds")))
    for name, file in sorted(kbuilds):
        out = ctx.actions.declare_file("%s.%s.kbuild.json" % (ctx.label.name, _sanitize_output_fragment(name)))
        args = ctx.actions.args()
        args.add("-kbuild", file)
        if ctx.attr.kbuild_recursive:
            args.add("-kbuild_recursive")
            args.add("-kbuild_srctree", source_root)
            _add_var_args(args, vars)
        args.add("-kbuild_out", out)
        inputs = [file]
        if ctx.attr.kbuild_recursive:
            inputs = [file, ctx.file.source_root] + ctx.files.srcs
        ctx.actions.run(
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
        "probe_model": attr.string(
            default = "linux_llvm",
            doc = "Hermetic Linux Kconfig probe model used when allow_shell is set. Set empty to use only the explicit shell environment.",
        ),
        "probe_config": attr.label(
            providers = [LinuxProbeInfo],
            doc = "Optional provider carrying Linux probe values derived from Bazel configuration/toolchain wrapper targets.",
        ),
        "probe_values": attr.string_dict(
            doc = "Overrides for the selected Linux Kconfig probe model, for example cc_version or pahole_version.",
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
    vars = dict(ctx.attr.vars)
    if "srctree" not in vars:
        vars["srctree"] = source_root

    out = ctx.actions.declare_file(ctx.label.name + ".kbuild_tree.json")
    args = ctx.actions.args()
    args.add("-kbuild_tree_root", source_root)
    args.add("-kbuild_tree_out", out)
    if ctx.attr.min_files:
        args.add("-kbuild_tree_min_count", ctx.attr.min_files)
    _add_var_args(args, vars)
    for exclude in ctx.attr.excludes:
        args.add("-kbuild_tree_exclude", exclude)

    ctx.actions.run(
        executable = ctx.executable._kconfig_parse,
        inputs = depset([ctx.file.source_root] + ctx.files.srcs),
        outputs = [out],
        arguments = [args],
        mnemonic = "LinuxKbuildTreeParseValidation",
        progress_message = "Validating all Linux Kbuild/Makefile files under %s" % source_root,
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
        configs = {},
        config = None,
        config_name = "",
        srcs = [],
        object_label_package = None,
        source_label_package = "",
        source_asn1_compiler = "",
        source_config = "",
        source_root_label = "",
        source_tree_labels = [],
        linux_objects_load = "@linux.bzl//internal:linux_objects.bzl",
        generated_headers = "",
        kbuild_tree = False,
        out_buildfile = None,
        out_metadata = None,
        generated_visibility = ["//visibility:public"],
        allow_shell = False,
        probe_model = "linux_llvm",
        probe_config = None,
        probe_values = {},
        vars = {},
        env = {},
        workspace_path_prefix = "",
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
        config = config,
        config_name = config_name,
        configs = configs,
        env = env,
        generated_visibility = generated_visibility,
        generated_headers = generated_headers,
        kbuild = kbuild,
        kbuild_tree = kbuild_tree,
        linux_objects_load = linux_objects_load,
        object_label_package = object_label_package,
        probe_config = probe_config,
        probe_model = probe_model,
        probe_values = probe_values,
        root = root,
        source_label_package = source_label_package,
        source_asn1_compiler = source_asn1_compiler,
        source_config = source_config,
        source_root_label = source_root_label,
        source_tree_labels = source_tree_labels,
        srcs = srcs,
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
        target_compatible_with = target_compatible_with,
        visibility = visibility,
    )
    native.filegroup(
        name = metadata,
        output_group = "metadata",
        srcs = [":" + generated],
        target_compatible_with = target_compatible_with,
        visibility = visibility,
    )

    update_files = {}
    if out_buildfile != None:
        update_files[":" + buildfile] = _workspace_source_path(out_buildfile)
        diff_test(
            name = name + "_buildfile_test",
            file1 = ":" + buildfile,
            file2 = _source_file_label(out_buildfile),
            target_compatible_with = target_compatible_with,
            tags = tags,
        )
    if out_metadata != None:
        update_files[":" + metadata] = _workspace_source_path(out_metadata)
        diff_test(
            name = name + "_metadata_test",
            file1 = ":" + metadata,
            file2 = _source_file_label(out_metadata),
            target_compatible_with = target_compatible_with,
            tags = tags,
        )
    if update_files:
        _linux_compact_update_source_files(
            name = name,
            files = update_files,
            target_compatible_with = target_compatible_with,
            tags = tags,
            visibility = visibility,
            workspace_path_prefix = workspace_path_prefix,
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
        probe_model = "linux_llvm",
        probe_config = None,
        probe_values = {},
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
        probe_model = probe_model,
        probe_config = probe_config,
        probe_values = probe_values,
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

def _workspace_source_path(path):
    package = native.package_name()
    if package:
        return package + "/" + path
    return path

def _join_package(parent, child):
    if not parent:
        return child
    if not child:
        return parent
    return parent + "/" + child

def _sh_quote(value):
    return "'" + value.replace("'", "'\\''") + "'"

def _config_file(target, attr_name):
    if KconfigInfo in target:
        return target[KconfigInfo].config
    return _single_file(target, attr_name)

def _add_var_args(args, vars):
    for key, value in sorted(vars.items()):
        args.add("-var", "%s=%s" % (key, value))

def _add_env_args(args, env):
    for key, value in sorted(env.items()):
        args.add("-env", "%s=%s" % (key, value))

def _probe_settings(ctx, env):
    if ctx.attr.probe_config:
        info = ctx.attr.probe_config[LinuxProbeInfo]
        for key, value in info.env.items():
            env.setdefault(key, value)
        _configure_probe_env(info.allow_shell, env)
        return struct(
            allow_shell = info.allow_shell,
            model = info.model,
            values = info.values,
        )

    _configure_probe_env(ctx.attr.allow_shell, env)
    return struct(
        allow_shell = ctx.attr.allow_shell,
        model = ctx.attr.probe_model,
        values = ctx.attr.probe_values,
    )

def _configure_probe_env(allow_shell, env):
    if not allow_shell:
        return
    if "CC" not in env:
        env["CC"] = "clang"
    if "LD" not in env:
        env["LD"] = "ld.lld"
    if "CLANG_FLAGS" not in env:
        env["CLANG_FLAGS"] = "-fintegrated-as"
    if "RUSTC" not in env:
        env["RUSTC"] = "rustc"
    if "PAHOLE" not in env:
        env["PAHOLE"] = "pahole"
    if "BINDGEN" not in env:
        env["BINDGEN"] = "bindgen"

def _add_probe_args(args, allow_shell, probe_model, probe_values):
    if not allow_shell:
        return
    args.add("-allow_shell")
    if probe_model:
        args.add("-linux_probe_model", probe_model)
    for key, value in sorted(probe_values.items()):
        args.add("-linux_probe_value", "%s=%s" % (key, value))

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
    if file.path.startswith(prefix):
        return file.path[len(prefix):]
    return file.path
