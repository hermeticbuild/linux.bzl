"""Analysis test proving external modules consume only their kernel SDK tools."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("@rules_bison//bison:toolchain_type.bzl", "BISON_TOOLCHAIN_TYPE")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_flex//flex:toolchain_type.bzl", "FLEX_TOOLCHAIN_TYPE")
load("@rules_m4//m4:toolchain_type.bzl", "M4_TOOLCHAIN_TYPE")
load("//internal:execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")
load("//internal:host_cc_toolchain.bzl", "host_cc_toolchain", "host_cc_toolchain_attr")
load("//internal:linux_modules.bzl", "linux_cc_module", "linux_test_dependency_symvers_kbuild_lines")
load("//internal:mapped_kernel.bzl", "linux_kbuild_toolset")
load("//internal:platform_transition_gateway.bzl", "linux_platform_transition")
load("//internal:providers.bzl", "LinuxModuleInfo", "LinuxModuleSdkInfo")
load(
    "//internal:rust_toolchain.bzl",
    "RUST_TOOLCHAIN_TYPE",
)
load("//internal:script_runtime_toolchain.bzl", "SCRIPT_RUNTIME_TOOLCHAIN_TYPE")

visibility("private")

_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE = str(Label("@rules_python//python:exec_tools_toolchain_type"))
_PERL_TOOLCHAIN_TYPE = str(Label("@rules_perl//perl:toolchain_type"))
_PLAN_STAGES = ["prehost", "bootstrap", "host", "prep", "target"]
_TEST_LIBELF_COMPILE_FLAGS = ["-I__LINUX_BZL_HOST_DEPS__/external/libelf/include"]
_TEST_LIBELF_LINK_FLAGS = ["-L__LINUX_BZL_HOST_DEPS__/external/libelf/lib", "-lelf"]
_TEST_SDK_MAKE_VARS = {"EXTERNAL_SHARED_TEST": "configured-kernel-value"}
_TEST_SHARED_VARS = [
    "EXTERNAL_SHARED_TEST=configured-kernel-value",
    "LIBELF_FLAGS=" + " ".join(_TEST_LIBELF_COMPILE_FLAGS),
    "LIBELF_LIBS=" + " ".join(_TEST_LIBELF_LINK_FLAGS),
]
_DIVERGENT_PRIMARY_EXECUTION_PLATFORM = Label("//internal/tests:linux_external_module_test_primary_execution_platform")
_DIVERGENT_RUST_EXECUTION_PLATFORM = Label("//internal/tests:linux_external_module_test_rust_execution_platform")
_DIVERGENT_RUST_TOOLCHAIN = Label("//internal/tests:linux_external_module_test_rust_only_toolchain")
_PERL_MISSING_EXECUTION_PLATFORM = Label("//internal/tests:linux_external_module_test_perl_missing_execution_platform")
_PERL_PRESENT_EXECUTION_PLATFORM = Label("//internal/tests:linux_external_module_test_perl_present_execution_platform")
_EXTERNAL_MODULE_PLATFORM_TOOLCHAIN_TYPES = [
    BISON_TOOLCHAIN_TYPE,
    CC_TOOLCHAIN_TYPE,
    FLEX_TOOLCHAIN_TYPE,
    M4_TOOLCHAIN_TYPE,
    _PERL_TOOLCHAIN_TYPE,
    _PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE,
    SCRIPT_RUNTIME_TOOLCHAIN_TYPE,
]

def _dependency_symvers_kbuild_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "override KBUILD_EXTRA_SYMBOLS := $(KBUILD_EXTRA_SYMBOLS) $(src)/.linux-bzl-dependencies/00000000.symvers",
            "override KBUILD_EXTRA_SYMBOLS := $(KBUILD_EXTRA_SYMBOLS) $(src)/.linux-bzl-dependencies/00000001.symvers",
        ],
        linux_test_dependency_symvers_kbuild_lines(2),
    )
    return unittest.end(env)

_dependency_symvers_kbuild_test = unittest.make(_dependency_symvers_kbuild_test_impl)

def _flag_values(argv, flag):
    return [argv[index + 1] for index in range(len(argv) - 1) if argv[index] == flag]

def _action_path_names_artifact(value, artifact):
    # Analysis tests see path-mapped argv under bazel-out/cfg while Artifact.path
    # retains its configuration-specific directory. The repository-relative
    # suffix is the stable identity shared by both views.
    return value == artifact.path or value.endswith("/" + artifact.short_path)

def _probe_runner_fixture_impl(ctx):
    executable = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.write(
        executable,
        "#!/bin/sh\nexit 1\n",
        is_executable = True,
    )
    return [DefaultInfo(executable = executable)]

_probe_runner_fixture = rule(
    implementation = _probe_runner_fixture_impl,
    executable = True,
)

def _fake_optional_rust_toolchain_impl(_ctx):
    return [platform_common.ToolchainInfo()]

_fake_optional_rust_toolchain = rule(
    implementation = _fake_optional_rust_toolchain_impl,
)

def _divergent_execution_platform_fixtures(name):
    # The first pair differs only by direct Rust-toolchain availability; the
    # second differs only by Perl availability. Candidate order makes the SDK's
    # mapped-kernel-shaped anchors the oracle for the external rule.
    setting = name + "_execution_platform_slot"
    primary_slot = name + "_primary_execution_slot"
    rust_slot = name + "_rust_execution_slot"
    perl_missing_slot = name + "_perl_missing_execution_slot"
    perl_present_slot = name + "_perl_present_execution_slot"
    primary_platform = name + "_primary_execution_platform"
    rust_platform = name + "_rust_execution_platform"
    perl_missing_platform = name + "_perl_missing_execution_platform"
    perl_present_platform = name + "_perl_present_execution_platform"
    rust_toolchain_impl = name + "_rust_only_toolchain_impl"
    rust_toolchain = name + "_rust_only_toolchain"
    native.constraint_setting(name = setting)
    native.constraint_value(
        name = primary_slot,
        constraint_setting = ":" + setting,
    )
    native.constraint_value(
        name = rust_slot,
        constraint_setting = ":" + setting,
    )
    native.constraint_value(
        name = perl_missing_slot,
        constraint_setting = ":" + setting,
    )
    native.constraint_value(
        name = perl_present_slot,
        constraint_setting = ":" + setting,
    )
    native.platform(
        name = primary_platform,
        allowed_toolchain_types = _EXTERNAL_MODULE_PLATFORM_TOOLCHAIN_TYPES,
        check_toolchain_types = True,
        constraint_values = [
            ":" + primary_slot,
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
    )
    native.platform(
        name = rust_platform,
        allowed_toolchain_types = _EXTERNAL_MODULE_PLATFORM_TOOLCHAIN_TYPES + [RUST_TOOLCHAIN_TYPE],
        check_toolchain_types = True,
        constraint_values = [
            ":" + rust_slot,
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
    )
    native.platform(
        name = perl_missing_platform,
        allowed_toolchain_types = [
            toolchain_type
            for toolchain_type in _EXTERNAL_MODULE_PLATFORM_TOOLCHAIN_TYPES + [RUST_TOOLCHAIN_TYPE]
            if toolchain_type != _PERL_TOOLCHAIN_TYPE
        ],
        check_toolchain_types = True,
        constraint_values = [
            ":" + perl_missing_slot,
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
    )
    native.platform(
        name = perl_present_platform,
        allowed_toolchain_types = _EXTERNAL_MODULE_PLATFORM_TOOLCHAIN_TYPES + [RUST_TOOLCHAIN_TYPE],
        check_toolchain_types = True,
        constraint_values = [
            ":" + perl_present_slot,
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
    )
    _fake_optional_rust_toolchain(name = rust_toolchain_impl)
    native.toolchain(
        name = rust_toolchain,
        exec_compatible_with = [":" + rust_slot],
        toolchain = ":" + rust_toolchain_impl,
        toolchain_type = RUST_TOOLCHAIN_TYPE,
    )

def _fake_sdk_impl(ctx):
    target_cc = find_cpp_toolchain(ctx)
    host_cc = host_cc_toolchain(ctx)
    target_contract = linux_kbuild_toolset(ctx, target_cc, "target")
    host_contract = linux_kbuild_toolset(ctx, host_cc, "host")
    marker = ctx.actions.declare_file(ctx.label.name + ".sdk-tool")
    source_root = ctx.actions.declare_file(ctx.label.name + ".Kconfig")
    kbuild = ctx.actions.declare_file(ctx.label.name + ".Makefile")
    config = ctx.actions.declare_file(ctx.label.name + ".config")
    auto_conf = ctx.actions.declare_file(ctx.label.name + ".auto.conf")
    auto_conf_cmd = ctx.actions.declare_file(ctx.label.name + ".auto.conf.cmd")
    autoconf = ctx.actions.declare_file(ctx.label.name + ".autoconf.h")
    rustc_cfg = ctx.actions.declare_file(ctx.label.name + ".rustc_cfg")
    kernel_release = ctx.actions.declare_file(ctx.label.name + ".kernel.release")
    sdk = ctx.actions.declare_directory(ctx.label.name + ".sdk")
    host_toolset_identity = ctx.actions.declare_directory(ctx.label.name + ".host-toolset")
    target_toolset_identity = ctx.actions.declare_directory(ctx.label.name + ".target-toolset")
    host_toolset_manifest = ctx.actions.declare_file(ctx.label.name + ".host-toolset.json")
    target_toolset_manifest = ctx.actions.declare_file(ctx.label.name + ".target-toolset.json")
    host_probe_results = ctx.actions.declare_directory(ctx.label.name + ".host-probes")
    target_probe_results = ctx.actions.declare_directory(ctx.label.name + ".target-probes")
    host_kconfig_probe_results = ctx.actions.declare_directory(ctx.label.name + ".host-kconfig-probes")
    target_kconfig_probe_results = ctx.actions.declare_directory(ctx.label.name + ".target-kconfig-probes")
    host_deps = ctx.actions.declare_directory(ctx.label.name + ".host-deps")
    rust_source_files = ctx.actions.declare_directory(ctx.label.name + ".rust-source")
    for file in [
        marker,
        source_root,
        kbuild,
        config,
        auto_conf,
        auto_conf_cmd,
        autoconf,
        rustc_cfg,
        kernel_release,
        host_toolset_manifest,
        target_toolset_manifest,
    ]:
        ctx.actions.write(file, "fixture\n")
    ctx.actions.run_shell(
        outputs = [
            sdk,
            host_toolset_identity,
            target_toolset_identity,
            host_probe_results,
            target_probe_results,
            host_kconfig_probe_results,
            target_kconfig_probe_results,
            host_deps,
            rust_source_files,
        ],
        command = "for output in \"$@\"; do mkdir -p \"$output\"; done",
        arguments = [
            sdk.path,
            host_toolset_identity.path,
            target_toolset_identity.path,
            host_probe_results.path,
            target_probe_results.path,
            host_kconfig_probe_results.path,
            target_kconfig_probe_results.path,
            host_deps.path,
            rust_source_files.path,
        ],
    )
    auxiliary_roles = ["actionfile", "pahole", "python3", "scriptrun", "script-runtime"]
    target_tools = dict(target_contract.tools)
    host_tools = dict(host_contract.tools)
    target_arguments = dict(target_contract.arguments)
    host_arguments = dict(host_contract.arguments)
    target_environments = dict(target_contract.environments)
    host_environments = dict(host_contract.environments)
    target_requirements = dict(target_contract.requirements_by_role)
    host_requirements = dict(host_contract.requirements_by_role)
    for role in auxiliary_roles:
        target_tools[role] = marker
        host_tools[role] = marker
        target_arguments[role] = []
        host_arguments[role] = []
        target_environments[role] = {}
        host_environments[role] = {}
        target_requirements[role] = {}
        host_requirements[role] = {}
    return [
        DefaultInfo(files = depset([marker])),
        LinuxModuleSdkInfo(
            auto_conf = auto_conf,
            auto_conf_cmd = auto_conf_cmd,
            autoconf = autoconf,
            config = config,
            host_action_args = host_arguments,
            host_action_environments = host_environments,
            host_action_requirements = host_requirements,
            host_companion_tools = {},
            host_deps = host_deps,
            host_execution_platform = linux_execution_platform_label(ctx.attr._host_execution_platform),
            host_kconfig_probe_results = host_kconfig_probe_results,
            host_probe_results = host_probe_results,
            host_probe_runner = ctx.attr.host_probe_runner[DefaultInfo].files_to_run,
            host_recipe_runner = ctx.attr.host_recipe_runner[DefaultInfo].files_to_run,
            host_tool_files = host_tools,
            host_toolchain_files = depset([marker], transitive = [host_cc.all_files]),
            host_toolset_identity = host_toolset_identity,
            host_toolset_manifest = host_toolset_manifest,
            kbuild = kbuild,
            kernel_key = "sdk-fixture",
            kernel_release = kernel_release,
            libelf_compile_flags = _TEST_LIBELF_COMPILE_FLAGS,
            libelf_link_flags = _TEST_LIBELF_LINK_FLAGS,
            make_vars = ctx.attr.make_vars,
            rust_source_files = depset([rust_source_files]),
            rust_source_root = rust_source_files.path,
            rustc_cfg = rustc_cfg,
            sdk = sdk,
            source = depset([source_root]),
            source_root = source_root,
            target_action_args = target_arguments,
            target_action_environments = target_environments,
            target_action_requirements = target_requirements,
            target_companion_tools = {},
            target_execution_platform = (
                ctx.attr.execution_platform_override.label if ctx.attr.execution_platform_override != None else linux_execution_platform_label(ctx.attr._target_execution_platform)
            ),
            target_kconfig_probe_results = target_kconfig_probe_results,
            target_probe_results = target_probe_results,
            target_probe_runner = ctx.attr.target_probe_runner[DefaultInfo].files_to_run,
            target_recipe_runner = ctx.attr.target_recipe_runner[DefaultInfo].files_to_run,
            target_tool_files = target_tools,
            target_toolchain_files = depset([marker], transitive = [target_cc.all_files]),
            target_toolset_identity = target_toolset_identity,
            target_toolset_manifest = target_toolset_manifest,
            version = "6.18.0",
        ),
    ]

_fake_sdk = rule(
    implementation = _fake_sdk_impl,
    attrs = {
        "execution_platform_override": attr.label(),
        "host_probe_runner": attr.label(cfg = config.exec(exec_group = "host_cc"), executable = True, mandatory = True),
        "host_recipe_runner": attr.label(cfg = config.exec(exec_group = "host_cc"), executable = True, mandatory = True),
        "make_vars": attr.string_dict(),
        "target_probe_runner": attr.label(cfg = "exec", executable = True, mandatory = True),
        "target_recipe_runner": attr.label(cfg = "exec", executable = True, mandatory = True),
        "_host_cc_toolchain": host_cc_toolchain_attr(exec_group = "host_cc"),
        "_host_execution_platform": linux_execution_platform_attr(exec_group = "host_cc"),
        "_target_execution_platform": linux_execution_platform_attr(),
    },
    exec_groups = {"host_cc": exec_group(toolchains = use_cc_toolchain() + [
        BISON_TOOLCHAIN_TYPE,
        FLEX_TOOLCHAIN_TYPE,
        M4_TOOLCHAIN_TYPE,
        _PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE,
        _PERL_TOOLCHAIN_TYPE,
        SCRIPT_RUNTIME_TOOLCHAIN_TYPE,
    ])},
    fragments = ["cpp"],
    toolchains = use_cc_toolchain() + [_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE, _PERL_TOOLCHAIN_TYPE, SCRIPT_RUNTIME_TOOLCHAIN_TYPE],
)

def _fake_kernel(name, **kwargs):
    """Models the generated kernel's platform-transitioned public facade."""
    graph = name + "__mapped"
    tags = kwargs.get("tags")
    _fake_sdk(
        name = graph,
        **kwargs
    )
    linux_platform_transition(
        name = name,
        graph = ":" + graph,
        platform = "@platforms//host",
        tags = tags,
    )

