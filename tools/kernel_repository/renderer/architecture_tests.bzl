"""Analysis tests for one symbolic export across configured platforms."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load(":architecture.bzl", "KernelArchitectureInfo")
load(":compiler_capabilities.bzl", "kernel_compiler_capabilities_check", "kernel_compiler_capabilities_test")
load(":expressions.bzl", "evaluate")
load(":image.bzl", "kbuild_resolved_build")
load(":multi_platform.bzl", "KernelPlatformsInfo", "SUPPORTED_KERNEL_CPUS", "kernel_multi_platform")
load(":rules.bzl", "KernelConfigInfo", "kernel_config")
load(":toolchain.bzl", "KernelToolchainInfo")

SnapshotInfo = provider(doc = "Snapshot Info.", fields = ["architecture", "values", "flags"])
_ACTIVE = str(Label("//:active_config"))

# Bazel callback signatures include inputs this implementation does not use.
# buildifier: disable=unused-variable
def _select(settings, attr):
    return {_ACTIVE: str(attr.config)}

_transition = transition(implementation = _select, inputs = [], outputs = [_ACTIVE])

def _snapshot_impl(ctx):
    config = ctx.attr.config[KernelConfigInfo]
    setup = ctx.attr._setup[KernelToolchainInfo]
    flags = {name: evaluate([{"op": "reference", "name": [{"op": "literal", "value": name}]}], setup.variables, setup.sources) for name in ["KBUILD_CFLAGS", "KBUILD_CPPFLAGS", "LINUXINCLUDE", "LDFLAGS_vmlinux"]}
    out = ctx.actions.declare_file(ctx.label.name + ".json")
    data = {"arch": config.architecture.arch, "srcarch": config.architecture.srcarch, "bits": config.architecture.bits, "values": config.values, "flags": flags, "config": config.config.path, "report": config.report.path}
    ctx.actions.write(out, json.encode(data))
    return [SnapshotInfo(architecture = config.architecture, values = config.values, flags = flags), DefaultInfo(files = depset([out, config.config, config.report]))]

snapshot = rule(implementation = _snapshot_impl, cfg = _transition, attrs = {
    "config": attr.label(providers = [KernelConfigInfo]),
    "_setup": attr.label(default = "//:toolchain", providers = [KernelToolchainInfo]),
    "_allowlist_function_transition": attr.label(default = "@bazel_tools//tools/allowlists/function_transition_allowlist"),
})

def _check_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[SnapshotInfo]
    asserts.equals(env, ctx.attr.arch, info.architecture.arch)
    asserts.equals(env, ctx.attr.srcarch, info.architecture.srcarch)
    asserts.equals(env, ctx.attr.bits, info.architecture.bits)
    asserts.equals(env, "y" if ctx.attr.bits == 64 else "n", info.values.get("64BIT", "n"))
    asserts.equals(env, "y", info.values.get(ctx.attr.symbol))
    asserts.true(env, "--target=" + ctx.attr.triple in info.flags["KBUILD_CPPFLAGS"], str(info.flags))
    asserts.true(env, "/arch/" + ctx.attr.srcarch + "/include" in info.flags["LINUXINCLUDE"], str(info.flags))
    asserts.true(env, ("-z max-page-size=0x200000" if ctx.attr.srcarch == "x86" else "--pic-veneer") in info.flags["LDFLAGS_vmlinux"], str(info.flags))
    for symbol in ["X86", "ARM64", "RISCV"]:
        if symbol != ctx.attr.symbol:
            asserts.equals(env, "n", info.values.get(symbol, "n"))
    return analysistest.end(env)

def _config_check_impl(ctx):
    env = analysistest.begin(ctx)
    config = analysistest.target_under_test(env)[KernelConfigInfo]
    asserts.equals(env, "riscv", config.architecture.srcarch)
    asserts.equals(env, "y", config.values.get("RISCV"))
    asserts.equals(env, "y", config.values.get("64BIT"))
    asserts.equals(env, "n", config.values.get("X86", "n"))
    return analysistest.end(env)

def _architecture_check_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[KernelArchitectureInfo]
    asserts.equals(env, "i386", info.arch)
    asserts.equals(env, "x86", info.srcarch)
    asserts.equals(env, 32, info.bits)
    return analysistest.end(env)

def _failure_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.message)
    return analysistest.end(env)

_expected_failure_test = analysistest.make(_failure_impl, expect_failure = True, config_settings = {"//command_line_option:platforms": [str(Label("//architecture_tests:x86_64"))]}, attrs = {"message": attr.string()})

_check_x86_64_test = analysistest.make(_check_impl, config_settings = {"//command_line_option:platforms": [str(Label("//architecture_tests:x86_64"))]}, attrs = {"arch": attr.string(), "srcarch": attr.string(), "bits": attr.int(), "symbol": attr.string(), "triple": attr.string()})

_check_aarch64_test = analysistest.make(_check_impl, config_settings = {"//command_line_option:platforms": [str(Label("//architecture_tests:aarch64"))]}, attrs = {"arch": attr.string(), "srcarch": attr.string(), "bits": attr.int(), "symbol": attr.string(), "triple": attr.string()})

_check_riscv64_test = analysistest.make(_config_check_impl, config_settings = {"//command_line_option:platforms": [str(Label("//architecture_tests:riscv64"))]}, attrs = {"arch": attr.string(), "srcarch": attr.string(), "bits": attr.int(), "symbol": attr.string(), "triple": attr.string()})

_unsupported_cpu_test = analysistest.make(_failure_impl, expect_failure = True, config_settings = {"//command_line_option:platforms": [str(Label("//architecture_tests:wasm32"))]}, attrs = {"message": attr.string()})
_x86_32_test = analysistest.make(_architecture_check_impl, config_settings = {"//command_line_option:platforms": [str(Label("//architecture_tests:x86_32"))]})

# Fixed package fixtures or action helpers, not target-naming macros.
# buildifier: disable=unnamed-macro
def architecture_tests():
    kernel_compiler_capabilities_check(name = "compiler_capability_checks", tags = ["manual"])
    kernel_multi_platform(name = "all_compiler_capability_checks", target = ":compiler_capability_checks", tags = ["manual"])
    kernel_compiler_capabilities_test(name = "compiler_capabilities_test", checks = ":all_compiler_capability_checks")
    kernel_multi_platform(name = "all_configs", target = "//multi_platform:config")
    _multi_platform_test(name = "multi_platform_test", target_under_test = ":all_configs")
    native.platform(name = "x86_32", constraint_values = ["@platforms//os:linux", "@platforms//cpu:x86_32"])
    native.platform(name = "wasm32", constraint_values = ["@platforms//os:linux", "@platforms//cpu:wasm32"])
    _x86_32_test(name = "x86_32_test", target_under_test = "//:architecture")
    _unsupported_cpu_test(name = "unsupported_cpu_test", target_under_test = "//:architecture", message = "Kernel requires one supported CPU constraint")

    kernel_config(name = "config", options = "//options:options", requested = {"EXPERT": "y", "STACKPROTECTOR": "n", "UNWINDER_FRAME_POINTER": "y"})
    snapshot(name = "snapshot", config = ":config")
    kbuild_resolved_build(name = "object", config = ":config", catalog = "//:catalog", roots = ["lib/oid_registry.o"], tags = ["manual"])
    for cpu, arch, srcarch, symbol, triple in [("x86_64", "x86_64", "x86", "X86", "x86_64-linux-gnu"), ("aarch64", "arm64", "arm64", "ARM64", "aarch64-linux-gnu"), ("riscv64", "riscv", "riscv", "RISCV", "riscv64-linux-gnu")]:
        native.platform(name = cpu, constraint_values = ["@platforms//os:linux", "@platforms//cpu:" + cpu])
        check = {"x86_64": _check_x86_64_test, "aarch64": _check_aarch64_test, "riscv64": _check_riscv64_test}[cpu]
        check(name = cpu + "_test", target_under_test = ":config" if cpu == "riscv64" else ":snapshot", arch = arch, srcarch = srcarch, bits = 64, symbol = symbol, triple = triple)
    kernel_config(name = "conflicting_width", options = "//options:options", requested = {"64BIT": "n"}, tags = ["manual"])
    _expected_failure_test(name = "conflicting_width_test", target_under_test = ":conflicting_width", message = "conflicts with platform CPU")

def _multi_platform_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[KernelPlatformsInfo]
    asserts.equals(env, SUPPORTED_KERNEL_CPUS, sorted(info.targets.keys()))
    outputs = {}
    for cpu, target in info.targets.items():
        config = target[KernelConfigInfo]
        asserts.equals(env, cpu, config.architecture.cpu)
        asserts.equals(env, "y" if config.architecture.bits == 64 else "n", config.values.get("64BIT", "n"))
        asserts.true(env, config.config.path not in outputs, "Configurations share an output path")
        outputs[config.config.path] = True
        asserts.true(env, config.config in info.files[cpu].to_list())
    return analysistest.end(env)

_multi_platform_test = analysistest.make(_multi_platform_impl)
