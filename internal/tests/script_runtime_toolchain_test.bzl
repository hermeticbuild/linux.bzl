"""Analysis test for the hermetic script runtime toolchain."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(
    "//internal:script_runtime_toolchain.bzl",
    "SCRIPT_RUNTIME_TOOLCHAIN_TYPE",
    "ScriptRuntimeToolchainInfo",
    "linux_script_runtime",
    "script_runtime_toolchain",
)

visibility("private")

_ScriptRuntimeSelectionInfo = provider(
    doc = "Selected script runtime observed by the test probe.",
    fields = {
        "applets": "Selected command-name override mapping.",
        "execution_constraints": "Constraint labels declared by the selected runtime.",
        "files": "Complete runtime file closure.",
        "interpreter": "Selected interpreter File.",
        "interpreter_args": "Selected interpreter arguments.",
        "multicall": "Selected multicall File.",
    },
)

_ScriptRuntimeDeclarationInfo = provider(
    doc = "Native script runtime toolchain declaration observed by the aspect.",
    fields = {
        "exec_compatible_with": "Execution constraints on the native toolchain declaration.",
        "implementation": "Label of the toolchain implementation target.",
        "toolchain_type": "Label of the declared toolchain type.",
    },
)

def _execution_platform_runtime_impl(ctx):
    executable = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = executable,
        content = "#!/bin/sh\n",
        is_executable = True,
    )
    return [DefaultInfo(executable = executable)]

_execution_platform_runtime = rule(
    implementation = _execution_platform_runtime_impl,
    executable = True,
)

def _label_string(value):
    if type(value) == "Label":
        return str(value)
    return str(value.label)

def _script_runtime_declaration_aspect_impl(_target, ctx):
    return [_ScriptRuntimeDeclarationInfo(
        exec_compatible_with = [_label_string(value) for value in ctx.rule.attr.exec_compatible_with],
        implementation = _label_string(ctx.rule.attr.toolchain),
        toolchain_type = _label_string(ctx.rule.attr.toolchain_type),
    )]

_script_runtime_declaration_aspect = aspect(
    implementation = _script_runtime_declaration_aspect_impl,
)

def _script_runtime_probe_impl(ctx):
    runtime = script_runtime_toolchain(ctx)
    return [_ScriptRuntimeSelectionInfo(
        applets = runtime.applets,
        execution_constraints = runtime.execution_constraints,
        files = runtime.files,
        interpreter = runtime.interpreter,
        interpreter_args = runtime.interpreter_args,
        multicall = runtime.multicall,
    )]

_script_runtime_probe = rule(
    implementation = _script_runtime_probe_impl,
    toolchains = [SCRIPT_RUNTIME_TOOLCHAIN_TYPE],
)

def _script_runtime_toolchain_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_ScriptRuntimeSelectionInfo]
    asserts.equals(env, "busybox", info.interpreter.basename)
    asserts.equals(env, info.interpreter, info.multicall)
    asserts.equals(env, ["sh"], info.interpreter_args)
    asserts.equals(env, ["find"], sorted(info.applets.keys()))
    asserts.equals(env, "toybox", info.applets["find"].basename)
    asserts.equals(env, ["busybox", "toybox"], sorted([file.basename for file in info.files.to_list()]))
    asserts.equals(
        env,
        [
            str(Label("@platforms//os:linux")),
            str(Label("@platforms//cpu:x86_64")),
        ],
        info.execution_constraints,
    )
    return analysistest.end(env)

_script_runtime_toolchain_test = analysistest.make(
    _script_runtime_toolchain_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_arm64")),
    },
)

def _script_runtime_exec_configuration_test_impl(ctx):
    env = analysistest.begin(ctx)
    runtime = analysistest.target_under_test(env)[ScriptRuntimeToolchainInfo]
    asserts.equals(env, ctx.attr.expected_basename, runtime.interpreter.basename)
    asserts.equals(env, runtime.interpreter, runtime.multicall)
    return analysistest.end(env)

_script_runtime_exec_configuration_test = analysistest.make(
    _script_runtime_exec_configuration_test_impl,
    attrs = {
        "expected_basename": attr.string(mandatory = True),
    },
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_arm64")),
    },
)

def _script_runtime_declaration_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_ScriptRuntimeDeclarationInfo]
    asserts.equals(
        env,
        [
            str(Label("@platforms//os:linux")),
            str(Label("@platforms//cpu:x86_64")),
        ],
        info.exec_compatible_with,
    )
    asserts.equals(env, str(Label("//internal:script_runtime")), info.implementation)
    asserts.equals(env, str(Label("//internal:script_runtime_toolchain_type")), info.toolchain_type)
    return analysistest.end(env)

_script_runtime_declaration_test = analysistest.make(
    _script_runtime_declaration_test_impl,
    extra_target_under_test_aspects = [_script_runtime_declaration_aspect],
)

def script_runtime_toolchain_test(name):
    target = name + "_target"
    selection_test = name + "_selection"
    declaration_test = name + "_declaration"
    exec_runtime = name + "_exec_runtime"
    exec_implementation = name + "_exec_implementation"
    exec_configuration_test = name + "_exec_configuration"
    _script_runtime_probe(
        name = target,
        tags = ["manual"],
    )
    _script_runtime_toolchain_test(
        name = selection_test,
        target_under_test = ":" + target,
    )
    _script_runtime_declaration_test(
        name = declaration_test,
        target_under_test = "//internal:busybox_script_runtime_toolchain",
    )
    _execution_platform_runtime(
        name = exec_runtime,
        tags = ["manual"],
        target_compatible_with = [
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
    )
    linux_script_runtime(
        name = exec_implementation,
        interpreter_args = ["sh"],
        runtime = ":" + exec_runtime,
        tags = ["manual"],
    )
    _script_runtime_exec_configuration_test(
        name = exec_configuration_test,
        expected_basename = exec_runtime + ".sh",
        target_under_test = ":" + exec_implementation,
    )
    native.test_suite(
        name = name,
        tests = [
            ":" + selection_test,
            ":" + declaration_test,
            ":" + exec_configuration_test,
        ],
    )