def _external_module_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    asserts.true(env, LinuxModuleInfo in target)
    asserts.true(env, OutputGroupInfo in target)
    asserts.equals(env, "sdk-fixture", target[LinuxModuleInfo].kernel_key)
    asserts.equals(env, [ctx.attr.expected_output], [file.basename for file in target[DefaultInfo].files.to_list()])
    actions = analysistest.target_actions(env)
    planner_actions = [action for action in actions if action.mnemonic == "LinuxExternalModulePlan"]
    asserts.equals(env, 1, len(planner_actions))
    probe_planner_actions = [action for action in actions if action.mnemonic == "LinuxExternalKbuildProbePlan"]
    asserts.equals(env, 1, len(probe_planner_actions))

    # analysistest does not expose map_directory's ActionTemplate or tool
    # dictionary here. Exact runner provenance is asserted on the SDK provider
    # and exercised by the executable E2E builds.
    expected_root = "__LINUX_BZL_SOURCE_TREE__/.linux-bzl/external/" + ctx.attr.expected_module_name
    expected_kbuild_vars = ["M=" + expected_root] + ctx.attr.expected_kbuild_vars
    for action in probe_planner_actions + planner_actions:
        shared_vars = _flag_values(action.argv, "-var")
        kbuild_vars = _flag_values(action.argv, "-kbuild_var")
        rust_source_vars = [value for value in shared_vars if value.startswith("RUST_LIB_SRC=")]
        asserts.equals(env, 1, len(rust_source_vars))
        asserts.equals(env, sorted(ctx.attr.expected_shared_vars + rust_source_vars), sorted(shared_vars))
        asserts.equals(env, sorted(expected_kbuild_vars), sorted(kbuild_vars))
    if planner_actions:
        planner = planner_actions[0]
        stage_outputs = _flag_values(planner.argv, "-action_plan_stage_out")
        asserts.equals(env, 5, len(stage_outputs))
        plan_files = {}
        for stage in _PLAN_STAGES:
            matches = [
                file
                for file in planner.outputs.to_list()
                if file.basename.endswith(".external-plan-v4-" + stage)
            ]
            asserts.equals(env, 1, len(matches))
            if matches:
                plan_files[stage] = matches[0]
                values = [value for value in stage_outputs if value.startswith(stage + "=")]
                asserts.equals(env, 1, len(values))
                asserts.true(
                    env,
                    len(values) == 1 and _action_path_names_artifact(values[0][len(stage) + 1:], matches[0]),
                    "the %s external stage flag must name its exact planner TreeArtifact" % stage,
                )
        if OutputGroupInfo in target:
            asserts.equals(
                env,
                sorted([file.path for file in plan_files.values()]),
                sorted([file.path for file in target[OutputGroupInfo].plan.to_list()]),
                "the external plan output group must expose exactly the five planner shards",
            )
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-object_root"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-source_namespace"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-selected_products_only"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "modules"]))
        for prefix in ["PYTHON3=", "RUSTC=", "RUSTC_OR_CLIPPY=", "BINDGEN="]:
            asserts.equals(env, 0, len([arg for arg in planner.argv if arg.startswith(prefix)]))
    source_actions = [action for action in actions if action.mnemonic == "LinuxExternalModuleSources"]
    asserts.equals(env, 1, len(source_actions))
    if source_actions:
        expected_kbuild = ".linux-bzl/external/" + ctx.attr.expected_module_name + "/Kbuild="
        asserts.equals(env, 1, len([arg for arg in source_actions[0].argv if arg.startswith(expected_kbuild)]))
    return analysistest.end(env)

