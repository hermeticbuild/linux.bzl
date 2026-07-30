"""Toolchain graph-profile recording and configured action context."""

load(
    "@rules_cc//cc:action_names.bzl",
    "CPP_LINK_STATIC_LIBRARY_ACTION_NAME",
    "C_COMPILE_ACTION_NAME",
    "OBJ_COPY_ACTION_NAME",
)
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(
    ":path_mapping.bzl",
    "add_directory_arg",
    "add_mapped_values",
    "directory_anchor",
    "path_mapped_run",
)

visibility("public")

_SOURCE_SENTINEL = "__LINUX_BZL_GRAPH_PROFILE_SOURCE__.c"
_OUTPUT_SENTINEL = "__LINUX_BZL_GRAPH_PROFILE_OUTPUT__.o"
_KBUILD_FLAGS_SENTINEL = "__LINUX_BZL_KBUILD_FLAGS_v1__"
_SH_TOOLCHAIN_TYPE = "@bazel_tools//tools/sh:toolchain_type"
_COREUTILS_TOOLCHAIN_TYPE = "@bazel_lib//lib:coreutils_toolchain_type"

LinuxGraphProfileInfo = provider(
    doc = "Validated graph projection and mutable command template for lazy Linux actions.",
    fields = {
        "arch": "Canonical profile architecture: x86_64 or aarch64.",
        "cc_toolchain": "Selected CcToolchainInfo.",
        "command_template": "Execution-generated canonical mutable compiler command template.",
        "compiler": "Selected compiler executable as a File.",
        "environment": "Deterministic compiler environment captured from the selected toolchain.",
        "execution_requirements": "Execution requirements for the selected C compile action.",
        "feature_configuration": "Configured C toolchain features used to derive the template.",
        "identity": "Execution-generated compiler identity.",
        "kbuild_linker": "Explicit raw linker executable used by Kbuild link actions and flag probes.",
        "kbuild_flags_sentinel": "Unique mutable-argv insertion token for resolved Kbuild flags.",
        "projection": "Repository-emitted GraphProjection used to validate the configured toolchain.",
        "source_inputs": "Exact source files that graph-profile command identities may read.",
        "toolchain_files": "All files required by the selected C toolchain.",
        "validation": "Stamp proving that the selected compiler identity matches the projection.",
    },
)

def _compiler_family(cc_toolchain):
    compiler = cc_toolchain.compiler.lower()
    if "clang" in compiler:
        return "clang"
    if compiler == "gcc" or "gcc" in compiler:
        return "gcc"
    fail(
        "Linux graph profiles require a clang or gcc C toolchain, got compiler %r" %
        cc_toolchain.compiler,
    )

def _matching_toolchain_files(cc_toolchain, tool_path):
    return [
        file
        for file in cc_toolchain.all_files.to_list()
        if file.path == tool_path
    ]

def _toolchain_file(cc_toolchain, tool_name, tool_path):
    matches = _matching_toolchain_files(cc_toolchain, tool_path)
    if len(matches) != 1:
        fail(
            "selected C toolchain %s %r must resolve to exactly one declared toolchain file, got %d" %
            (tool_name, tool_path, len(matches)),
        )
    return matches[0]

def _nm_toolchain_file(cc_toolchain, objcopy):
    if cc_toolchain.nm_executable:
        # Bazel 8 may synthesize a legacy nm path even when no such tool is declared.
        matches = _matching_toolchain_files(
            cc_toolchain,
            cc_toolchain.nm_executable,
        )
        if len(matches) == 1:
            return matches[0]
        if len(matches) > 1:
            fail(
                "selected C toolchain nm %r must resolve to at most one declared toolchain file, got %d" %
                (cc_toolchain.nm_executable, len(matches)),
            )
    suffix = ".exe" if objcopy.basename.endswith(".exe") else ""
    stem = objcopy.basename.removesuffix(suffix)
    if not stem.endswith("objcopy"):
        fail(
            (
                "selected C toolchain nm_executable %r does not resolve to a declared file and objcopy %r cannot " +
                "provide the conventional fallback; expected a basename ending in objcopy%s"
            ) % (cc_toolchain.nm_executable, objcopy.path, suffix),
        )
    nm_basename = stem.removesuffix("objcopy") + "nm" + suffix
    matches = [
        file
        for file in cc_toolchain.all_files.to_list()
        if file.dirname == objcopy.dirname and file.basename == nm_basename
    ]
    if len(matches) != 1:
        fail(
            (
                "selected C toolchain nm_executable %r does not resolve to a declared file and fallback nm %r, " +
                "derived from objcopy %r, must resolve to exactly one declared toolchain file, got %d"
            ) % (cc_toolchain.nm_executable, nm_basename, objcopy.path, len(matches)),
        )
    return matches[0]

