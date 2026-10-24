"""Source identity and probe handoff checks at the kernel analysis boundary."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//rules:kernel.bzl", "kernel_build")
load("//rules:syntax.bzl", "KernelSyntaxInfo")

# The analysis tests inspect registered actions without executing this callback.
# buildifier: disable=unused-variable
def _expand(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    fail("Analysis-only syntax callback must not execute")

def _syntax_impl(ctx):
    return [KernelSyntaxInfo(anchor = ctx.file.anchor, expand = _expand)]

_syntax = rule(implementation = _syntax_impl, attrs = {"anchor": attr.label(allow_single_file = True)})

def _test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "does not match native configuration source")
    return analysistest.end(env)

_test = analysistest.make(
    _test_impl,
    expect_failure = True,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
        "//command_line_option:extra_execution_platforms": [str(Label("@llvm//platforms:linux_x86_64"))],
    },
)

def syntax_identity_test(name):
    _syntax(name = name + "_foreign", anchor = ":main.c", tags = ["manual"])
    kernel_build(
        name = name + "_kernel",
        config = "//native:config",
        syntax = ":" + name + "_foreign",
        tags = ["manual"],
    )
    _test(name = name, target_under_test = ":" + name + "_kernel")

def _rounds_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = analysistest.target_actions(env)
    seeds = [action for action in actions if action.mnemonic == "KernelProbeSeed"]
    asserts.equals(env, 1, len(seeds))
    asserts.equals(env, "seed", seeds[0].argv[-3])
    probes = {action.outputs.to_list()[0].basename: action for action in actions if action.mnemonic == "KernelKbuildProbe"}
    asserts.equals(env, ctx.attr.rounds, len(probes))
    for round, consumer in enumerate(ctx.attr.consumers):
        action = probes[target.label.name + ".answers%d" % round]
        asserts.equals(env, consumer, action.argv[-1])
    return analysistest.end(env)

_rounds_test = analysistest.make(
    _rounds_test_impl,
    attrs = {"rounds": attr.int(), "consumers": attr.string_list()},
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
        "//command_line_option:extra_execution_platforms": [str(Label("@llvm//platforms:linux_x86_64"))],
    },
)

def probe_rounds_test(name):
    _syntax(name = name + "_syntax", anchor = "@linux_source//:Makefile", tags = ["manual"])
    tests = []
    for rounds, consumers in [(0, []), (1, ["true"]), (2, ["false", "true"])]:
        kernel = name + "_kernel%d" % rounds
        kernel_build(
            name = kernel,
            config = "//native:config",
            syntax = ":" + name + "_syntax",
            rounds = rounds,
            schedule_steps = 0,
            tags = ["manual"],
        )
        test = name + "_%d" % rounds
        _rounds_test(name = test, target_under_test = ":" + kernel, rounds = rounds, consumers = consumers)
        tests.append(":" + test)
    native.test_suite(name = name, tests = tests)
