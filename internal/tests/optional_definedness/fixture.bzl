"""Real configured-compiler tests of detached optional query process status."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE", "use_cc_toolchain")
load("//internal:execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")
load("//internal:probe_map_directory.bzl", "expand_linux_probe_plan", "linux_probe_map_directory_params", "linux_probe_map_directory_tools")
load("//internal:providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo")

visibility("//internal/tests/...")

def _path(args, flag, artifact):
    args.add_all([artifact], expand_directories = False, format_each = flag + "=%s")

def _compiler_optional_definedness_fixture_impl(ctx):
    sdk = ctx.attr.kernel[LinuxModuleSdkInfo]
    if linux_execution_platform_label(ctx.attr._execution_platform) != sdk.target_execution_platform:
        fail("optional definedness fixture must execute on the kernel's selected target execution platform")
    arch = ctx.attr.kernel[LinuxKernelInfo].arch
    plan = ctx.actions.declare_directory(ctx.label.name + ".plan")
    results = ctx.actions.declare_directory(ctx.label.name + ".results")
    receipt = ctx.actions.declare_file(ctx.label.name + ".json")
    common = {
        "-arch": arch,
        "-bootstrap": sdk.target_probe_results,
        "-identity": sdk.target_toolset_identity,
        "-manifest": sdk.target_toolset_manifest,
    }
    for mode, output in [("discover", plan), ("verify", receipt)]:
        args = ctx.actions.args()
        args.add("-mode", mode)
        for flag, artifact in sorted(common.items()):
            _path(args, flag, artifact)
        _path(args, "-out", output)
        inputs = common.values()
        if mode == "verify":
            _path(args, "-plan", plan)
            _path(args, "-results", results)
            inputs = inputs + [plan, results]
        ctx.actions.run(
            mnemonic = "MappedOptionalDefinednessDiscover" if mode == "discover" else "MappedOptionalDefinednessVerify",
            executable = ctx.executable._fixture,
            arguments = [args],
            inputs = inputs,
            outputs = [output],
            execution_requirements = {"supports-path-mapping": "1"},
        )

    # The production callback owns compiler invocation, exact action envelopes,
    # runtime closure, marker validation, and normal failed-process recording.
    # Five optional-query and three macro-write test probes do not add or alter
    # a production guard round.
    ctx.actions.map_directory(
        implementation = expand_linux_probe_plan,
        input_directories = {
            "plan": plan,
            "target_toolset_identity": sdk.target_toolset_identity,
        },
        additional_inputs = {},
        output_directories = {"results": results},
        tools = linux_probe_map_directory_tools(
            sdk.target_probe_runner,
            sdk.target_tool_files,
            sdk.target_toolchain_files,
            sdk.target_toolset_manifest,
            sdk.target_toolset_anchors,
            sdk.target_companion_tools,
        ),
        additional_params = linux_probe_map_directory_params("target", sdk.target_action_args, sdk.target_action_environments),
        env = {},
        execution_requirements = dict(sdk.target_action_requirements["cc"], **{"supports-path-mapping": "1"}),
        mnemonic = "MappedOptionalDefinednessProbe",
        toolchain = CC_TOOLCHAIN_TYPE,
    )
    return [DefaultInfo(files = depset([receipt]))]

compiler_optional_definedness_fixture = rule(
    implementation = _compiler_optional_definedness_fixture_impl,
    attrs = {
        "kernel": attr.label(mandatory = True, providers = [LinuxKernelInfo, LinuxModuleSdkInfo]),
        "_execution_platform": linux_execution_platform_attr(),
        "_fixture": attr.label(default = Label("//internal/tests/optional_definedness:fixture"), executable = True, cfg = "exec"),
    },
    toolchains = use_cc_toolchain(),
)
