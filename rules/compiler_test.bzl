"""A test C/C++ toolchain whose compiler executable is produced by an action."""

load("@rules_cc//cc:cc_toolchain_config_lib.bzl", "tool_path")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/toolchains:cc_toolchain_config_info.bzl", "CcToolchainConfigInfo")
load(":compiler.bzl", "KernelCompilerInfo")

TestCompilerInfo = provider("Action-built test compiler and its selected host toolchain.", fields = ["executable", "host"])

def _driver_impl(ctx):
    host = ctx.attr.host[KernelCompilerInfo]
    if not host.roles["CC"].startswith("/"):
        fail("This local driver fixture requires an absolute host compiler path")
    output = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.run(
        executable = host.roles["CC"],
        arguments = host.context.spec["compile_flags"] + ["-DREAL_CC=" + json.encode(host.roles["CC"]), ctx.file.src.path, "-o", output.path],
        inputs = depset([ctx.file.src], transitive = [host.files]),
        outputs = [output],
        env = dict(host.context.spec["compile_env"], PATH = host.roles["LD"].rpartition("/")[0]),
        mnemonic = "BuildTestCompiler",
    )
    return [
        DefaultInfo(executable = output, files = depset([output], transitive = [host.files])),
        TestCompilerInfo(executable = output, host = host),
    ]

action_built_compiler = rule(implementation = _driver_impl, executable = True, attrs = {
    "src": attr.label(allow_single_file = True, mandatory = True),
    "host": attr.label(default = "//:compiler", cfg = "exec", providers = [KernelCompilerInfo]),
})

def _config_impl(ctx):
    compiler = ctx.attr.driver[TestCompilerInfo]
    paths = {"gcc": compiler.executable.path, "cpp": compiler.executable.path}
    paths.update({name.lower(): compiler.host.roles[name] for name in ["LD", "AR", "NM", "OBJCOPY", "OBJDUMP", "STRIP"]})
    return [cc_common.create_cc_toolchain_config_info(
        ctx = ctx,
        toolchain_identifier = "action-built-compiler-test",
        host_system_name = "local",
        target_system_name = "local",
        target_cpu = "k8",
        target_libc = "local",
        compiler = "action-built-test",
        abi_version = "local",
        abi_libc_version = "local",
        tool_paths = [tool_path(name = name, path = path) for name, path in paths.items()],
    )]

action_built_toolchain_config = rule(implementation = _config_impl, attrs = {
    "driver": attr.label(providers = [TestCompilerInfo], mandatory = True),
}, provides = [CcToolchainConfigInfo])