def _graph_profile_tool_files(cc_toolchain, feature_configuration):
    archiver = _toolchain_file(
        cc_toolchain,
        "archiver",
        cc_common.get_tool_for_action(
            feature_configuration = feature_configuration,
            action_name = CPP_LINK_STATIC_LIBRARY_ACTION_NAME,
        ),
    )
    objcopy = _toolchain_file(
        cc_toolchain,
        "objcopy",
        cc_common.get_tool_for_action(
            feature_configuration = feature_configuration,
            action_name = OBJ_COPY_ACTION_NAME,
        ),
    )
    return struct(
        archiver = archiver,
        nm = _nm_toolchain_file(cc_toolchain, objcopy),
        objcopy = objcopy,
    )

def _toolchain_directory_anchors(files):
    anchors = {}
    for file in files:
        parts = file.dirname.split("/")
        for length in range(len(parts), 0, -1):
            directory = "/".join(parts[:length])
            if not directory or directory == ".":
                continue
            if directory not in anchors:
                anchors[directory] = directory_anchor(file, directory)
    return anchors

def _execution_requirements(feature_configuration):
    return {
        requirement: ""
        for requirement in cc_common.get_execution_requirements(
            feature_configuration = feature_configuration,
            action_name = C_COMPILE_ACTION_NAME,
        )
    }

def _inspect_args(
        ctx,
        analysis_compiler,
        analysis_target,
        compiler,
        compiler_argv,
        environment,
        path_mapping_files,
        directory_anchors,
        command_template,
        identity):
    args = ctx.actions.args()
    args.add("inspect")
    args.add("-architecture=" + ctx.attr.arch)
    args.add("-analysis_compiler=" + analysis_compiler)
    args.add("-analysis_target_gnu_system_name=" + analysis_target)
    args.add(compiler, format = "-compiler=%s")
    args.add("-source_sentinel=" + _SOURCE_SENTINEL)
    args.add("-output_sentinel=" + _OUTPUT_SENTINEL)
    args.add("-kbuild_flags_sentinel=" + _KBUILD_FLAGS_SENTINEL)
    add_mapped_values(
        args,
        ["-compile_arg=" + value for value in compiler_argv],
        files = path_mapping_files,
        directory_anchors = directory_anchors,
    )
    add_mapped_values(
        args,
        [
            "-compile_env=%s=%s" % (name, environment[name])
            for name in sorted(environment.keys())
        ],
        files = path_mapping_files,
        directory_anchors = directory_anchors,
    )
    args.add(command_template, format = "-template_out=%s")
    args.add(identity, format = "-identity_out=%s")
    return args

def _validate_args(
        ctx,
        graph,
        source_root,
        linker,
        archiver,
        nm,
        objcopy,
        coreutils,
        shell,
        command_template,
        identity,
        validation):
    args = ctx.actions.args()
    args.add("validate-graph")
    args.add(graph, format = "-profile=%s")
    args.add(identity, format = "-identity=%s")
    args.add(command_template, format = "-template=%s")
    args.add(linker, format = "-linker=%s")
    args.add(archiver, format = "-archiver=%s")
    args.add(nm, format = "-nm=%s")
    args.add(objcopy, format = "-objcopy=%s")
    args.add(coreutils, format = "-coreutils=%s")
    args.add(ctx.executable._ccprofile, format = "-grep=%s")
    args.add("-shell=" + shell)
    add_directory_arg(
        args,
        directory_anchor(source_root),
        format = "-source_root=%s",
    )
    args.add(validation, format = "-out=%s")
    return args