_EXTERNAL_MODULE_TEST_ATTRS = {
    "expected_kbuild_vars": attr.string_list(),
    "expected_module_name": attr.string(mandatory = True),
    "expected_output": attr.string(mandatory = True),
    "expected_shared_vars": attr.string_list(),
}

_external_module_test = analysistest.make(
    _external_module_test_impl,
    attrs = _EXTERNAL_MODULE_TEST_ATTRS,
    config_settings = {
        # External map actions intentionally resolve an execution platform
        # through the same target/host C++ toolchain types as their SDK.
        "//command_line_option:platforms": str(Label("@platforms//host")),
    },
)

_external_module_rust_skew_execution_platform_test = analysistest.make(
    _external_module_test_impl,
    attrs = _EXTERNAL_MODULE_TEST_ATTRS,
    config_settings = {
        "//command_line_option:extra_execution_platforms": [
            str(_DIVERGENT_PRIMARY_EXECUTION_PLATFORM),
            str(_DIVERGENT_RUST_EXECUTION_PLATFORM),
        ],
        "//command_line_option:extra_toolchains": [str(_DIVERGENT_RUST_TOOLCHAIN)],
        "//command_line_option:host_platform": str(Label("@platforms//host")),
        "//command_line_option:platforms": str(Label("@platforms//host")),
    },
)

