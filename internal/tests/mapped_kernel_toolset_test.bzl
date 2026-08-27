"""Tests for mapped-kernel toolsets and recipe-opaque input binding."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load(
    "//internal:mapped_kernel.bzl",
    "linux_map_directory_tools",
    "linux_test_canonical_file_path",
    "linux_test_canonical_rust_source_root",
    "linux_test_canonicalize_toolchain_action_value",
    "linux_test_companion_tool_bindings",
    "linux_test_composed_tree_base_paths",
    "linux_test_declare_working_output",
    "linux_test_decode_packed_node_inputs",
    "linux_test_execution_root_marker",
    "linux_test_host_dependency_compile_flags",
    "linux_test_host_dependency_library_search_flags",
    "linux_test_host_library_artifact",
    "linux_test_kbuild_action_name",
    "linux_test_merge_action_arguments",
    "linux_test_node_action_contract_role",
    "linux_test_node_input_artifact_tree_roots",
    "linux_test_node_source_closure_keys",
    "linux_test_parse_plan_marker_paths",
    "linux_test_render_toolchain_action_value",
    "linux_test_resolve_node_input_bindings",
    "linux_test_rewrite_host_dependency_link_flag",
    "linux_test_runtime_tool_bindings",
    "linux_test_selected_prior_outputs",
    "linux_test_source_input_namespace_names",
    "linux_test_tool_file",
    "linux_test_toolset_execution_requirements",
    "linux_test_tree_input_directory_name",
    "linux_test_validate_host_dependency_artifact",
    "linux_test_with_compile_action_arguments",
    "linux_test_with_link_runtime_arguments",
    "linux_test_with_rust_toolchain",
)
load("//internal:probe_map_directory.bzl", "linux_probe_map_directory_tools")
load("//internal:providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo")

visibility("private")

_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE = str(Label("@rules_python//python:exec_tools_toolchain_type"))
_EXEC_PYTHON_INTERPRETER = "mapped_kernel_exec_python_interpreter.sh"
_PLAN_STAGES = ["prehost", "bootstrap", "host", "prep", "target"]

def _exec_python_interpreter_impl(ctx):
    interpreter = ctx.actions.declare_file(_EXEC_PYTHON_INTERPRETER)
    ctx.actions.write(
        output = interpreter,
        content = "#!/bin/sh\n",
        is_executable = True,
    )
    runtime = struct(
        files = depset([interpreter]),
        interpreter = interpreter,
    )
    return [
        DefaultInfo(
            executable = interpreter,
            files = depset([interpreter]),
        ),
        platform_common.ToolchainInfo(py3_runtime = runtime),
    ]

_exec_python_interpreter = rule(
    implementation = _exec_python_interpreter_impl,
    executable = True,
)

def _exec_python_toolchain_impl(ctx):
    return [platform_common.ToolchainInfo(
        exec_tools = struct(exec_interpreter = ctx.attr.interpreter),
    )]

_exec_python_toolchain = rule(
    implementation = _exec_python_toolchain_impl,
    attrs = {
        "interpreter": attr.label(
            cfg = "exec",
            mandatory = True,
        ),
    },
)

def _flag_values(argv, flag):
    return [argv[index + 1] for index in range(len(argv) - 1) if argv[index] == flag]

def _action_path_names_artifact(value, artifact):
    # Analysis tests see path-mapped argv under bazel-out/cfg while Artifact.path
    # retains its configuration-specific directory. The repository-relative
    # suffix is the stable identity shared by both views.
    return value == artifact.path or value.endswith("/" + artifact.short_path)

def _assert_manifest_derived_rust(env, action):
    variables = _flag_values(action.argv, "-var")
    asserts.equals(env, 0, len([arg for arg in action.argv if arg == "-rust_toolchain_contract"]))
    asserts.equals(env, 1, len([value for value in variables if value.startswith("RUST_LIB_SRC=")]))
    for prefix in ["BINDGEN=", "HOSTRUSTC=", "RUSTC=", "RUSTC_OR_CLIPPY=", "RUSTC_VERSION_TEXT="]:
        asserts.equals(
            env,
            0,
            len([value for value in variables if value.startswith(prefix)]),
            "%s must come from the identity-bound toolset manifest or an execution-time probe" % prefix[:-1],
        )

def _assert_selected_rust_sources_are_inputs(env, action):
    roots = [
        value[len("RUST_LIB_SRC="):]
        for value in _flag_values(action.argv, "-var")
        if value.startswith("RUST_LIB_SRC=")
    ]
    asserts.equals(env, 1, len(roots))
    if roots:
        prefix = roots[0] + "/"
        asserts.true(
            env,
            any([file.path == roots[0] or file.path.startswith(prefix) for file in action.inputs.to_list()]),
            "%s must declare the selected Rust source closure below %s" % (action.mnemonic, roots[0]),
        )

def _fake_declare_file(filename, directory):
    return struct(kind = "file", name = filename, parent = directory)

def _mapped_kernel_toolset_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = analysistest.target_actions(env)
    identity_actions = [action for action in actions if action.mnemonic == "LinuxToolsetIdentity"]
    probe_plan_actions = [action for action in actions if action.mnemonic == "LinuxProbePlan"]
    kconfig_probe_plan_actions = [action for action in actions if action.mnemonic == "LinuxKconfigProbePlan"]
    kbuild_probe_plan_actions = [action for action in actions if action.mnemonic == "LinuxKbuildProbePlan"]
    planner_actions = [action for action in actions if action.mnemonic == "LinuxMappedPlan"]
    prep_base_actions = [action for action in actions if action.mnemonic == "LinuxMappedPrepBase"]
    asserts.equals(env, 2, len(identity_actions))
    asserts.equals(env, 1, len(probe_plan_actions))
    asserts.equals(env, 1, len(kconfig_probe_plan_actions))
    asserts.equals(env, 1, len(kbuild_probe_plan_actions))
    asserts.equals(env, 1, len(planner_actions))
    asserts.equals(env, 1, len(prep_base_actions))
    asserts.true(env, OutputGroupInfo in target)
    asserts.true(env, LinuxKernelInfo in target)
    asserts.true(env, LinuxModuleSdkInfo in target)
    if OutputGroupInfo in target:
        asserts.equals(env, ["analysis_smoke.arch"], [file.basename for file in target[OutputGroupInfo].arch.to_list()])
        asserts.equals(env, 2, len(target[OutputGroupInfo].toolsets.to_list()))
        asserts.equals(env, [
            "analysis_smoke.plan-v4-bootstrap",
            "analysis_smoke.plan-v4-host",
            "analysis_smoke.plan-v4-prehost",
            "analysis_smoke.plan-v4-prep",
            "analysis_smoke.plan-v4-target",
        ], sorted([file.basename for file in target[OutputGroupInfo].plan.to_list()]))
        asserts.equals(env, [
            "analysis_smoke.kbuild-probe-plan",
            "analysis_smoke.kbuild-probe-results-host",
            "analysis_smoke.kbuild-probe-results-target",
            "analysis_smoke.kconfig-probe-plan",
            "analysis_smoke.kconfig-probe-results-host",
            "analysis_smoke.kconfig-probe-results-target",
            "analysis_smoke.probe-plan",
            "analysis_smoke.probe-results-host",
            "analysis_smoke.probe-results-target",
        ], sorted([file.basename for file in target[OutputGroupInfo].probes.to_list()]))
    if probe_plan_actions:
        probe_plan = probe_plan_actions[0]
        asserts.equals(env, 1, len([arg for arg in probe_plan.argv if arg == "-probe_plan_out"]))
        asserts.equals(env, 1, len([arg for arg in probe_plan.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in probe_plan.argv if arg == "-host_toolset_identity"]))
    if kconfig_probe_plan_actions:
        kconfig_probe_plan = kconfig_probe_plan_actions[0]
        _assert_manifest_derived_rust(env, kconfig_probe_plan)
        _assert_selected_rust_sources_are_inputs(env, kconfig_probe_plan)
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-kconfig_probe_plan_out"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-target_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-host_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-host_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-target_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-host_toolset_manifest"]))
        asserts.equals(env, 0, len([value for value in _flag_values(kconfig_probe_plan.argv, "-var") if value.startswith("PYTHON3=")]))
        asserts.equals(env, {}, kconfig_probe_plan.env)
    if kbuild_probe_plan_actions:
        kbuild_probe_plan = kbuild_probe_plan_actions[0]
        _assert_manifest_derived_rust(env, kbuild_probe_plan)
        _assert_selected_rust_sources_are_inputs(env, kbuild_probe_plan)
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-kbuild_probe_plan_out"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_toolset_manifest"]))
        asserts.equals(env, ["all"], _flag_values(kbuild_probe_plan.argv, "-kbuild_target"))
        asserts.equals(env, ["modules_prepare"], _flag_values(kbuild_probe_plan.argv, "-kbuild_prepare_target"))
        asserts.equals(env, 0, len([value for value in _flag_values(kbuild_probe_plan.argv, "-var") if value.startswith("PYTHON3=")]))
        asserts.equals(env, {}, kbuild_probe_plan.env)
    if planner_actions:
        planner = planner_actions[0]
        _assert_manifest_derived_rust(env, planner)
        _assert_selected_rust_sources_are_inputs(env, planner)
        plan_stage_outputs = _flag_values(planner.argv, "-action_plan_stage_out")
        asserts.equals(env, 5, len(plan_stage_outputs))
        plan_files = {}
        for stage in _PLAN_STAGES:
            matches = [
                file
                for file in planner.outputs.to_list()
                if file.basename == "analysis_smoke.plan-v4-" + stage
            ]
            asserts.equals(env, 1, len(matches))
            if matches:
                plan_files[stage] = matches[0]
                values = [value for value in plan_stage_outputs if value.startswith(stage + "=")]
                asserts.equals(env, 1, len(values))
                asserts.true(
                    env,
                    len(values) == 1 and _action_path_names_artifact(values[0][len(stage) + 1:], matches[0]),
                    "the %s stage flag must name its exact planner TreeArtifact" % stage,
                )
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-resolved_arch_out"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_kbuild_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_kbuild_probe_results"]))
        asserts.equals(env, ["all"], _flag_values(planner.argv, "-kbuild_target"))
        asserts.equals(env, ["modules_prepare"], _flag_values(planner.argv, "-kbuild_prepare_target"))
        asserts.equals(env, 0, len([value for value in _flag_values(planner.argv, "-var") if value.startswith("PYTHON3=")]))
        asserts.equals(env, {}, planner.env)
        asserts.equals(env, 1, len([file for file in planner.outputs.to_list() if file.basename == "analysis_smoke.arch"]))
        asserts.equals(env, [
            "analysis_smoke.plan-v4-bootstrap",
            "analysis_smoke.plan-v4-host",
            "analysis_smoke.plan-v4-prehost",
            "analysis_smoke.plan-v4-prep",
            "analysis_smoke.plan-v4-target",
        ], sorted([
            file.basename
            for file in planner.outputs.to_list()
            if file.is_directory and ".plan-v4-" in file.basename
        ]))
        if OutputGroupInfo in target:
            asserts.equals(
                env,
                sorted([file.path for file in plan_files.values()]),
                sorted([file.path for file in target[OutputGroupInfo].plan.to_list()]),
                "the plan output group must expose exactly the five planner shards",
            )
        planner_inputs = planner.inputs.to_list()
        asserts.equals(
            env,
            2,
            len([file for file in planner_inputs if ".toolset-" in file.basename and not file.basename.endswith(".json")]),
        )
        asserts.equals(
            env,
            2,
            len([file for file in planner_inputs if ".toolset-" in file.basename and file.basename.endswith(".json")]),
        )
    if LinuxKernelInfo in target:
        kernel = target[LinuxKernelInfo]
        asserts.equals(env, "File", type(kernel.arch))
        asserts.equals(env, "analysis_smoke.arch", kernel.arch.basename)
    for action in identity_actions:
        asserts.equals(env, 1, len([arg for arg in action.argv if arg == "-manifest"]))
        asserts.equals(env, 1, len([arg for arg in action.argv if arg == "-out"]))
        asserts.true(env, len(action.inputs.to_list()) > 1, "identity action must depend on the complete toolchain closure")
    if LinuxModuleSdkInfo in target:
        sdk = target[LinuxModuleSdkInfo]
        for scope, kind, runner, closure in [
            ("host", "probe", sdk.host_probe_runner, sdk.host_toolchain_files),
            ("host", "recipe", sdk.host_recipe_runner, sdk.host_toolchain_files),
            ("target", "probe", sdk.target_probe_runner, sdk.target_toolchain_files),
            ("target", "recipe", sdk.target_recipe_runner, sdk.target_toolchain_files),
        ]:
            asserts.equals(
                env,
                "FilesToRunProvider",
                type(runner),
                "%s %s runner must retain the exact configured FilesToRunProvider" % (scope, kind),
            )
            asserts.true(env, runner.executable != None, "%s %s runner must be executable" % (scope, kind))
            if runner.executable != None:
                asserts.true(
                    env,
                    runner.executable.path in {file.path: True for file in closure.to_list()},
                    "%s %s runner must remain in the identity-bound toolset closure" % (scope, kind),
                )
        for scope, action_args, closure in [
            ("host", sdk.host_action_args, sdk.host_toolchain_files),
            ("target", sdk.target_action_args, sdk.target_toolchain_files),
        ]:
            for role in ["ar", "as", "cc", "cc-link", "cxx", "cxx-link", "ld", "nm", "objcopy", "objdump", "ranlib", "readelf", "strip"]:
                asserts.equals(
                    env,
                    1,
                    len([arg for arg in action_args[role] if arg == "__LINUX_BZL_KBUILD_ARGS_V1__"]),
                    "%s %s action must expose the insertion sentinel exactly once" % (scope, role),
                )
            for role in ["cc", "cxx"]:
                link_role = role + "-link"
                asserts.equals(
                    env,
                    sdk.host_tool_files[role] if scope == "host" else sdk.target_tool_files[role],
                    sdk.host_tool_files[link_role] if scope == "host" else sdk.target_tool_files[link_role],
                    "%s %s must retain the source-selected compiler executable" % (scope, link_role),
                )
                asserts.true(
                    env,
                    len(action_args[link_role]) > len(action_args[role]),
                    "%s %s must add the configured standard link envelope" % (scope, link_role),
                )
            closure_paths = {file.path: True for file in closure.to_list()}
            runtime_paths = {}
            for role in ["cc-link", "cxx-link"]:
                argv = action_args[role]
                markers = [index for index, arg in enumerate(argv) if arg == "__LINUX_BZL_KBUILD_ARGS_V1__"]
                runtime_paths[role] = sorted([
                    arg
                    for arg in argv[markers[0] + 1:]
                    if arg in closure_paths and arg.endswith(".a")
                ])
                asserts.true(
                    env,
                    len(runtime_paths[role]) > 0,
                    "%s %s must append configured runtime archives after source-selected inputs" % (scope, role),
                )
            asserts.equals(
                env,
                runtime_paths["cc-link"],
                runtime_paths["cxx-link"],
                "%s compiler-driver link roles must share configured runtime archives" % scope,
            )
            for path in runtime_paths["cc-link"]:
                asserts.false(
                    env,
                    path in action_args["cc"] or path in action_args["cxx"],
                    "%s runtime archive must remain link-only" % scope,
                )
        host_dependency_path = sdk.host_deps.path
        for role in ["cc", "cc-link", "cxx", "cxx-link"]:
            dependency_arguments = [
                arg
                for arg in sdk.host_action_args[role]
                if host_dependency_path in arg
            ]
            asserts.true(
                env,
                len(dependency_arguments) > 0,
                "host %s action must carry staged dependency compile defaults" % role,
            )
            asserts.true(
                env,
                any([arg.startswith("-isystem") for arg in dependency_arguments]),
                "host %s action must classify ordinary dependency roots as system headers" % role,
            )
            asserts.false(
                env,
                any([arg.startswith("-I") for arg in dependency_arguments]),
                "host %s action must not expose staged dependency headers to Linux warning policy" % role,
            )
            for argument in dependency_arguments:
                asserts.true(
                    env,
                    argument.startswith("-iquote") or argument.startswith("-isystem"),
                    "host %s dependency root must retain its CcInfo search-path class" % role,
                )
                rendered = linux_test_render_toolchain_action_value(argument, [sdk.host_deps])
                asserts.true(
                    env,
                    rendered.substituted and sdk.host_deps in rendered.fragments,
                    "host %s dependency flag must retain its typed TreeArtifact root" % role,
                )
        asserts.true(
            env,
            any([flag.startswith("-isystem") for flag in sdk.libelf_compile_flags]),
            "module SDK must classify ordinary dependency roots as system headers",
        )
        asserts.false(
            env,
            any([flag.startswith("-I") for flag in sdk.libelf_compile_flags]),
            "module SDK must not expose staged dependency headers to Linux warning policy",
        )
        for role in ["cc", "cc-link", "cxx", "cxx-link"]:
            asserts.false(
                env,
                any([host_dependency_path in arg for arg in sdk.target_action_args[role]]),
                "target %s action must not inherit host dependency compile defaults" % role,
            )
        for role in ["as", "cc", "cxx"]:
            host_args = sdk.host_action_args[role]
            asserts.true(
                env,
                len([arg for arg in host_args if arg == "-isystem"]) >= 2,
                "host %s action must carry toolchain-declared kernel and libc header roots" % role,
            )
            asserts.true(
                env,
                "-target" in sdk.target_action_args[role],
                "target %s action must retain the configured LLVM target" % role,
            )
        for scope, action_args, tool_files in [
            ("host", sdk.host_action_args, sdk.host_tool_files),
            ("target", sdk.target_action_args, sdk.target_tool_files),
        ]:
            for applet_role, basename in [
                ("script-applet-find", "toybox"),
                ("script-applet-perl", "perl"),
            ]:
                asserts.true(env, applet_role in tool_files)
                asserts.equals(env, basename, tool_files[applet_role].basename)
                asserts.equals(
                    env,
                    [],
                    action_args[applet_role],
                    "%s %s override must remain a runtime applet, not an action-role wrapper" % (scope, basename),
                )
        asserts.true(env, "actionfile" in sdk.host_tool_files)
        asserts.true(env, "awk" in sdk.host_tool_files)
        asserts.true(env, "bison" in sdk.host_tool_files)
        asserts.true(env, "flex" in sdk.host_tool_files)
        asserts.true(env, "m4" in sdk.host_tool_files)
        asserts.true(env, "pkg-config" in sdk.host_tool_files)
        asserts.equals(
            env,
            1,
            len([arg for arg in sdk.host_action_args["pkg-config"] if arg == "__LINUX_BZL_KBUILD_ARGS_V1__"]),
            "host pkg-config action must expose one source-argument insertion point",
        )
        pkg_config_contract = sdk.host_action_args["pkg-config"]
        asserts.equals(env, 4, len(pkg_config_contract))
        asserts.equals(env, "-manifest", pkg_config_contract[0])
        asserts.equals(env, "--", pkg_config_contract[2])
        manifest_path = pkg_config_contract[1]
        pkg_config_manifests = [
            file
            for file in sdk.host_toolchain_files.to_list()
            if file.path == manifest_path and file.basename == "analysis_smoke.pkg-config.json"
        ]
        asserts.equals(
            env,
            1,
            len(pkg_config_manifests),
            "host pkg-config manifest argument must name its identity-bound File",
        )
        asserts.false(env, "pkg-config" in sdk.target_tool_files)
        asserts.false(env, "m4-deny-shell" in sdk.host_tool_files)
        asserts.true(env, "script-runtime" in sdk.host_tool_files)
        asserts.true(env, "scriptrun" in sdk.host_tool_files)
        asserts.true(env, "actionfile" in sdk.target_tool_files)
        asserts.true(env, "awk" in sdk.target_tool_files)
        asserts.true(env, "lz4" in sdk.target_tool_files)
        asserts.true(env, "pahole" in sdk.target_tool_files)
        asserts.true(env, "python3" in sdk.target_tool_files)
        asserts.true(env, "script-runtime" in sdk.target_tool_files)
        asserts.true(env, "scriptrun" in sdk.target_tool_files)
        asserts.true(env, "rustc" in sdk.target_tool_files)
        asserts.true(env, "bindgen" in sdk.target_tool_files)
        asserts.true(env, "rustc" in sdk.host_tool_files)
        asserts.true(env, "python3" in sdk.host_tool_files)
        asserts.false(env, "lz4" in sdk.host_tool_files)
        asserts.false(env, "pahole" in sdk.host_tool_files)

        target_closure = {file.path: True for file in sdk.target_toolchain_files.to_list()}
        asserts.true(
            env,
            any([file.basename == "rustc" for file in sdk.target_toolchain_files.to_list()]),
            "target map toolchain closure must contain the selected rustc executable",
        )
        for role in ["awk", "lz4", "pahole"]:
            executable = getattr(sdk.target_tool_files.get(role), "executable", None)
            if executable == None:
                executable = sdk.target_tool_files.get(role)
            asserts.true(env, executable != None)
            if executable != None:
                asserts.true(
                    env,
                    executable.path in target_closure,
                    "%s must be part of the identity-bound target closure" % role,
                )

        host_closure = {file.path: True for file in sdk.host_toolchain_files.to_list()}
        target_closure = {file.path: True for file in sdk.target_toolchain_files.to_list()}
        m4 = sdk.host_tool_files.get("m4")
        m4_executable = getattr(m4, "executable", None)
        bison_companions = sdk.host_companion_tools.get("bison", [])
        flex_companions = sdk.host_companion_tools.get("flex", [])
        m4_companions = sdk.host_companion_tools.get("m4", [])
        asserts.true(env, m4_executable != None)
        asserts.equals(env, 1, len(bison_companions))
        asserts.equals(env, 2, len(flex_companions))
        asserts.equals(env, 1, len(m4_companions))
        for role, companions in [
            ("bison", bison_companions),
            ("flex", flex_companions),
            ("m4", m4_companions),
        ]:
            for companion in companions:
                asserts.equals(env, "FilesToRunProvider", type(companion), "%s companions must retain their typed runfiles provider" % role)
                asserts.true(env, companion.executable.path in host_closure, "%s companion executable must be identity-bound" % role)
        if m4_executable != None and len(bison_companions) == 1 and len(flex_companions) == 2 and len(m4_companions) == 1:
            bison_deny_shell = bison_companions[0]
            flex_deny_shell = flex_companions[0]
            m4_deny_shell = m4_companions[0]
            asserts.equals(env, m4, flex_companions[1])
            asserts.equals(env, bison_deny_shell.executable.path, sdk.host_action_environments["bison"].get("M4_SYSCMD_SHELL"))
            asserts.equals(env, m4_executable.path, sdk.host_action_environments["flex"].get("M4"))
            asserts.equals(env, flex_deny_shell.executable.path, sdk.host_action_environments["flex"].get("M4_SYSCMD_SHELL"))
            asserts.equals(env, m4_deny_shell.executable.path, sdk.host_action_environments["m4"].get("M4_SYSCMD_SHELL"))
            asserts.true(env, m4_executable.path in host_closure)
            mapped_host_tools = linux_map_directory_tools(
                "runner",
                "host",
                sdk.target_tool_files,
                sdk.target_toolchain_files,
                sdk.host_tool_files,
                sdk.host_toolchain_files,
                sdk.target_companion_tools,
                sdk.host_companion_tools,
            )
            asserts.equals(env, bison_companions, linux_test_companion_tool_bindings(mapped_host_tools, "bison", "host"))
            asserts.equals(env, flex_companions, linux_test_companion_tool_bindings(mapped_host_tools, "flex", "host"))
            asserts.equals(env, m4_companions, linux_test_companion_tool_bindings(mapped_host_tools, "m4", "host"))
        planner_input_paths = {
            file.path: True
            for file in planner_actions[0].inputs.to_list()
        } if planner_actions else {}
        kconfig_probe_plan_input_paths = {
            file.path: True
            for file in kconfig_probe_plan_actions[0].inputs.to_list()
        } if kconfig_probe_plan_actions else {}
        kbuild_probe_plan_input_paths = {
            file.path: True
            for file in kbuild_probe_plan_actions[0].inputs.to_list()
        } if kbuild_probe_plan_actions else {}
        for scope, tool_files, closure in [
            ("target", sdk.target_tool_files, target_closure),
            ("host", sdk.host_tool_files, host_closure),
        ]:
            for role in ["ar", "as", "awk", "cc", "cxx", "ld", "nm", "objcopy", "objdump", "ranlib", "readelf", "strip"]:
                executable = getattr(tool_files.get(role), "executable", None)
                if executable == None:
                    executable = tool_files.get(role)
                asserts.true(env, executable != None, "%s %s executable is selected" % (scope, role))
                if executable != None:
                    asserts.true(env, executable.path in closure, "%s %s is identity-bound" % (scope, role))
                    asserts.false(
                        env,
                        executable.path in planner_input_paths,
                        "final planner must not receive selected %s %s executable" % (scope, role),
                    )
                    asserts.false(
                        env,
                        executable.path in kconfig_probe_plan_input_paths,
                        "Kconfig probe discovery must not receive %s %s executable" % (scope, role),
                    )
                    asserts.false(
                        env,
                        executable.path in kbuild_probe_plan_input_paths,
                        "Kbuild probe discovery must not receive %s %s executable" % (scope, role),
                    )
            for role, tool in tool_files.items():
                executable = getattr(tool, "executable", None)
                if executable == None:
                    executable = tool
                asserts.false(
                    env,
                    executable.path in kconfig_probe_plan_input_paths,
                    "Kconfig probe discovery must not receive selected %s %s tool input" % (scope, role),
                )
                asserts.false(
                    env,
                    executable.path in kbuild_probe_plan_input_paths,
                    "Kbuild probe discovery must not receive selected %s %s tool input" % (scope, role),
                )
                asserts.false(
                    env,
                    executable.path in planner_input_paths,
                    "final planner must not receive selected %s %s tool input" % (scope, role),
                )
        for role in ["bindgen", "python3", "rustc"]:
            tool = sdk.target_tool_files.get(role)
            executable = getattr(tool, "executable", None)
            if executable == None:
                executable = tool
            asserts.true(env, executable != None, "target %s probe tool is selected" % role)
            if executable != None:
                asserts.false(
                    env,
                    executable.path in kconfig_probe_plan_input_paths,
                    "Kconfig discovery must not receive selected %s binary" % role,
                )
                asserts.false(
                    env,
                    executable.path in kbuild_probe_plan_input_paths,
                    "Kbuild discovery must not receive selected %s binary" % role,
                )
                asserts.false(
                    env,
                    executable.path in planner_input_paths,
                    "final planner must not receive selected %s binary" % role,
                )
                asserts.true(
                    env,
                    executable.path in target_closure,
                    "Kconfig map toolset closure must receive selected %s binary" % role,
                )
        for name in ["BISON_BAZEL_RUNFILES_M4", "BISON_PKGDATADIR", "M4"]:
            asserts.true(env, name in sdk.host_action_environments["bison"])
        asserts.false(env, "RUSTC_BOOTSTRAP" in sdk.target_action_environments["rustc"])
        asserts.false(env, "RUSTC_BOOTSTRAP" in sdk.host_action_environments["rustc"])
        for scope, rustc_args in [
            ("host", sdk.host_action_args["rustc"]),
            ("target", sdk.target_action_args["rustc"]),
        ]:
            asserts.equals(env, [
                "__LINUX_BZL_KBUILD_ARGS_V1__",
                "-Zunstable-options",
                "-Clink-self-contained=-linker",
            ], rustc_args, "%s rustc action must preserve only the source-selected invocation boundary" % scope)
        asserts.equals(
            env,
            sdk.target_action_args["rustc"],
            sdk.target_action_args["clippy"],
            "clippy must preserve the same external-linker contract as rustc",
        )
        asserts.true(env, "clippy" in sdk.target_tool_files)
        asserts.true(env, "rustdoc" in sdk.target_tool_files)
        asserts.equals(env, sdk.target_action_environments["rustc"], sdk.target_action_environments["clippy"])
        asserts.equals(env, {}, sdk.target_action_environments["rustdoc"])
        asserts.equals(env, {}, sdk.target_action_environments["bindgen"])
    return analysistest.end(env)

_mapped_kernel_toolset_test = analysistest.make(
    _mapped_kernel_toolset_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
    },
)

def mapped_kernel_toolset_test(name):
    _mapped_kernel_toolset_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:analysis_smoke",
    )

def _mapped_kernel_exec_python_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    sdk = target[LinuxModuleSdkInfo]
    for scope, tools, closure in [
        ("target", sdk.target_tool_files, sdk.target_toolchain_files),
        ("host", sdk.host_tool_files, sdk.host_toolchain_files),
    ]:
        interpreter = tools.get("python3")
        asserts.true(env, interpreter != None, "%s toolset must select Python" % scope)
        if interpreter != None:
            asserts.equals(env, _EXEC_PYTHON_INTERPRETER, interpreter.basename)
            asserts.true(
                env,
                interpreter.path in {file.path: True for file in closure.to_list()},
                "%s execution Python must be identity-bound" % scope,
            )

    for mnemonic in ["LinuxKconfigProbePlan", "LinuxKbuildProbePlan", "LinuxMappedPlan"]:
        actions = [action for action in analysistest.target_actions(env) if action.mnemonic == mnemonic]
        asserts.equals(env, 1, len(actions))
        if actions:
            python_vars = [value for value in _flag_values(actions[0].argv, "-var") if value.startswith("PYTHON3=")]
            asserts.equals(
                env,
                0,
                len(python_vars),
                "%s must derive Python only from the identity-bound toolset manifest" % mnemonic,
            )
    identity_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxToolsetIdentity"
    ]
    asserts.equals(env, 2, len(identity_actions))
    asserts.equals(
        env,
        2,
        len([
            action
            for action in identity_actions
            if _EXEC_PYTHON_INTERPRETER in [file.basename for file in action.inputs.to_list()]
        ]),
        "both execution scopes must identity-bind the execution-platform Python closure",
    )
    return analysistest.end(env)

_mapped_kernel_exec_python_test = analysistest.make(
    _mapped_kernel_exec_python_test_impl,
    config_settings = {
        "//command_line_option:extra_toolchains": [
            str(Label("//internal/tests:mapped_kernel_exec_python_test_toolchain")),
        ],
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_arm64")),
    },
)

def mapped_kernel_exec_python_test(name):
    interpreter = name + "_interpreter"
    implementation = name + "_implementation"
    toolchain = name + "_toolchain"
    _exec_python_interpreter(
        name = interpreter,
        tags = ["manual"],
        target_compatible_with = [
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
    )
    _exec_python_toolchain(
        name = implementation,
        interpreter = ":" + interpreter,
        tags = ["manual"],
    )
    native.toolchain(
        name = toolchain,
        exec_compatible_with = [
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
        toolchain = ":" + implementation,
        toolchain_type = _PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE,
    )
    _mapped_kernel_exec_python_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:analysis_smoke",
    )

def _mapped_kernel_backend_test_impl(ctx):
    env = unittest.begin(ctx)
    empty_toolset = struct(
        arguments = {},
        environments = {},
        make_variables = {},
        requirements_by_role = {},
        tools = {},
    )
    rust_environment = {"RUST_TOOLCHAIN_ENV": "selected"}
    rust_tools = linux_test_with_rust_toolchain(
        "target",
        empty_toolset,
        struct(
            clippy_driver = "selected-clippy",
            env = rust_environment,
            rust_doc = "selected-rustdoc",
            rustc = "selected-rustc",
            rustfmt = "selected-rustfmt",
        ),
        struct(bindgen = "selected-bindgen"),
    )
    asserts.equals(env, {
        "BINDGEN": "bindgen",
        "CLIPPY_DRIVER": "clippy",
        "RUSTC": "rustc",
        "RUSTDOC": "rustdoc",
        "RUSTFMT": "rustfmt",
    }, rust_tools.make_variables)
    asserts.false(env, "RUSTC_OR_CLIPPY" in rust_tools.make_variables)
    asserts.equals(env, rust_environment, rust_tools.environments["rustc"])
    asserts.equals(env, rust_environment, rust_tools.environments["clippy"])
    asserts.equals(env, {}, rust_tools.environments["rustdoc"])
    asserts.equals(env, {}, rust_tools.environments["rustfmt"])
    asserts.equals(env, {}, rust_tools.environments["bindgen"])
    asserts.equals(env, [
        "__LINUX_BZL_KBUILD_ARGS_V1__",
        "-Zunstable-options",
        "-Clink-self-contained=-linker",
    ], rust_tools.arguments["rustc"])
    asserts.equals(env, rust_tools.arguments["rustc"], rust_tools.arguments["clippy"])
    for role in ["bindgen", "rustdoc", "rustfmt"]:
        asserts.equals(env, [], rust_tools.arguments[role])
    bindgen_only = linux_test_with_rust_toolchain(
        "target",
        empty_toolset,
        None,
        struct(bindgen = "selected-bindgen"),
    )
    asserts.equals(env, {"BINDGEN": "bindgen"}, bindgen_only.make_variables)
    asserts.equals(env, {"bindgen": {}}, bindgen_only.environments)
    asserts.equals(
        env,
        "external/rust-src/lib/rustlib/src/library",
        linux_test_canonical_rust_source_root(
            "../rust-src",
            "lib/rustlib/src",
            "library",
        ),
    )
    asserts.equals(env, "linux-kbuild-host-cc", linux_test_kbuild_action_name("host", "cc"))
    asserts.equals(env, "linux-kbuild-target-cc", linux_test_kbuild_action_name("target", "cc"))
    indexed_cc = struct(name = "cc", path = "toolchain/bin/cc")
    asserts.equals(env, indexed_cc, linux_test_tool_file(
        [
            struct(name = "ld", path = "toolchain/bin/ld"),
            indexed_cc,
        ],
        indexed_cc.path,
        "cc",
    ))
    asserts.equals(env, [
        "link-prefix",
        "compile-prefix",
        "__LINUX_BZL_KBUILD_ARGS_V1__",
        "compile-suffix",
        "link-suffix",
    ], linux_test_merge_action_arguments(
        ["link-prefix", "__LINUX_BZL_KBUILD_ARGS_V1__", "link-suffix"],
        ["compile-prefix", "__LINUX_BZL_KBUILD_ARGS_V1__", "compile-suffix"],
    ))
    asserts.equals(env, "cc-link", linux_test_node_action_contract_role("link-driver", "cc"))
    asserts.equals(env, "cc", linux_test_node_action_contract_role("compile", "cc"))
    asserts.equals(env, "ld", linux_test_node_action_contract_role("link-driver", "ld"))
    asserts.equals(env, {
        "cc": "configured-cc",
        "ld": "configured-ld",
    }, linux_test_runtime_tool_bindings({
        "cc": struct(executable = "configured-cc"),
        "ld": "configured-ld",
        "runner": "recipe-runner",
        "toolchain_files": depset(),
    }))
    execution_root_marker = linux_test_execution_root_marker()
    sysroot = struct(
        is_directory = True,
        is_source = False,
        path = "bazel-out/k8-opt-exec/bin/external/toolchain/sysroot",
        short_path = "../toolchain/sysroot",
    )
    header = struct(
        is_directory = False,
        is_source = False,
        path = "bazel-out/k8-opt-exec/bin/external/toolchain/tool.h",
        short_path = "../toolchain/tool.h",
    )
    rendered_action_value = linux_test_render_toolchain_action_value(
        "-Iexternal/toolchain/sysroot/include:external/toolchain/tool.h;external/unrelated/tool.h-not-an-artifact",
        [sysroot, header],
    )
    asserts.true(env, rendered_action_value.substituted)
    asserts.equals(env, [
        "-I",
        execution_root_marker + "/",
        sysroot,
        "/include:",
        execution_root_marker + "/",
        header,
        ";external/unrelated/tool.h-not-an-artifact",
    ], rendered_action_value.fragments)
    asserts.equals(
        env,
        "-Iexternal/toolchain/sysroot/include:external/toolchain/tool.h;external/unrelated/tool.h-not-an-artifact",
        linux_test_canonicalize_toolchain_action_value(
            "-Ibazel-out/k8-opt-exec/bin/external/toolchain/sysroot/include:bazel-out/k8-opt-exec/bin/external/toolchain/tool.h;external/unrelated/tool.h-not-an-artifact",
            [sysroot, header],
        ),
    )

    # Prebuilt repositories can expose an opaque source directory as a source
    # File even though Starlark does not mark it as a directory. Its exact
    # artifact still anchors descendants such as Clang's builtin includes.
    opaque_resource_root = struct(
        is_directory = False,
        is_source = True,
        path = "external/toolchain/lib/clang/22",
        short_path = "../toolchain/lib/clang/22",
    )
    rendered_opaque_resource = linux_test_render_toolchain_action_value(
        opaque_resource_root.path + "/include",
        [opaque_resource_root],
    )
    asserts.equals(env, [
        execution_root_marker + "/",
        opaque_resource_root,
        "/include",
    ], rendered_opaque_resource.fragments)

    # Runfiles trees are mapped from their exact executable. Keep the File
    # typed through Args and leave only the Bazel-defined suffix textual.
    bison = struct(
        is_directory = False,
        is_source = False,
        path = "bazel-out/k8-opt-exec/bin/external/bison_repo/bin/bison",
        short_path = "../bison_repo/bin/bison",
    )
    rendered_runfiles_directory = linux_test_render_toolchain_action_value(
        bison.path + ".runfiles/bison_repo/data",
        [bison],
    )
    asserts.equals(env, [
        execution_root_marker + "/",
        bison,
        ".runfiles/bison_repo/data",
    ], rendered_runfiles_directory.fragments)

    # Clang's cc-link action passes its resource directory after a pair of
    # -Xclang options.  The directory is not itself a File, but the selected
    # toolchain closure proves it through the resource headers below it.
    resource_directory = "external/toolchain/lib/clang/22/include"
    resource_header = struct(
        is_directory = False,
        is_source = True,
        path = resource_directory + "/stddef.h",
        short_path = "../toolchain/lib/clang/22/include/stddef.h",
    )
    rendered_cc_link_resource = linux_test_render_toolchain_action_value(
        "-Xclang -internal-isystem -Xclang " + resource_directory,
        [resource_header],
    )
    asserts.equals(env, [
        "-Xclang -internal-isystem -Xclang ",
        execution_root_marker + "/",
        resource_directory,
    ], rendered_cc_link_resource.fragments)

    # Directory discovery is syntax-independent: attached options and
    # delimiter-separated path values use the same closure-derived index.
    rendered_source_directories = linux_test_render_toolchain_action_value(
        "-I%s:--resource-dir=%s;%s-not-an-artifact" % (
            resource_directory,
            resource_directory,
            resource_directory,
        ),
        [resource_header],
    )
    asserts.equals(env, [
        "-I",
        execution_root_marker + "/",
        resource_directory,
        ":--resource-dir=",
        execution_root_marker + "/",
        resource_directory,
        ";" + resource_directory + "-not-an-artifact",
    ], rendered_source_directories.fragments)
    asserts.equals(
        env,
        {"no-remote": "1", "supports-path-mapping": "1"},
        linux_test_toolset_execution_requirements({
            "cc": {"supports-path-mapping": "1"},
            "ld": {"no-remote": "1", "supports-path-mapping": "1"},
        }, "test target"),
    )
    asserts.equals(
        env,
        "external/elfutils+/libcpu/i386.mnemonics",
        linux_test_canonical_file_path(
            "bazel-out/k8-opt-exec/bin/external/elfutils+/libcpu/i386.mnemonics",
            "../elfutils+/libcpu/i386.mnemonics",
        ),
    )
    producer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    other_producer = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    node = {
        "inputs": {
            "payload:00000003": struct(producer = other_producer, slot = "00000001"),
            "subtool:00000000": struct(producer = producer, slot = "00000000"),
        },
    }
    bindings = linux_test_resolve_node_input_bindings(
        "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
        node,
        {
            other_producer + ":00000001": "exact-payload-tree-file",
            producer + ":00000000": "exact-generated-executable-tree-file",
        },
        {},
        {},
    )
    asserts.equals(env, {
        "payload:00000003": "exact-payload-tree-file",
        "subtool:00000000": "exact-generated-executable-tree-file",
    }, bindings)

    prior_tree_producer = "3" * 64
    artifact_tree_roots = linux_test_node_input_artifact_tree_roots(
        "c" * 64,
        {
            "host-tool:00000002": struct(producer = prior_tree_producer, slot = "00000002"),
            "payload:00000003": struct(producer = other_producer, slot = "00000001"),
            "subtool:00000000": struct(producer = producer, slot = "00000000"),
        },
        {
            other_producer + ":00000001": "exact-payload-tree-file",
            producer + ":00000000": "exact-generated-executable-tree-file",
        },
        {"host": struct(directory = "prior-host-tree-root")},
        {"objects": "current-objects-tree-root"},
        {
            other_producer + ":00000001": struct(artifact_path = "payload.o", tree = "objects"),
            prior_tree_producer + ":00000002": struct(artifact_path = "bin/host-tool", tree = "host"),
            producer + ":00000000": struct(artifact_path = "generated-tool", tree = "objects"),
        },
    )
    asserts.equals(env, {
        "host": "prior-host-tree-root",
        "objects": "current-objects-tree-root",
    }, artifact_tree_roots, "producer roots must be typed and deduplicated by output tree")

    cross_stage = linux_test_resolve_node_input_bindings(
        "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
        {"inputs": {"subtool:00000007": struct(producer = producer, slot = "00000002")}},
        {},
        {"host:bin/generated-helper": "exact-prior-stage-tree-file"},
        {
            producer + ":00000002": struct(tree = "host", artifact_path = "bin/generated-helper"),
        },
    )
    asserts.equals(env, {"subtool:00000007": "exact-prior-stage-tree-file"}, cross_stage)

    prehost_to_bootstrap = linux_test_resolve_node_input_bindings(
        "9" * 64,
        {"inputs": {"host-tool:00000000": struct(producer = producer, slot = "00000004")}},
        {},
        {"prehost:bin/early-helper": "exact-prehost-tree-file"},
        {
            producer + ":00000004": struct(tree = "prehost", artifact_path = "bin/early-helper"),
        },
    )
    asserts.equals(env, {"host-tool:00000000": "exact-prehost-tree-file"}, prehost_to_bootstrap)

    bootstrap_to_host = linux_test_resolve_node_input_bindings(
        "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
        {"inputs": {"target:00000000": struct(producer = producer, slot = "00000003")}},
        {},
        {"bootstrap:target-input.o": "exact-bootstrap-tree-file"},
        {
            producer + ":00000003": struct(tree = "bootstrap", artifact_path = "target-input.o"),
        },
    )
    asserts.equals(env, {"target:00000000": "exact-bootstrap-tree-file"}, bootstrap_to_host)

    asserts.equals(
        env,
        "prep_base",
        linux_test_tree_input_directory_name(
            "prep",
            {"prep_base": "immutable-config-or-sdk"},
            {"bootstrap": "bootstrap-output"},
            {"input_tree_alias_prep": "prep_base"},
        ),
        "bootstrap/host recipes must bind logical prep to the immutable pre-host view",
    )
    asserts.equals(
        env,
        ["prep", "prep_base"],
        linux_test_source_input_namespace_names(
            "prep_base",
            {"input_tree_alias_prep": "prep_base"},
        ),
        "aliased input trees must index source leaves under both physical and logical namespaces",
    )
    asserts.equals(
        env,
        ["rust_source_files"],
        linux_test_node_source_closure_keys(["kernel", "rust", "rust"]),
        "a Rust source edge must carry the selected recursive module closure exactly once",
    )
    asserts.equals(
        env,
        [],
        linux_test_node_source_closure_keys(["kernel", "config"]),
        "non-Rust source edges must retain their fine-grained inputs",
    )
    asserts.equals(
        env,
        ["include/config/auto.conf", "sdk-only/generated-tool"],
        linux_test_composed_tree_base_paths(
            ["include/config/auto.conf", "generated/same", "sdk-only/generated-tool"],
            ["generated/same", "module-owned/prep.h"],
        ),
        "plan-owned prep leaves must replace same-path SDK leaves",
    )
    composed_exact = linux_test_resolve_node_input_bindings(
        "f" * 64,
        {"inputs": {"generated:00000000": struct(producer = producer, slot = "00000004")}},
        {},
        {"prep:generated/same": "planned-child-in-composed-prep"},
        {producer + ":00000004": struct(tree = "prep", artifact_path = "generated/same")},
    )
    asserts.equals(env, {"generated:00000000": "planned-child-in-composed-prep"}, composed_exact)

    recipe = "d" * 64
    bindings_digest = "9" * 64
    parsed_outputs = linux_test_parse_plan_marker_paths([
        "schema/linux-kernel-plan-v4",
        "index/00000000/" + producer,
        "index/00000001/" + other_producer,
        "toolsets/target/sha256-" + ("0" * 64),
        "recipes/" + recipe + ".json",
        "nodes/target/" + producer + "/kind/generate",
        "nodes/target/" + producer + "/product/vmlinux",
        "nodes/target/" + producer + "/recipe/" + recipe,
        "nodes/target/" + producer + "/tool/cc",
        "nodes/target/" + producer + "/in/bindings/" + bindings_digest + ".json",
        "nodes/target/" + producer + "/in/tool/target/objcopy/unscoped",
        "nodes/target/" + producer + "/in/tool/host/cc/scoped",
        "nodes/target/" + producer + "/out/objects/00000000/.linux-bzl-versions/first/generated/preserved",
        "nodes/target/" + other_producer + "/kind/generate",
        "nodes/target/" + other_producer + "/product/vmlinux",
        "nodes/target/" + other_producer + "/recipe/" + recipe,
        "nodes/target/" + other_producer + "/tool/cc",
        "nodes/target/" + other_producer + "/in/bindings/" + bindings_digest + ".json",
        "nodes/target/" + other_producer + "/out/objects/00000000/generated/ephemeral",
    ], "target")
    asserts.equals(
        env,
        ".linux-bzl-versions/first/generated/preserved",
        parsed_outputs.declared_outputs[producer + ":00000000"].artifact_path,
        "plan output markers must retain their physical TreeFile path",
    )
    asserts.equals(
        env,
        ["host@cc", "objcopy"],
        sorted(parsed_outputs.nodes[producer]["tools"].keys()),
        "plan tool markers must retain scoped and source-owned binding spellings",
    )
    asserts.equals(env, bindings_digest, parsed_outputs.nodes[producer]["input_bindings"].id)
    asserts.equals(
        env,
        "nodes/target/" + producer + "/in/bindings/" + bindings_digest + ".json",
        parsed_outputs.nodes[producer]["input_bindings"].file.tree_relative_path,
    )
    versioned_cross_stage = linux_test_resolve_node_input_bindings(
        "e" * 64,
        {"inputs": {"versioned:00000000": struct(producer = producer, slot = "00000000")}},
        {},
        {"objects:.linux-bzl-versions/first/generated/preserved": "exact-versioned-tree-file"},
        parsed_outputs.declared_outputs,
    )
    asserts.equals(
        env,
        {"versioned:00000000": "exact-versioned-tree-file"},
        versioned_cross_stage,
        "cross-stage edges must bind the producer's physical TreeFile path",
    )
    prehost_recipe = "7" * 64
    prehost_node = "8" * 64
    parsed_prehost = linux_test_parse_plan_marker_paths([
        "schema/linux-kernel-plan-v4",
        "index/00000000/" + prehost_node,
        "toolsets/host/sha256-" + ("6" * 64),
        "recipes/" + prehost_recipe + ".json",
        "nodes/prehost/" + prehost_node + "/kind/generate",
        "nodes/prehost/" + prehost_node + "/product/sdk",
        "nodes/prehost/" + prehost_node + "/recipe/" + prehost_recipe,
        "nodes/prehost/" + prehost_node + "/tool/cc",
        "nodes/prehost/" + prehost_node + "/in/bindings/" + bindings_digest + ".json",
        "nodes/prehost/" + prehost_node + "/out/prehost/00000000/bin/early-helper",
    ], "prehost")
    asserts.equals(env, "bin/early-helper", parsed_prehost.declared_outputs[prehost_node + ":00000000"].artifact_path)

    # Stage parsing must retain only action metadata used by this callback,
    # independent of the order in which ExpandedDirectory exposes markers.
    current_node = "1" * 64
    prior_node = "2" * 64
    unused_node = "3" * 64
    current_recipe = "4" * 64
    unused_recipe = "5" * 64
    prior_artifact_path = ".linux-bzl-versions/prior/bin/selected-helper"
    parsed_stage = linux_test_parse_plan_marker_paths([
        "recipes/" + current_recipe + ".json",
        "recipes/" + unused_recipe + ".json",
        "sources/src-00000001/kernel/selected.c",
        "sources/src-00000002/kernel/unused.c",
        "nodes/host/" + prior_node + "/out/host/00000000/" + prior_artifact_path,
        "schema/linux-kernel-plan-v4",
        "index/00000000/" + current_node,
        "index/00000001/" + prior_node,
        "index/00000002/" + unused_node,
        "toolsets/target/sha256-" + ("0" * 64),
        "nodes/target/" + current_node + "/kind/compile",
        "nodes/target/" + current_node + "/product/vmlinux",
        "nodes/target/" + current_node + "/recipe/" + current_recipe,
        "nodes/target/" + current_node + "/tool/cc",
        "nodes/target/" + current_node + "/in/bindings/" + bindings_digest + ".json",
        "nodes/target/" + current_node + "/in/source/src/00000000/src-00000001",
        "nodes/target/" + current_node + "/in/node-pack/helper/00000000.0.1.0",
        "nodes/target/" + current_node + "/out/objects/00000000/selected.o",
    ], "target", source_paths = ["selected.c", "unused.c"])
    asserts.false(env, hasattr(parsed_stage, "children"))
    asserts.false(env, hasattr(parsed_stage, "source_children"))
    asserts.equals(env, [current_recipe], sorted(parsed_stage.recipes))
    asserts.equals(env, ["src-00000001"], sorted(parsed_stage.sources))
    asserts.equals(env, "selected.c", parsed_stage.sources["src-00000001"].file.tree_relative_path)
    asserts.equals(env, [
        current_node + ":00000000",
        prior_node + ":00000000",
    ], sorted(parsed_stage.declared_outputs))
    asserts.equals(env, "selected.o", parsed_stage.nodes[current_node]["outputs"]["00000000"].artifact_path)
    asserts.equals(env, prior_artifact_path, parsed_stage.declared_outputs[prior_node + ":00000000"].artifact_path)
    decoded_stage_inputs = linux_test_decode_packed_node_inputs(parsed_stage, current_node)
    asserts.equals(env, {
        "helper:00000000": struct(producer = prior_node, slot = "00000000"),
    }, decoded_stage_inputs)

    # Every noncurrent descriptor in a trusted stage shard is exact and must be
    # indexed; same-stage output descriptors still remain action-local.
    same_stage_node = "6" * 64
    prior_file = struct(tree_relative_path = prior_artifact_path)
    unused_prior_file = struct(tree_relative_path = "bin/unused-helper")
    same_stage_file = struct(tree_relative_path = "same-stage.o")
    selected_prior = linux_test_selected_prior_outputs(
        {
            "host": struct(children = [prior_file, unused_prior_file]),
            "objects": struct(children = [same_stage_file]),
        },
        {
            current_node: {},
            same_stage_node: {},
        },
        {
            prior_node + ":00000000": struct(artifact_path = prior_artifact_path, tree = "host"),
            unused_node + ":00000000": struct(artifact_path = "bin/unused-helper", tree = "host"),
            same_stage_node + ":00000000": struct(artifact_path = "same-stage.o", tree = "objects"),
        },
    )
    asserts.equals(env, {
        "host:" + prior_artifact_path: prior_file,
        "host:bin/unused-helper": unused_prior_file,
    }, selected_prior)
    template_ctx = struct(
        declare_file = _fake_declare_file,
    )
    ephemeral = linux_test_declare_working_output(template_ctx, "work-tree", producer)
    asserts.equals(env, "file", ephemeral.artifact.kind)
    asserts.equals(env, producer + "/.linux-bzl-work-root", ephemeral.artifact.name)
    asserts.equals(env, "work-tree", ephemeral.artifact.parent)
    asserts.equals(env, "-working_directory_marker", ephemeral.argument)

    tools = linux_map_directory_tools(
        "exact-runner",
        "target",
        {
            "lz4": "exact-lz4-tool",
            "pahole": "exact-pahole-tool",
        },
        "exact-target-toolchain-closure",
        {"cc": "exact-host-cc-tool"},
        "exact-host-toolchain-closure",
        {"lz4": ["exact-lz4-helper", "exact-lz4-runtime"]},
    )
    asserts.equals(env, "exact-lz4-tool", tools.get("lz4"))
    asserts.equals(env, "exact-pahole-tool", tools.get("pahole"))
    asserts.equals(env, "exact-runner", tools.get("runner"))
    asserts.equals(env, "exact-target-toolchain-closure", tools.get("toolchain_files@target"))
    asserts.equals(env, "exact-host-toolchain-closure", tools.get("toolchain_files@host"))
    asserts.equals(env, "exact-host-cc-tool", tools.get("host@cc"))
    asserts.equals(env, ["exact-lz4-helper", "exact-lz4-runtime"], linux_test_companion_tool_bindings(tools, "lz4"))
    asserts.equals(env, {
        "lz4": "exact-lz4-tool",
        "pahole": "exact-pahole-tool",
    }, linux_test_runtime_tool_bindings(tools))
    asserts.equals(env, {
        "host@cc": "exact-host-cc-tool",
        "lz4": "exact-lz4-tool",
        "pahole": "exact-pahole-tool",
    }, linux_test_runtime_tool_bindings(tools, "target", {"host": True, "target": True}))

    probe_tools = linux_probe_map_directory_tools(
        "exact-probe-runner",
        {
            "bindgen": "exact-bindgen-tool",
            "python3": "exact-python-tool",
            "rustc": "exact-rustc-tool",
        },
        "exact-rust-toolchain-closure",
        "exact-rust-toolset-manifest",
        {"bindgen": ["exact-bindgen-helper"]},
    )
    asserts.equals(env, "exact-bindgen-tool", probe_tools.get("probe_role_bindgen"))
    asserts.equals(env, "exact-python-tool", probe_tools.get("probe_role_python3"))
    asserts.equals(env, "exact-rustc-tool", probe_tools.get("probe_role_rustc"))
    asserts.equals(env, "exact-probe-runner", probe_tools.get("probe_runner"))
    asserts.equals(env, "exact-rust-toolchain-closure", probe_tools.get("toolchain_files"))
    asserts.equals(env, "exact-rust-toolset-manifest", probe_tools.get("toolset_manifest"))
    asserts.equals(env, "exact-bindgen-helper", probe_tools.get("companion_tool_bindgen_00000000"))

    host_compile_flags = linux_test_host_dependency_compile_flags(
        struct(
            defines = ["LIBELF_STATIC=1", "LIBELF_STATIC=1"],
            external_includes = ["external/libelf/include"],
            framework_includes = [],
            includes = ["external/libelf/public"],
            local_defines = ["PRIVATE_TO_LIBELF=1"],
            local_includes = ["external/libelf/private"],
            quote_includes = ["external/libelf/quoted"],
            system_includes = [
                "external/libelf/system",
                "external/libelf/include",
            ],
        ),
        "libelf",
    )
    asserts.equals(env, [
        "-DLIBELF_STATIC=1",
        "-iquote__LINUX_BZL_HOST_DEPS__/external/libelf/quoted",
        "-isystem__LINUX_BZL_HOST_DEPS__/external/libelf/public",
        "-isystem__LINUX_BZL_HOST_DEPS__/external/libelf/system",
        "-isystem__LINUX_BZL_HOST_DEPS__/external/libelf/include",
    ], host_compile_flags)

    asserts.equals(
        env,
        [
            "--configured-before",
            "__LINUX_BZL_KBUILD_ARGS_V1__",
            "-Ityped/host-dependency/include",
            "--configured-after",
        ],
        linux_test_with_compile_action_arguments(
            [
                "--configured-before",
                "__LINUX_BZL_KBUILD_ARGS_V1__",
                "--configured-after",
            ],
            ["-Ityped/host-dependency/include"],
        ),
    )
    asserts.equals(
        env,
        [
            "--configured-before",
            "__LINUX_BZL_KBUILD_ARGS_V1__",
            "toolchain/lib/libc++.a",
            "toolchain/lib/libunwind.a",
            "--configured-after",
        ],
        linux_test_with_link_runtime_arguments(
            [
                "--configured-before",
                "__LINUX_BZL_KBUILD_ARGS_V1__",
                "--configured-after",
            ],
            [
                struct(path = "toolchain/lib/libc++.a"),
                struct(path = "toolchain/lib/libunwind.a"),
            ],
        ),
    )

    static_archive = "external/libelf/libelf.a"
    pic_static_archive = "external/libelf/libelf.pic.a"
    asserts.equals(
        env,
        static_archive,
        linux_test_host_library_artifact(
            struct(
                alwayslink = False,
                pic_static_library = pic_static_archive,
                static_library = static_archive,
            ),
            "static test library",
        ),
    )
    asserts.equals(
        env,
        pic_static_archive,
        linux_test_host_library_artifact(
            struct(
                alwayslink = False,
                pic_static_library = pic_static_archive,
                static_library = None,
            ),
            "PIC static test library",
        ),
    )

    asserts.equals(
        env,
        [
            "-L__LINUX_BZL_HOST_DEPS__/external/zlib+",
            "-L__LINUX_BZL_HOST_DEPS__/external/elfutils+",
        ],
        linux_test_host_dependency_library_search_flags([
            "__LINUX_BZL_HOST_DEPS__/external/zlib+/libz.a",
            "__LINUX_BZL_HOST_DEPS__/external/elfutils+/libelf.a",
            "__LINUX_BZL_HOST_DEPS__/external/elfutils+/libeu.a",
            "__LINUX_BZL_HOST_DEPS__/external/zlib+/libz.a",
        ]),
    )

    staged_script = "__LINUX_BZL_HOST_DEPS__/external/libelf/version.lds"
    link_paths = {
        "bazel-out/k8-opt-exec/bin/external/libelf/version.lds": staged_script,
        "external/libelf/version.lds": staged_script,
    }
    asserts.equals(
        env,
        "-Wl,--version-script=" + staged_script,
        linux_test_rewrite_host_dependency_link_flag(
            "-Wl,--version-script=external/libelf/version.lds",
            link_paths,
        ),
    )
    asserts.equals(
        env,
        "@" + staged_script,
        linux_test_rewrite_host_dependency_link_flag(
            "@bazel-out/k8-opt-exec/bin/external/libelf/version.lds",
            link_paths,
        ),
    )
    return unittest.end(env)

_mapped_kernel_backend_test = unittest.make(_mapped_kernel_backend_test_impl)

def mapped_kernel_backend_test(name):
    _mapped_kernel_backend_test(name = name)

def _requirement_conflict_probe_impl(_ctx):
    linux_test_toolset_execution_requirements({
        "cc": {"requires-network": "0"},
        "ld": {"requires-network": "1"},
    }, "conflicting target")
    return []

_requirement_conflict_probe = rule(implementation = _requirement_conflict_probe_impl)

def _requirement_conflict_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "tool execution requirements disagree on requires-network")
    return analysistest.end(env)

_requirement_conflict_test = analysistest.make(
    _requirement_conflict_test_impl,
    expect_failure = True,
)

def mapped_kernel_requirement_validation_test(name):
    subject = name + "_subject"
    _requirement_conflict_probe(name = subject, tags = ["manual"])
    _requirement_conflict_test(name = name, target_under_test = ":" + subject)

def _canonical_path_probe_impl(ctx):
    linux_test_canonical_file_path(ctx.attr.path, ctx.attr.short_path)
    return []

_canonical_path_probe = rule(
    implementation = _canonical_path_probe_impl,
    attrs = {
        "path": attr.string(mandatory = True),
        "short_path": attr.string(mandatory = True),
    },
)

def _canonical_path_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected_error)
    return analysistest.end(env)

_canonical_path_failure_test = analysistest.make(
    _canonical_path_failure_test_impl,
    attrs = {"expected_error": attr.string(mandatory = True)},
    expect_failure = True,
)

def _packed_plan_paths(index_markers, input_markers = [], binding_markers = None):
    node = "b" * 64
    recipe = "c" * 64
    root = "nodes/target/" + node
    if binding_markers == None:
        binding_markers = [("9" * 64) + ".json"]
    return [
        "schema/linux-kernel-plan-v4",
        "toolsets/target/sha256-" + ("0" * 64),
        "recipes/" + recipe + ".json",
        root + "/kind/compile",
        root + "/product/vmlinux",
        root + "/recipe/" + recipe,
        root + "/tool/cc",
        root + "/out/objects/00000000/result.o",
    ] + index_markers + [
        root + "/in/bindings/" + marker
        for marker in binding_markers
    ] + [
        root + "/in/node-pack/" + marker
        for marker in input_markers
    ]

def _packed_plan_probe_impl(ctx):
    parsed = linux_test_parse_plan_marker_paths(ctx.attr.paths, "target")
    linux_test_decode_packed_node_inputs(parsed, ctx.attr.node_id)
    return []

_packed_plan_probe = rule(
    implementation = _packed_plan_probe_impl,
    attrs = {
        "node_id": attr.string(mandatory = True),
        "paths": attr.string_list(mandatory = True),
    },
)

def _artifact_tree_root_probe_impl(ctx):
    producer = "a" * 64
    slot = "00000000"
    output_key = producer + ":" + slot
    dependency = struct(producer = producer, slot = slot)
    outputs = {output_key: "current-output"}
    if ctx.attr.mode == "invalid_producer_id":
        dependency = struct(producer = "not-a-digest", slot = slot)
        outputs = {}
    linux_test_node_input_artifact_tree_roots(
        "b" * 64,
        {"payload:00000000": dependency},
        outputs,
        {},
        {},
        {output_key: struct(artifact_path = "result.o", tree = "objects")},
    )
    return []

_artifact_tree_root_probe = rule(
    implementation = _artifact_tree_root_probe_impl,
    attrs = {"mode": attr.string(mandatory = True)},
)

def _stage_output_tree_probe_impl(_ctx):
    node = "a" * 64
    recipe = "b" * 64
    linux_test_parse_plan_marker_paths([
        "schema/linux-kernel-plan-v4",
        "index/00000000/" + node,
        "toolsets/target/sha256-" + ("0" * 64),
        "recipes/" + recipe + ".json",
        "nodes/prep/" + node + "/kind/generate",
        "nodes/prep/" + node + "/product/vmlinux",
        "nodes/prep/" + node + "/recipe/" + recipe,
        "nodes/prep/" + node + "/tool/actionfile",
        "nodes/prep/" + node + "/in/bindings/" + ("9" * 64) + ".json",
        "nodes/prep/" + node + "/out/objects/00000000/generated/object",
    ], "prep")
    return []

_stage_output_tree_probe = rule(implementation = _stage_output_tree_probe_impl)

def _composed_tree_prefix_collision_probe_impl(_ctx):
    linux_test_composed_tree_base_paths(
        ["include/generated"],
        ["include/generated/header.h"],
    )
    return []

_composed_tree_prefix_collision_probe = rule(implementation = _composed_tree_prefix_collision_probe_impl)

def _planned_tree_prefix_collision_probe_impl(_ctx):
    linux_test_composed_tree_base_paths(
        [],
        [
            "tools/bpf/resolve_btfids/libsubcmd",
            "tools/bpf/resolve_btfids/libsubcmd/subcmd-config.o",
        ],
        tree = "host",
    )
    return []

_planned_tree_prefix_collision_probe = rule(implementation = _planned_tree_prefix_collision_probe_impl)

def _generated_toolchain_directory_path_probe_impl(_ctx):
    linux_test_render_toolchain_action_value(
        "-Ibazel-out/k8-opt-exec/bin/toolchain/lib/clang/22/include",
        [struct(
            is_directory = False,
            is_source = False,
            path = "bazel-out/k8-opt-exec/bin/toolchain/lib/clang/22/include/stddef.h",
            short_path = "toolchain/lib/clang/22/include/stddef.h",
        )],
    )
    return []

_generated_toolchain_directory_path_probe = rule(implementation = _generated_toolchain_directory_path_probe_impl)

def mapped_kernel_path_validation_test(name):
    tests = []
    for case in [
        struct(
            expected_error = "has invalid relative path",
            name = "parent",
            path = "external/repo/../escape",
            short_path = "external/repo/../escape",
        ),
        struct(
            expected_error = "has invalid relative path",
            name = "absolute",
            path = "/tmp/tool",
            short_path = "/tmp/tool",
        ),
        struct(
            expected_error = "map to different canonical paths",
            name = "mismatch",
            path = "bazel-out/k8-opt-exec/bin/external/repo/one",
            short_path = "../repo/two",
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        _canonical_path_probe(
            name = subject,
            path = case.path,
            short_path = case.short_path,
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    consumer = "b" * 64
    for case in [
        struct(
            expected_error = "repeats node index ordinal or ID",
            indexes = ["index/00000000/" + ("a" * 64), "index/00000000/" + consumer],
            inputs = [],
            name = "packed_duplicate_index",
        ),
        struct(
            indexes = ["index/00000000/" + consumer],
            expected_error = "non-canonical base36 ordinal",
            inputs = ["payload/00000000.00.0.0"],
            name = "packed_noncanonical_base36",
        ),
        struct(
            bindings = [],
            expected_error = "has no input binding manifest",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "missing_input_bindings",
        ),
        struct(
            bindings = ["sha256-" + ("9" * 64) + ".json"],
            expected_error = "has invalid input binding manifest",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "invalid_input_bindings_id",
        ),
        struct(
            bindings = [("8" * 64) + ".json", ("9" * 64) + ".json"],
            expected_error = "repeats input binding manifest",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "duplicate_input_bindings",
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        _packed_plan_probe(
            name = subject,
            node_id = consumer,
            paths = _packed_plan_paths(
                case.indexes,
                input_markers = case.inputs,
                binding_markers = getattr(case, "bindings", None),
            ),
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    for case in [
        struct(
            expected_error = "has invalid producer binding",
            mode = "invalid_producer_id",
            name = "artifact_tree_invalid_producer_id",
        ),
        struct(
            expected_error = "requires unavailable current-stage output artifact tree objects",
            mode = "missing_current_root",
            name = "artifact_tree_missing_current_root",
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        _artifact_tree_root_probe(
            name = subject,
            mode = case.mode,
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    for case in [
        struct(
            expected_error = "cannot write objects tree",
            name = "stage_output_tree",
            rule = _stage_output_tree_probe,
        ),
        struct(
            expected_error = "file/subtree collision",
            name = "composed_tree_prefix",
            rule = _composed_tree_prefix_collision_probe,
        ),
        struct(
            expected_error = "file/subtree collision",
            name = "planned_tree_prefix",
            rule = _planned_tree_prefix_collision_probe,
        ),
        struct(
            expected_error = "references generated directory",
            name = "generated_toolchain_directory",
            rule = _generated_toolchain_directory_path_probe,
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        case.rule(name = subject, tags = ["manual"])
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    native.test_suite(name = name, tests = tests)

def _host_dependency_tree_artifact_probe_impl(ctx):
    tree = ctx.actions.declare_directory(ctx.label.name + ".headers")
    linux_test_validate_host_dependency_artifact(tree, "test header")
    return []

_host_dependency_tree_artifact_probe = rule(implementation = _host_dependency_tree_artifact_probe_impl)

def _host_library_validation_probe_impl(ctx):
    fields = {
        "alwayslink": False,
        "dynamic_library": None,
        "interface_library": None,
        "objects": [],
        "pic_objects": [],
        "pic_static_library": None,
        "static_library": None,
    }
    if ctx.attr.kind == "alwayslink":
        fields["alwayslink"] = True
        fields["static_library"] = "libalways.a"
    elif ctx.attr.kind == "dynamic":
        fields["dynamic_library"] = "libdynamic.so"
    elif ctx.attr.kind == "objects":
        fields["objects"] = ["loose.o"]
    linux_test_host_library_artifact(struct(**fields), "test library")
    return []

_host_library_validation_probe = rule(
    implementation = _host_library_validation_probe_impl,
    attrs = {"kind": attr.string(mandatory = True)},
)

def mapped_kernel_host_dependency_validation_test(name):
    tests = []
    tree_subject = name + "_tree_subject"
    tree_test = name + "_tree"
    _host_dependency_tree_artifact_probe(name = tree_subject, tags = ["manual"])
    _canonical_path_failure_test(
        name = tree_test,
        expected_error = "test header is a TreeArtifact",
        target_under_test = ":" + tree_subject,
    )
    tests.append(":" + tree_test)
    for case in [
        struct(error = "requires alwayslink/whole-archive semantics", kind = "alwayslink"),
        struct(error = "has only dynamic/interface library inputs", kind = "dynamic"),
        struct(error = "has only loose object inputs", kind = "objects"),
    ]:
        subject = name + "_" + case.kind + "_subject"
        test = name + "_" + case.kind
        _host_library_validation_probe(
            name = subject,
            kind = case.kind,
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    native.test_suite(name = name, tests = tests)
