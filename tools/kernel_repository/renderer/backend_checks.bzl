"""Exercise the selected compiler and ELF linker for every requested backend."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load(":multi_platform.bzl", "KernelPlatformsInfo", "kernel_multi_platform")

def _check_impl(ctx):
    cc = find_cc_toolchain(ctx)
    features = cc_common.configure_features(ctx = ctx, cc_toolchain = cc)
    flags = cc_common.get_memory_inefficient_command_line(
        feature_configuration = features,
        action_name = ACTION_NAMES.c_compile,
        variables = cc_common.create_compile_variables(cc_toolchain = cc, feature_configuration = features),
    )
    source = ctx.actions.declare_file(ctx.label.name + ".c")
    ctx.actions.write(source, "int entry(void) { return 0; }\n")
    report = ctx.actions.declare_file(ctx.label.name + ".json")
    ld = [f.path for f in cc.all_files.to_list() if f.basename == "ld.lld"]
    if len(ld) != 1:
        fail("Expected one selected ELF linker: %s" % ld)
    ctx.actions.run(
        executable = "/usr/bin/python3",
        arguments = [
            ctx.file._runner.path,
            "--cc",
            cc_common.get_tool_for_action(feature_configuration = features, action_name = ACTION_NAMES.c_compile),
            "--ld",
            ld[0],
            "--source",
            source.path,
            "--flags",
            json.encode(flags),
            "--out",
            report.path,
        ],
        inputs = depset([ctx.file._runner, source], transitive = [cc.all_files]),
        outputs = [report],
        mnemonic = "KernelBackendCheck",
    )
    return [DefaultInfo(files = depset([report]))]

_check = rule(implementation = _check_impl, toolchains = use_cc_toolchain(), fragments = ["cpp"], attrs = {
    "_runner": attr.label(default = "//rules:backend_check.py", allow_single_file = True),
})

def _test_impl(ctx):
    aliases = {
        "kernel-backends/" + cpu + ".json": files.to_list()[0]
        for cpu, files in ctx.attr.checks[KernelPlatformsInfo].files.items()
    }
    script = ctx.actions.declare_file(ctx.label.name + ".sh")
    paths = ['"$TEST_SRCDIR/$TEST_WORKSPACE/%s"' % path for path in sorted(aliases)]
    ctx.actions.write(script, '#!/bin/sh\nexec /usr/bin/python3 "$TEST_SRCDIR/$TEST_WORKSPACE/%s" --check %s\n' %
                              (ctx.file._runner.short_path, " ".join(paths)), is_executable = True)
    return [DefaultInfo(executable = script, runfiles = ctx.runfiles(files = [ctx.file._runner], symlinks = aliases))]

_test = rule(implementation = _test_impl, test = True, attrs = {
    "checks": attr.label(providers = [KernelPlatformsInfo]),
    "_runner": attr.label(default = "//rules:backend_check.py", allow_single_file = True),
})

# Fixed package fixtures or action helpers, not target-naming macros.
# buildifier: disable=unnamed-macro
def backend_checks():
    _check(name = "check")
    kernel_multi_platform(name = "all", target = ":check")
    _test(name = "all_test", checks = ":all")
