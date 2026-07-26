"""Analysis tests for the public Rust-for-Linux module rule."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:linux_module_actions.bzl", "linux_module_actions")
load("//internal:linux_modules.bzl", "linux_module")
load("//internal:providers.bzl", "LinuxModuleInfo", "LinuxModuleSdkInfo")

visibility("private")

def _fake_module_kernel_impl(ctx):
    module_symvers = ctx.actions.declare_file(ctx.label.name + ".Module.symvers")
    rustc = ctx.actions.declare_file(ctx.label.name + ".rustc")
    rustc_version_runner = ctx.actions.declare_file(ctx.label.name + ".rustcversionrun")
    rustc_cfg = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/include/generated/rustc_cfg")
    target_spec = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/scripts/target.json")
    rust_dir_anchor_file = ctx.actions.declare_file(ctx.label.name + ".rust_sdk/rust/.anchor")
    for output in [
        module_symvers,
        rustc,
        rustc_cfg,
        rustc_version_runner,
        rust_dir_anchor_file,
        target_spec,
    ]:
        ctx.actions.write(output, "")

    return [
        DefaultInfo(files = depset([module_symvers])),
        LinuxModuleSdkInfo(
            arch = "x86_64",
            config = struct(
                config_flags = {
                    "CONFIG_MODULES": "y",
                    "CONFIG_RUST": "y",
                },
                rustc_cfg = rustc_cfg,
            ),
            kernel_key = ctx.attr.kernel_key,
            module_symvers = module_symvers,
            rust = struct(
                compile_inputs = depset(),
                enabled = True,
                module_flags = [],
                objtool = None,
                objtree = target_spec.dirname.rsplit("/", 1)[0],
                objtree_anchor = struct(file = target_spec, parents = 1),
                rust_dir = rust_dir_anchor_file.dirname,
                rust_dir_anchor = struct(file = rust_dir_anchor_file, parents = 0),
                rustc = rustc,
                rustc_env = {},
                rustc_files = depset([rustc, rustc_version_runner]),
                rustc_version = "1.97.0",
                rustc_version_runner = rustc_version_runner,
                target_spec = target_spec,
            ),
            version = "6.18.39",
        ),
    ]

_fake_module_kernel = rule(
    implementation = _fake_module_kernel_impl,
    attrs = {
        "kernel_key": attr.string(mandatory = True),
    },
)

def _fake_linux_module_impl(ctx):
    ko = ctx.actions.declare_file(ctx.label.name + ".ko")
    module_symvers = ctx.actions.declare_file(ctx.label.name + ".Module.symvers")
    ctx.actions.write(ko, "")
    ctx.actions.write(module_symvers, "")
    return [
        DefaultInfo(files = depset([ko])),
        LinuxModuleInfo(
            kernel_key = ctx.attr.kernel_key,
            ko = ko,
            module_symvers = module_symvers,
        ),
    ]

_fake_linux_module = rule(
    implementation = _fake_linux_module_impl,
    attrs = {
        "kernel_key": attr.string(mandatory = True),
    },
)

def _module_dependency_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "built against a different configured kernel")
    return analysistest.end(env)

_module_dependency_failure_test = analysistest.make(
    _module_dependency_failure_test_impl,
    expect_failure = True,
)

def _module_command_flags_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-config",
            "kernel.config",
            "-objtool",
            "objtool",
            "-in",
            "module.raw.o",
            "-mode",
            "module",
            "-out",
            "module.o",
        ],
        linux_module_actions.objtool_args(
            "kernel.config",
            "objtool",
            "module.raw.o",
            "module.o",
            "module",
        ),
    )
    return unittest.end(env)

_module_command_flags_test = unittest.make(_module_command_flags_test_impl)

def linux_module_test_suite(name):
    kernel = name + "_kernel"
    dependency = name + "_foreign_dependency"
    module = name + "_module"

    _fake_module_kernel(
        name = kernel,
        kernel_key = "kernel-a",
        tags = ["manual"],
    )
    _fake_linux_module(
        name = dependency,
        kernel_key = "kernel-b",
        tags = ["manual"],
    )
    linux_module(
        name = module,
        deps = [":" + dependency],
        kernel = ":" + kernel,
        srcs = ["linux_modules_test_fixture.rs"],
        tags = ["manual"],
    )
    _module_dependency_failure_test(
        name = name + "_dependency_mismatch_test",
        target_under_test = ":" + module,
    )
    command_flags_test = name + "_command_flags_test"
    _module_command_flags_test(name = command_flags_test)

    native.test_suite(
        name = name,
        tests = [
            ":" + name + "_dependency_mismatch_test",
            ":" + command_flags_test,
        ],
    )