def _refresh_args(
        ctx,
        recorded,
        source_root,
        linker,
        archiver,
        nm,
        objcopy,
        coreutils,
        shell,
        command_template,
        identity,
        output):
    args = ctx.actions.args()
    args.add("refresh-graph")
    args.add(recorded, format = "-profile=%s")
    args.add(identity, format = "-identity=%s")
    args.add(command_template, format = "-template=%s")
    args.add(linker, format = "-linker=%s")
    args.add(archiver, format = "-archiver=%s")
    args.add(nm, format = "-nm=%s")
    args.add(objcopy, format = "-objcopy=%s")
    args.add(coreutils, format = "-coreutils=%s")
    args.add(ctx.executable._ccprofile, format = "-grep=%s")
    args.add("-shell=" + shell)
    add_directory_arg(
        args,
        directory_anchor(source_root),
        format = "-source_root=%s",
    )
    args.add(output, format = "-out=%s")
    return args

def _record_args(
        ctx,
        root,
        configs,
        metadata,
        output,
        compiler,
        seed = None,
        command_template = None,
        archiver = None,
        nm = None,
        objcopy = None,
        coreutils = None,
        shell = None):
    args = ctx.actions.args()
    args.add("-root", root)
    add_directory_arg(
        args,
        directory_anchor(root),
        format = "-srctree=%s",
    )
    args.add("-kbuild", ctx.file.kbuild)
    args.add("-compact_kbuild_tree")
    args.add("-compact_metadata_out", metadata)
    args.add("-compile_environment_abi", ctx.attr.compile_environment_abi)
    args.add("-config_mode", ctx.attr.config_mode)
    args.add("-kernel_version", ctx.attr.kernel_version)
    args.add("-allow_shell")
    args.add("-graph_profile_record_out", output)
    if seed:
        args.add("-graph_profile", seed)
        args.add("-graph_profile_template", command_template)
        args.add("-graph_profile_linker", ctx.file.kbuild_linker)
        args.add(archiver, format = "-graph_profile_archiver=%s")
        args.add(nm, format = "-graph_profile_nm=%s")
        args.add(objcopy, format = "-graph_profile_objcopy=%s")
        args.add("-graph_profile_coreutils", coreutils)
        args.add("-graph_profile_grep", ctx.executable._ccprofile)
        args.add("-graph_profile_shell=" + shell)
    else:
        args.add("-linux_probe_model", ctx.attr.probe_model)
        args.add("-graph_profile_architecture", ctx.attr.arch)
        args.add("-graph_profile_compiler", compiler)
        args.add(
            "-graph_profile_target_gnu_system_name",
            find_cpp_toolchain(ctx).target_gnu_system_name,
        )

    variables = dict(ctx.attr.vars)
    if "srctree" not in variables:
        args.add("-var")
        add_directory_arg(
            args,
            directory_anchor(root),
            format = "srctree=%s",
        )
    for key, value in sorted(variables.items()):
        args.add("-var", "%s=%s" % (key, value))
    for key, value in sorted(ctx.attr.env.items()):
        args.add("-env", "%s=%s" % (key, value))
    if not seed:
        for key, value in sorted(ctx.attr.probe_values.items()):
            args.add("-linux_probe_value", "%s=%s" % (key, value))

    for name, file in sorted(configs):
        args.add("-config")
        args.add(file, format = name + "=%s")
    for name, label in sorted(ctx.attr.generated_headers_by_config.items()):
        args.add("-generated_headers_for_config", "%s=%s" % (name, label))
    return args

