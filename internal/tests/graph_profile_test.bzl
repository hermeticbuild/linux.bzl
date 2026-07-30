"""Analysis test for the configured graph-profile context."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:graph_profile.bzl", "LinuxGraphProfileInfo", "linux_graph_profile_context")

visibility("private")

_SOURCE_SENTINEL = "__LINUX_BZL_GRAPH_PROFILE_SOURCE__.c"
_OUTPUT_SENTINEL = "__LINUX_BZL_GRAPH_PROFILE_OUTPUT__.o"
_KBUILD_FLAGS_SENTINEL = "__LINUX_BZL_KBUILD_FLAGS_v1__"

def _action_with_mnemonic(actions, mnemonic):
    matches = [action for action in actions if action.mnemonic == mnemonic]
    return matches[0] if len(matches) == 1 else None

def _count(values, expected):
    return len([value for value in values if value == expected])

def _graph_profile_context_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxGraphProfileInfo]
    actions = analysistest.target_actions(env)

    asserts.equals(env, "x86_64", info.arch)
    asserts.equals(env, _KBUILD_FLAGS_SENTINEL, info.kbuild_flags_sentinel)
    asserts.equals(env, "graph_projection.json", info.projection.basename)
    asserts.equals(env, "ld.lld", info.kbuild_linker.basename)
    asserts.true(
        env,
        info.compiler in info.toolchain_files.to_list(),
        "selected compiler must be a declared C toolchain input",
    )
    asserts.equals(env, "C", info.environment.get("LANG"))
    asserts.equals(env, "C", info.environment.get("LC_ALL"))
    asserts.equals(env, "UTC", info.environment.get("TZ"))

    inspect = _action_with_mnemonic(actions, "LinuxGraphProfileInspect")
    validate = _action_with_mnemonic(actions, "LinuxGraphProfileValidate")
    asserts.true(env, inspect != None, "missing compiler inspection action")
    asserts.true(env, validate != None, "missing profile validation action")
    if inspect != None:
        compile_args = [
            arg.removeprefix("-compile_arg=")
            for arg in inspect.argv
            if arg.startswith("-compile_arg=")
        ]
        asserts.equals(env, 1, _count(compile_args, "-c"))
        asserts.equals(env, 1, _count(compile_args, _SOURCE_SENTINEL))
        asserts.equals(env, 1, _count(compile_args, _OUTPUT_SENTINEL))
        asserts.equals(env, 1, _count(compile_args, _KBUILD_FLAGS_SENTINEL))
        asserts.true(env, "-o" in compile_args)
        template_outputs = [
            arg
            for arg in inspect.argv
            if arg.startswith("-template_out=")
        ]
        asserts.equals(env, 1, len(template_outputs))
        if template_outputs:
            asserts.true(
                env,
                template_outputs[0].startswith("-template_out=bazel-out/"),
                "expected File-backed compiler template output, got %s" % template_outputs[0],
            )
    if validate != None:
        asserts.true(env, "validate-graph" in validate.argv)
        asserts.equals(
            env,
            1,
            len([arg for arg in validate.argv if arg.startswith("-shell=")]),
        )
        asserts.equals(
            env,
            1,
            len([arg for arg in validate.argv if arg.startswith("-coreutils=")]),
        )
        asserts.equals(
            env,
            1,
            len([arg for arg in validate.argv if arg.startswith("-grep=")]),
        )
        asserts.equals(
            env,
            1,
            len([arg for arg in validate.argv if arg.startswith("-archiver=")]),
        )
        asserts.equals(
            env,
            1,
            len([arg for arg in validate.argv if arg.startswith("-nm=")]),
        )
        asserts.equals(
            env,
            1,
            len([arg for arg in validate.argv if arg.startswith("-objcopy=")]),
        )
        input_basenames = [file.basename for file in validate.inputs.to_list()]
        asserts.true(env, "coreutils" in input_basenames)
        asserts.true(env, "llvm-ar" in input_basenames)
        asserts.true(env, "llvm-nm" in input_basenames)
        asserts.true(env, "llvm-objcopy" in input_basenames)
        asserts.true(env, "graph_projection.json" in input_basenames)
        asserts.false(env, "graph_profile.json" in input_basenames)

    output_basenames = sorted([file.basename for file in target.files.to_list()])
    asserts.equals(
        env,
        [
            ctx.attr.target_name + ".command_template.json",
            ctx.attr.target_name + ".compiler_identity.json",
            ctx.attr.target_name + ".graph_profile.validated",
        ],
        output_basenames,
    )
    return analysistest.end(env)

_graph_profile_context_test = analysistest.make(
    _graph_profile_context_test_impl,
    attrs = {
        "target_name": attr.string(mandatory = True),
    },
)

def graph_profile_context_test(name):
    target_name = name + "_target"
    linux_graph_profile_context(
        name = target_name,
        arch = "x86_64",
        graph_projection = ":graph_projection.json",
        kbuild_linker = "@llvm//tools:ld.lld",
        source_root = "//tests/compile:source/Kconfig",
        tags = ["manual"],
    )
    _graph_profile_context_test(
        name = name,
        target_name = target_name,
        target_under_test = ":" + target_name,
    )
