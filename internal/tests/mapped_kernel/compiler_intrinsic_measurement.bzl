"""Independent real-compiler witness for mapped intrinsic fixture tests."""

load("@rules_cc//cc:find_cc_toolchain.bzl", "use_cc_toolchain")
load("//internal:execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")
load("//internal:providers.bzl", "LinuxModuleSdkInfo")
load("//internal:toolchain_action_paths.bzl", "EXECUTION_ROOT_MARKER", "render_toolchain_action_value", "toolchain_action_path_index")

def _compiler_intrinsic_measurement_impl(ctx):
    # Consume the family-selected compiler, not the auxiliary/default C/C++
    # toolchain: CI can configure those independently. The SDK's analysis-time
    # contract does not make this action consume any scanner/probe answers.
    sdk = ctx.attr.kernel[LinuxModuleSdkInfo]
    if linux_execution_platform_label(ctx.attr._execution_platform) != sdk.target_execution_platform:
        fail("intrinsic measurement must execute on its kernel's selected target execution platform")
    output = ctx.actions.declare_file(ctx.label.name + ".o")
    path_index = toolchain_action_path_index(sdk.target_toolchain_files)
    arguments = ctx.actions.args()
    sentinel_count = 0
    for argument in sdk.target_action_args["cc"]:
        if argument == "__LINUX_BZL_KBUILD_ARGS_V1__":
            sentinel_count += 1
            arguments.add_all(ctx.attr.copts)
            arguments.add_all(["-x", "c", "-c"])
            arguments.add(ctx.file.src)
            arguments.add("-o", output)
        else:
            rendered = render_toolchain_action_value(argument, path_index)

            # This independent action starts in the execroot. Retain typed
            # path fragments but omit the mapped runner's absolute-root marker.
            arguments.add_joined([
                fragment.replace(EXECUTION_ROOT_MARKER + "/", "") if type(fragment) == "string" else fragment
                for fragment in rendered.fragments
            ], join_with = "", expand_directories = False)
    if sentinel_count != 1:
        fail("intrinsic measurement requires one configured compiler argument boundary")

    # Unlike argv, Spawn environment values cannot contain typed File pieces.
    # Source-artifact paths stay stable under path mapping; reject generated
    # paths rather than silently measuring a different compiler environment.
    environment = {}
    for name, value in sdk.target_action_environments["cc"].items():
        rendered = render_toolchain_action_value(value, path_index)
        if any([type(fragment) == "File" and not fragment.is_source for fragment in rendered.fragments]):
            fail("intrinsic fixture measurement requires path-mapping-stable compiler environment")
        environment[name] = "".join([
            fragment.path if type(fragment) == "File" else fragment.replace(EXECUTION_ROOT_MARKER + "/", "")
            for fragment in rendered.fragments
        ])
    ctx.actions.run(
        mnemonic = "MappedIntrinsicMeasurement",
        executable = sdk.target_tool_files["cc"],
        arguments = [arguments],
        inputs = depset([ctx.file.src], transitive = [sdk.target_toolchain_files]),
        tools = sdk.target_companion_tools.get("cc", []),
        outputs = [output],
        env = environment,
        execution_requirements = dict(sdk.target_action_requirements["cc"], **{"supports-path-mapping": "1"}),
    )
    return [DefaultInfo(files = depset([output]))]

compiler_intrinsic_measurement = rule(
    implementation = _compiler_intrinsic_measurement_impl,
    attrs = {
        "copts": attr.string_list(),
        "kernel": attr.label(mandatory = True, providers = [LinuxModuleSdkInfo]),
        "src": attr.label(allow_single_file = [".c"], mandatory = True),
        "_execution_platform": linux_execution_platform_attr(),
    },
    toolchains = use_cc_toolchain(),
)
