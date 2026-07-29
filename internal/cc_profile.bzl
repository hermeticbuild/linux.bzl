"""Active C toolchain profile context for lazy Linux compile actions."""

load("@rules_cc//cc:action_names.bzl", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":path_mapping.bzl", "add_mapped_values", "directory_anchor", "path_mapped_run")

visibility("public")

_SOURCE_SENTINEL = "__LINUX_BZL_CC_PROFILE_SOURCE__.c"
_OUTPUT_SENTINEL = "__LINUX_BZL_CC_PROFILE_OUTPUT__.o"
_KBUILD_FLAGS_SENTINEL = "__LINUX_BZL_KBUILD_FLAGS_v1__"

LinuxCcProfileInfo = provider(
    doc = "Validated compiler identity and mutable command template for lazy Linux compile actions.",
    fields = {
        "arch": "Canonical profile architecture: x86_64 or aarch64.",
        "cc_toolchain": "Selected CcToolchainInfo.",
        "command_template": "Execution-generated canonical mutable compiler command template.",
        "compiler": "Selected compiler executable as a File.",
        "environment": "Deterministic compiler environment captured from the selected toolchain.",
        "execution_requirements": "Execution requirements for the selected C compile action.",
        "feature_configuration": "Configured C toolchain features used to derive the template.",
        "identity": "Execution-generated compiler identity.",
        "kbuild_flags_sentinel": "Unique mutable-argv insertion token for resolved Kbuild flags.",
        "profile": "Checked-in expected CC capability profile.",
        "structural_probes": "Optional repository-resolved structural-probe result manifest.",
        "toolchain_files": "All files required by the selected C toolchain.",
        "validation": "Stamp proving that the selected compiler identity matches the profile.",
    },
)

def _compiler_family(cc_toolchain):
    compiler = cc_toolchain.compiler.lower()
    if "clang" in compiler:
        return "clang"
    if compiler == "gcc" or "gcc" in compiler:
        return "gcc"
    fail(
        "linux_cc_profile_context requires a clang or gcc C toolchain, got compiler %r" %
        cc_toolchain.compiler,
    )

def _compiler_file(cc_toolchain, compiler_path):
    matches = [
        file
        for file in cc_toolchain.all_files.to_list()
        if file.path == compiler_path
    ]
    if len(matches) != 1:
        fail(
            "selected C compiler %r must resolve to exactly one declared toolchain file, got %d" %
            (compiler_path, len(matches)),
        )
    return matches[0]

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

def _validate_args(ctx, identity, validation):
    args = ctx.actions.args()
    args.add("validate")
    args.add(ctx.file.profile, format = "-profile=%s")
    args.add(identity, format = "-identity=%s")
    args.add(validation, format = "-out=%s")
    return args

def _linux_cc_profile_context_impl(ctx):
    if ctx.attr.arch not in ["aarch64", "x86_64"]:
        fail("linux_cc_profile_context arch must be aarch64 or x86_64, got %r" % ctx.attr.arch)

    cc_toolchain = find_cpp_toolchain(ctx)
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
    compiler = _compiler_file(cc_toolchain, compiler_path)
    analysis_compiler = _compiler_family(cc_toolchain)
    analysis_target = cc_toolchain.target_gnu_system_name
    toolchain_files = cc_toolchain.all_files.to_list()
    path_mapping_files = [file for file in toolchain_files if not file.is_source]
    directory_anchors = _toolchain_directory_anchors(path_mapping_files)

    command_template = ctx.actions.declare_file(ctx.label.name + ".cc_command_template.json")
    identity = ctx.actions.declare_file(ctx.label.name + ".cc_identity.json")
    validation = ctx.actions.declare_file(ctx.label.name + ".cc_profile.validated")

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
        mnemonic = "LinuxCcProfileInspect",
        progress_message = "Inspecting C compiler profile for %{label}",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    path_mapped_run(
        ctx.actions,
        executable = ctx.executable._ccprofile,
        arguments = [_validate_args(ctx, identity, validation)],
        inputs = [ctx.file.profile, identity],
        outputs = [validation],
        mnemonic = "LinuxCcProfileValidate",
        progress_message = "Validating C compiler profile for %{label}",
    )

    return [
        DefaultInfo(files = depset([command_template, identity, validation])),
        LinuxCcProfileInfo(
            arch = ctx.attr.arch,
            cc_toolchain = cc_toolchain,
            command_template = command_template,
            compiler = compiler,
            environment = environment,
            execution_requirements = execution_requirements,
            feature_configuration = feature_configuration,
            identity = identity,
            kbuild_flags_sentinel = _KBUILD_FLAGS_SENTINEL,
            profile = ctx.file.profile,
            structural_probes = ctx.file.structural_probes,
            toolchain_files = cc_toolchain.all_files,
            validation = validation,
        ),
    ]

linux_cc_profile_context = rule(
    implementation = _linux_cc_profile_context_impl,
    attrs = {
        "arch": attr.string(
            mandatory = True,
            values = ["aarch64", "x86_64"],
            doc = "Canonical architecture encoded by the checked-in profile.",
        ),
        "profile": attr.label(
            allow_single_file = [".json"],
            mandatory = True,
            doc = "Checked-in CC capability profile.",
        ),
        "structural_probes": attr.label(
            allow_single_file = [".json"],
            doc = "Optional repository-resolved structural-probe result manifest.",
        ),
        "_ccprofile": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/ccprofile"),
            executable = True,
        ),
    },
    doc = "Captures and validates the selected C toolchain used by lazy kernel compile actions.",
    fragments = ["cpp"],
    toolchains = use_cc_toolchain(),
)