def _linux_graph_profile_context_impl(ctx):
    cc_toolchain = find_cpp_toolchain(ctx)
    coreutils = ctx.toolchains[_COREUTILS_TOOLCHAIN_TYPE].coreutils_info.bin
    shell = ctx.toolchains[_SH_TOOLCHAIN_TYPE].path
    feature_configuration = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    variables = cc_common.create_compile_variables(
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
        source_file = _SOURCE_SENTINEL,
        output_file = _OUTPUT_SENTINEL,
        user_compile_flags = (
            ctx.fragments.cpp.copts +
            ctx.fragments.cpp.conlyopts +
            [_KBUILD_FLAGS_SENTINEL]
        ),
    )
    compiler_argv = cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    )
    environment = dict(cc_common.get_environment_variables(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    ))
    environment.setdefault("LANG", "C")
    environment.setdefault("LC_ALL", "C")
    environment.setdefault("TZ", "UTC")
    execution_requirements = _execution_requirements(feature_configuration)

    compiler_path = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    tools = _graph_profile_tool_files(cc_toolchain, feature_configuration)
    archiver = tools.archiver
    compiler = _toolchain_file(cc_toolchain, "C compiler", compiler_path)
    nm = tools.nm
    objcopy = tools.objcopy
    analysis_compiler = _compiler_family(cc_toolchain)
    analysis_target = cc_toolchain.target_gnu_system_name
    toolchain_files = cc_toolchain.all_files.to_list()
    path_mapping_files = [file for file in toolchain_files if not file.is_source]
    directory_anchors = _toolchain_directory_anchors(path_mapping_files)

    command_template = ctx.actions.declare_file(ctx.label.name + ".command_template.json")
    identity = ctx.actions.declare_file(ctx.label.name + ".compiler_identity.json")
    validation = ctx.actions.declare_file(ctx.label.name + ".graph_profile.validated")

    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        arguments = [_inspect_args(
            ctx,
            analysis_compiler,
            analysis_target,
            compiler,
            compiler_argv,
            environment,
            path_mapping_files,
            directory_anchors,
            command_template,
            identity,
        )],
        inputs = cc_toolchain.all_files,
        outputs = [command_template, identity],
        execution_requirements = execution_requirements,
        mnemonic = "LinuxGraphProfileInspect",
        progress_message = "Inspecting C compiler graph profile for %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        arguments = [_validate_args(
            ctx,
            ctx.file.graph_projection,
            ctx.file.source_root,
            ctx.file.kbuild_linker,
            archiver,
            nm,
            objcopy,
            coreutils,
            shell,
            command_template,
            identity,
            validation,
        )],
        inputs = depset(
            direct = [
                archiver,
                command_template,
                coreutils,
                ctx.file.graph_projection,
                ctx.file.kbuild_linker,
                ctx.file.source_root,
                identity,
                nm,
                objcopy,
            ] + ctx.files.srcs,
            transitive = [cc_toolchain.all_files],
        ),
        outputs = [validation],
        execution_requirements = execution_requirements,
        mnemonic = "LinuxGraphProfileValidate",
        progress_message = "Validating C compiler graph profile for %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    return [
        DefaultInfo(files = depset([command_template, identity, validation])),
        LinuxGraphProfileInfo(
            arch = ctx.attr.arch,
            cc_toolchain = cc_toolchain,
            command_template = command_template,
            compiler = compiler,
            environment = environment,
            execution_requirements = execution_requirements,
            feature_configuration = feature_configuration,
            identity = identity,
            kbuild_linker = ctx.file.kbuild_linker,
            kbuild_flags_sentinel = _KBUILD_FLAGS_SENTINEL,
            projection = ctx.file.graph_projection,
            source_inputs = depset(ctx.files.srcs),
            toolchain_files = cc_toolchain.all_files,
            validation = validation,
        ),
    ]

linux_graph_profile_context = rule(
    implementation = _linux_graph_profile_context_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["aarch64", "x86_64"],
            doc = "Canonical architecture encoded by the checked-in profile.",
        ),
        "graph_projection": attr.label(
            allow_single_file = [".json"],
            mandatory = True,
            doc = "Repository-emitted GraphProjection for the selected compact graph.",
        ),
        "kbuild_linker": attr.label(
            allow_single_file = True,
            cfg = "exec",
            mandatory = True,
            doc = "Explicit raw linker executable used by Kbuild.",
        ),
        "source_root": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "A file in the Linux source root used to replay graph-shaping probes.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Linux source inputs potentially consumed by graph-shaping probes.",
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
    },
    doc = "Captures and validates the selected C toolchain used by lazy kernel actions.",
    fragments = ["cpp"],
    toolchains = use_cc_toolchain() + [
        _COREUTILS_TOOLCHAIN_TYPE,
        _SH_TOOLCHAIN_TYPE,
    ],
)

