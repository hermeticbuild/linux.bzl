"""Check Aya's compiler matrix across target and execution configurations."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")

_CompilersInfo = provider(
    doc = "Compiler identities from the actual target and exec-configured toolchains.",
    fields = {
        "target": "Target C compiler identity.",
        "host": "Execution-host C compiler identity.",
    },
)

def _compiler_probe_impl(ctx):
    return [_CompilersInfo(
        target = find_cpp_toolchain(ctx).compiler,
        host = ctx.attr._host_cc_toolchain[cc_common.CcToolchainInfo].compiler,
    )]

_compiler_probe = rule(
    implementation = _compiler_probe_impl,
    attrs = {
        "_host_cc_toolchain": attr.label(
            default = Label("@rules_cc//cc:current_cc_toolchain"),
            cfg = config.exec(exec_group = "host_cc"),
            providers = [cc_common.CcToolchainInfo],
        ),
    },
    exec_groups = {"host_cc": exec_group(toolchains = use_cc_toolchain())},
    toolchains = use_cc_toolchain(),
)

def _compiler_test_impl(ctx):
    env = analysistest.begin(ctx)
    compilers = analysistest.target_under_test(env)[_CompilersInfo]
    asserts.equals(env, ctx.attr.expected_target, compilers.target, "target compiler")
    asserts.equals(env, ctx.attr.expected_host, compilers.host, "execution host compiler")
    return analysistest.end(env)

def _compiler_test_for_platform(platform):
    return analysistest.make(
        _compiler_test_impl,
        attrs = {
            "expected_target": attr.string(mandatory = True),
            "expected_host": attr.string(mandatory = True),
        },
        config_settings = {"//command_line_option:platforms": str(Label(platform))},
    )

_kernel_x86_64_test = _compiler_test_for_platform("//:kernel_x86_64")
_kernel_aarch64_test = _compiler_test_for_platform("//:kernel_aarch64")
_musl_x86_64_test = _compiler_test_for_platform("@rules_rs//rs/platforms:x86_64-unknown-linux-musl")
_musl_aarch64_test = _compiler_test_for_platform("@rules_rs//rs/platforms:aarch64-unknown-linux-musl")
_bpf_test = _compiler_test_for_platform("@llvm//platforms:none_bpfel")

def compiler_toolchain_tests(name):
    """Test the current matrix selector without building any kernel artifacts.

    Args:
        name: Name of the test suite and prefix for its analysis tests.
    """
    probe = name + "_probe"
    _compiler_probe(name = probe, tags = ["manual"])
    selected = select({
        ":gcc_selected": "gcc",
        "//conditions:default": "clang",
    })
    tests = []
    for suffix, test_rule, kernel in [
        ("kernel_x86_64", _kernel_x86_64_test, True),
        ("kernel_aarch64", _kernel_aarch64_test, True),
        ("musl_x86_64", _musl_x86_64_test, False),
        ("musl_aarch64", _musl_aarch64_test, False),
        ("bpf", _bpf_test, False),
    ]:
        test_name = name + "_" + suffix
        test_rule(
            name = test_name,
            target_under_test = ":" + probe,
            expected_target = selected if kernel else "clang",
            expected_host = selected,
        )
        tests.append(":" + test_name)
    native.test_suite(name = name, tests = tests)