_external_module_perl_execution_platform_test = analysistest.make(
    _external_module_test_impl,
    attrs = _EXTERNAL_MODULE_TEST_ATTRS,
    config_settings = {
        "//command_line_option:extra_execution_platforms": [
            str(_PERL_MISSING_EXECUTION_PLATFORM),
            str(_PERL_PRESENT_EXECUTION_PLATFORM),
        ],
        "//command_line_option:host_platform": str(Label("@platforms//host")),
        "//command_line_option:platforms": str(Label("@platforms//host")),
    },
)

def _reserved_module_make_vars_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "cannot set backend-owned module Make variables: LIBELF_FLAGS, M")
    return analysistest.end(env)

_reserved_module_make_vars_test = analysistest.make(
    _reserved_module_make_vars_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@platforms//host")),
    },
    expect_failure = True,
)

def _execution_platform_mismatch_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "target actions resolved execution platform")
    return analysistest.end(env)

_execution_platform_mismatch_test = analysistest.make(
    _execution_platform_mismatch_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@platforms//host")),
    },
    expect_failure = True,
)

def linux_external_module_test(name):
    _dependency_symvers_kbuild_test(name = name + "_dependency_symvers_kbuild")
    _divergent_execution_platform_fixtures(name)
    host_probe_runner = name + "_sdk_host_probe_runner"
    host_recipe_runner = name + "_sdk_host_recipe_runner"
    target_probe_runner = name + "_sdk_target_probe_runner"
    target_recipe_runner = name + "_sdk_target_recipe_runner"
    _probe_runner_fixture(
        name = host_probe_runner,
        tags = ["manual"],
    )
    _probe_runner_fixture(
        name = host_recipe_runner,
        tags = ["manual"],
    )
    _probe_runner_fixture(
        name = target_probe_runner,
        tags = ["manual"],
    )
    _probe_runner_fixture(
        name = target_recipe_runner,
        tags = ["manual"],
    )
    sdk = name + "_sdk"
    module = name + "_module"
    _fake_kernel(
        name = sdk,
        host_probe_runner = ":" + host_probe_runner,
        host_recipe_runner = ":" + host_recipe_runner,
        make_vars = _TEST_SDK_MAKE_VARS,
        target_probe_runner = ":" + target_probe_runner,
        target_recipe_runner = ":" + target_recipe_runner,
        tags = ["manual"],
    )
    linux_cc_module(
        name = module,
        kernel = ":" + sdk,
        copts = ["-DEXTERNAL_PHASE_TEST=1"],
        srcs = [
            "external_module.c",
            "external_module_second.c",
        ],
        tags = ["manual"],
    )
    _external_module_test(
        name = name,
        expected_kbuild_vars = ["LINUX_BZL_EXTERNAL_CFLAG_00000000=-DEXTERNAL_PHASE_TEST=1"],
        expected_module_name = module,
        expected_output = module + ".ko",
        expected_shared_vars = _TEST_SHARED_VARS,
        target_under_test = ":" + module,
    )
    _external_module_rust_skew_execution_platform_test(
        name = name + "_rust_skew_execution_platform",
        expected_kbuild_vars = ["LINUX_BZL_EXTERNAL_CFLAG_00000000=-DEXTERNAL_PHASE_TEST=1"],
        expected_module_name = module,
        expected_output = module + ".ko",
        expected_shared_vars = _TEST_SHARED_VARS,
        target_under_test = ":" + module,
    )
    _external_module_perl_execution_platform_test(
        name = name + "_perl_execution_platform",
        expected_kbuild_vars = ["LINUX_BZL_EXTERNAL_CFLAG_00000000=-DEXTERNAL_PHASE_TEST=1"],
        expected_module_name = module,
        expected_output = module + ".ko",
        expected_shared_vars = _TEST_SHARED_VARS,
        target_under_test = ":" + module,
    )
    normalized_module = "9Mixed-" + name
    linux_cc_module(
        name = normalized_module,
        kernel = ":" + sdk,
        srcs = ["external_module.c"],
        tags = ["manual"],
    )
    _external_module_test(
        name = name + "_module_name_normalization",
        expected_module_name = "_9Mixed_" + name,
        expected_output = normalized_module + ".ko",
        expected_shared_vars = _TEST_SHARED_VARS,
        target_under_test = ":" + normalized_module,
    )

    bad_sdk = name + "_bad_sdk"
    bad_module = name + "_bad_module"
    _fake_kernel(
        name = bad_sdk,
        host_probe_runner = ":" + host_probe_runner,
        host_recipe_runner = ":" + host_recipe_runner,
        make_vars = {
            "LIBELF_FLAGS": "-I/escape",
            "M": "/escape",
        },
        target_probe_runner = ":" + target_probe_runner,
        target_recipe_runner = ":" + target_recipe_runner,
        tags = ["manual"],
    )
    linux_cc_module(
        name = bad_module,
        kernel = ":" + bad_sdk,
        srcs = ["external_module.c"],
        tags = ["manual"],
    )
    _reserved_module_make_vars_test(
        name = name + "_reserved_make_vars",
        target_under_test = ":" + bad_module,
    )

    mismatch_sdk = name + "_mismatch_sdk"
    mismatch_module = name + "_mismatch_module"
    _fake_kernel(
        name = mismatch_sdk,
        execution_platform_override = "//internal:execution_platform",
        host_probe_runner = ":" + host_probe_runner,
        host_recipe_runner = ":" + host_recipe_runner,
        target_probe_runner = ":" + target_probe_runner,
        target_recipe_runner = ":" + target_recipe_runner,
        tags = ["manual"],
    )
    linux_cc_module(
        name = mismatch_module,
        kernel = ":" + mismatch_sdk,
        srcs = ["external_module.c"],
        tags = ["manual"],
    )
    _execution_platform_mismatch_test(
        name = name + "_execution_platform_mismatch",
        target_under_test = ":" + mismatch_module,
    )