def _linux_graph_profile_impl(ctx):
    cc_toolchain = find_cpp_toolchain(ctx)
    coreutils = ctx.toolchains[_COREUTILS_TOOLCHAIN_TYPE].coreutils_info.bin
    shell = ctx.toolchains[_SH_TOOLCHAIN_TYPE].path
    compiler = _compiler_family(cc_toolchain)
    root = ctx.file.root
    recorded = ctx.actions.declare_file(ctx.label.name + ".recorded.json")
    refreshed = ctx.actions.declare_file(ctx.label.name + ".refreshed.json")
    output = ctx.actions.declare_file(ctx.label.name + ".json")
    bootstrap_metadata = ctx.actions.declare_file(ctx.label.name + ".recorded.metadata.json")
    metadata = ctx.actions.declare_file(ctx.label.name + ".metadata.json")

    configs = []
    config_names = {}
    inputs = [root, ctx.file.kbuild] + ctx.files.srcs
    for target, name in ctx.attr.configs.items():
        if not name:
            fail("config names must be non-empty")
        if name in config_names:
            fail("duplicate config name %r" % name)
        files = target.files.to_list()
        if len(files) != 1:
            fail("configs target %s must provide exactly one file" % target.label)
        config_names[name] = True
        configs.append((name, files[0]))
        inputs.append(files[0])
    if not configs:
        fail("configs must contain at least one configuration")
    if sorted(ctx.attr.generated_headers_by_config.keys()) != sorted(config_names.keys()):
        fail(
            "generated_headers_by_config keys %s do not match config names %s" %
            (sorted(ctx.attr.generated_headers_by_config.keys()), sorted(config_names.keys())),
        )

    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._kconfig_parse,
        inputs = depset(inputs),
        outputs = [bootstrap_metadata, recorded],
        arguments = [_record_args(
            ctx,
            root,
            configs,
            bootstrap_metadata,
            recorded,
            compiler,
        )],
        mnemonic = "LinuxGraphProfileSeed",
        progress_message = "Seeding Linux toolchain graph profile for %{label}",
    )

    feature_configuration = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )
    variables = cc_common.create_compile_variables(
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
        source_file = _SOURCE_SENTINEL,
        output_file = _OUTPUT_SENTINEL,
        user_compile_flags = (
            ctx.fragments.cpp.copts +
            ctx.fragments.cpp.conlyopts +
            [_KBUILD_FLAGS_SENTINEL]
        ),
    )
    compiler_argv = cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    )
    environment = dict(cc_common.get_environment_variables(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = variables,
    ))
    environment.setdefault("LANG", "C")
    environment.setdefault("LC_ALL", "C")
    environment.setdefault("TZ", "UTC")
    execution_requirements = _execution_requirements(feature_configuration)
    compiler_path = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    tools = _graph_profile_tool_files(cc_toolchain, feature_configuration)
    archiver = tools.archiver
    compiler_file = _toolchain_file(cc_toolchain, "C compiler", compiler_path)
    nm = tools.nm
    objcopy = tools.objcopy
    toolchain_files = cc_toolchain.all_files.to_list()
    path_mapping_files = [file for file in toolchain_files if not file.is_source]
    directory_anchors = _toolchain_directory_anchors(path_mapping_files)
    command_template = ctx.actions.declare_file(ctx.label.name + ".command_template.json")
    identity = ctx.actions.declare_file(ctx.label.name + ".compiler_identity.json")
    validation = ctx.actions.declare_file(ctx.label.name + ".graph_profile.validated")

    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        arguments = [_inspect_args(
            ctx,
            compiler,
            cc_toolchain.target_gnu_system_name,
            compiler_file,
            compiler_argv,
            environment,
            path_mapping_files,
            directory_anchors,
            command_template,
            identity,
        )],
        inputs = cc_toolchain.all_files,
        outputs = [command_template, identity],
        execution_requirements = execution_requirements,
        mnemonic = "LinuxGraphProfileInspect",
        progress_message = "Inspecting C compiler graph profile for %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        arguments = [_refresh_args(
            ctx,
            recorded,
            root,
            ctx.file.kbuild_linker,
            archiver,
            nm,
            objcopy,
            coreutils,
            shell,
            command_template,
            identity,
            refreshed,
        )],
        inputs = depset(
            direct = [
                archiver,
                command_template,
                coreutils,
                ctx.file.kbuild_linker,
                identity,
                nm,
                objcopy,
                recorded,
                root,
            ] + ctx.files.srcs,
            transitive = [cc_toolchain.all_files],
        ),
        outputs = [refreshed],
        execution_requirements = execution_requirements,
        mnemonic = "LinuxGraphProfileRefresh",
        progress_message = "Refreshing Linux graph-profile probes for %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._kconfig_parse,
        arguments = [_record_args(
            ctx,
            root,
            configs,
            metadata,
            output,
            compiler,
            seed = refreshed,
            command_template = command_template,
            archiver = archiver,
            nm = nm,
            objcopy = objcopy,
            coreutils = coreutils,
            shell = shell,
        )],
        inputs = depset(
            direct = [
                archiver,
                command_template,
                coreutils,
                ctx.executable._ccprofile,
                ctx.file.kbuild_linker,
                identity,
                nm,
                objcopy,
                refreshed,
            ] + inputs,
            transitive = [cc_toolchain.all_files],
        ),
        outputs = [metadata, output],
        execution_requirements = execution_requirements,
        mnemonic = "LinuxGraphProfileExtend",
        progress_message = "Closing Linux graph profile for %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        arguments = [_validate_args(
            ctx,
            output,
            root,
            ctx.file.kbuild_linker,
            archiver,
            nm,
            objcopy,
            coreutils,
            shell,
            command_template,
            identity,
            validation,
        )],
        inputs = depset(
            direct = [
                archiver,
                command_template,
                coreutils,
                ctx.file.kbuild,
                ctx.file.kbuild_linker,
                identity,
                nm,
                objcopy,
                output,
                root,
            ] + ctx.files.srcs,
            transitive = [cc_toolchain.all_files],
        ),
        outputs = [validation],
        execution_requirements = execution_requirements,
        mnemonic = "LinuxGraphProfileValidate",
        progress_message = "Replaying Linux graph-profile probes for %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )

    return [
        DefaultInfo(files = depset([output, validation])),
        OutputGroupInfo(
            metadata = depset([metadata]),
            profile = depset([output]),
            validation = depset([validation]),
        ),
    ]

