"""Analysis tests for configured compact-v7 flag-program nodes."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:flag_programs.bzl", "LinuxFlagProgramsInfo", "linux_flag_programs")
load("//internal:graph_profile.bzl", "linux_graph_profile_context")

visibility("private")

_EMPTY = "0101010101010101010101010101010101010101010101010101010101010101"
_CONTEXT = "0202020202020202020202020202020202020202020202020202020202020202"
_WHEN_TRUE = "0303030303030303030303030303030303030303030303030303030303030303"
_CONTEXT_PROGRAM = "0404040404040404040404040404040404040404040404040404040404040404"
_RESULT_PROGRAM = "0505050505050505050505050505050505050505050505050505050505050505"
_PROBE = "0606060606060606060606060606060606060606060606060606060606060606"
_SELECT = "0707070707070707070707070707070707070707070707070707070707070707"

def _flag_programs_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[LinuxFlagProgramsInfo]
    actions = analysistest.target_actions(env)
    selects = [action for action in actions if action.mnemonic == "LinuxFlagSelect"]

    asserts.equals(env, [_CONTEXT_PROGRAM, _RESULT_PROGRAM], sorted(info.programs.keys()))
    asserts.equals(env, 1, len(selects))
    if selects:
        action = selects[0]
        asserts.true(env, "resolve-node" in action.argv)
        asserts.true(env, "-language" in action.argv)
        asserts.true(env, "c" in action.argv)
        asserts.true(env, "-srcarch" in action.argv)
        asserts.true(env, "x86" in action.argv)
        inputs = [file.basename for file in action.inputs.to_list()]
        asserts.true(env, "first.inc" in inputs)
        asserts.false(env, "unrelated.inc" in inputs)
    return analysistest.end(env)

_flag_programs_test = analysistest.make(_flag_programs_test_impl)

def flag_programs_test(name):
    profile = name + "_profile"
    linux_graph_profile_context(
        name = profile,
        arch = "x86_64",
        graph_projection = ":graph_projection.json",
        kbuild_linker = "@llvm//tools:ld.lld",
        source_root = "//tests/compile:source/Kconfig",
        tags = ["manual"],
    )
    subject = name + "_subject"
    linux_flag_programs(
        name = subject,
        graph_profile = ":" + profile,
        nodes = {
            _SELECT: json.encode({
                "kind": "select",
                "probe": _PROBE,
                "when_false": _EMPTY,
                "when_true": _WHEN_TRUE,
            }),
        },
        probes = {
            _PROBE: json.encode({
                "candidate_argv": ["-include", "__LINUX_BZL_SRCTREE__/shared/first.inc"],
                "context_program": _CONTEXT_PROGRAM,
                "kind": "cc_option",
            }),
        },
        programs = {
            _CONTEXT_PROGRAM: _CONTEXT,
            _RESULT_PROGRAM: _SELECT,
        },
        source_paths = [
            "cross_tree/unrelated.inc",
            "shared/first.inc",
        ],
        source_root = "//tests/compile:source/Kconfig",
        srcs = [
            "//tests/compile:source/cross_tree/unrelated.inc",
            "//tests/compile:source/shared/first.inc",
        ],
        tags = ["manual"],
        terminals = {
            _CONTEXT: json.encode(["-DTEST=1"]),
            _EMPTY: json.encode([]),
            _WHEN_TRUE: json.encode(["-include", "__LINUX_BZL_SRCTREE__/shared/first.inc"]),
        },
    )
    _flag_programs_test(
        name = name,
        target_under_test = ":" + subject,
    )
