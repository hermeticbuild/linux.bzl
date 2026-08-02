"""Analysis tests for platform-first multi-architecture kernel facades."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:providers.bzl", "LinuxKernelInfo", "LinuxModuleSdkInfo")

visibility("private")

def _graph_fixture_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(out, ctx.attr.arch + "\n")
    return [
        DefaultInfo(files = depset([out])),
        LinuxKernelInfo(
            arch = ctx.attr.arch,
            version = "test",
            kernel_release = out,
            image = out,
            vmlinux = out,
            config = out,
            system_map = out,
        ),
        LinuxModuleSdkInfo(
            arch = ctx.attr.arch,
            module_symvers = out,
            modules = depset([out]),
            modules_builtin = out,
            modules_builtin_modinfo = out,
            modules_order = out,
        ),
        OutputGroupInfo(selected_profile = depset([out])),
    ]

multiarch_graph_fixture = rule(
    implementation = _graph_fixture_impl,
    attrs = {
        "arch": attr.string(mandatory = True),
    },
)

def _facade_provider_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)

    asserts.true(env, LinuxKernelInfo in target)
    asserts.true(env, LinuxModuleSdkInfo in target)
    asserts.true(env, OutputGroupInfo in target)
    asserts.equals(env, "armv7", target[LinuxKernelInfo].arch)
    asserts.equals(env, "armv7", target[LinuxModuleSdkInfo].arch)
    asserts.equals(env, 1, len(target[DefaultInfo].files.to_list()))
    asserts.equals(env, 1, len(target[OutputGroupInfo].selected_profile.to_list()))
    asserts.equals(
        env,
        target[DefaultInfo].files.to_list()[0],
        target[OutputGroupInfo].selected_profile.to_list()[0],
    )
    return analysistest.end(env)

facade_provider_test = analysistest.make(_facade_provider_test_impl)

def _projection_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    files = target[DefaultInfo].files.to_list()

    asserts.equals(env, 1, len(files))
    asserts.equals(env, "armv7_graph.txt", files[0].basename)
    return analysistest.end(env)

projection_test = analysistest.make(_projection_test_impl)