linux_graph_profile = rule(
    implementation = _linux_graph_profile_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["aarch64", "x86_64"],
            doc = "Canonical architecture recorded in the graph profile.",
        ),
        "compile_environment_abi": attr.string(
            mandatory = True,
            doc = "Toolchain/action ABI bound into the recorded compact graph.",
        ),
        "config_mode": attr.string(
            default = "default",
            values = ["allnoconfig", "default"],
            doc = "Kconfig baseline used while resolving every recorded config.",
        ),
        "configs": attr.label_keyed_string_dict(
            allow_files = True,
            mandatory = True,
            doc = "Map of config file labels to unique compact config names.",
        ),
        "env": attr.string_dict(
            doc = "Hermetic Kconfig preprocessor environment values.",
        ),
        "generated_headers_by_config": attr.string_dict(
            mandatory = True,
            doc = "Map of config names to generated-header labels.",
        ),
        "kbuild": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Root Kbuild file traversed recursively for graph-shaping probes.",
        ),
        "kbuild_linker": attr.label(
            allow_single_file = True,
            cfg = "exec",
            mandatory = True,
            doc = "Raw linker used to replay every recorded Kbuild graph probe.",
        ),
        "kernel_version": attr.string(
            mandatory = True,
            doc = "Kernel release used to materialize indexed config payloads.",
        ),
        "probe_model": attr.string(
            default = "linux_llvm",
            doc = "Hermetic Linux Kconfig probe model used to record exact command results.",
        ),
        "probe_values": attr.string_dict(
            doc = "Overrides for the selected Linux Kconfig probe model.",
        ),
        "root": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Root Kconfig file for this kernel source tree.",
        ),
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Additional Kconfig files reachable from roots.",
        ),
        "vars": attr.string_dict(
            doc = "Kconfig preprocessor variables.",
        ),
        "_kconfig_parse": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/kconfig_parse:kconfig_parse"),
            executable = True,
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
    },
    doc = "Records and toolchain-validates a complete Kconfig/Kbuild graph profile for maintenance.",
    fragments = ["cpp"],
    toolchains = use_cc_toolchain() + [
        _COREUTILS_TOOLCHAIN_TYPE,
        _SH_TOOLCHAIN_TYPE,
    ],
)
