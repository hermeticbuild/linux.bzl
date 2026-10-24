"""A test C/C++ toolchain whose compiler executable is produced by an action."""

load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:cc_toolchain_config_lib.bzl", "env_entry", "env_set", "feature", "flag_group", "flag_set", "tool_path")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/toolchains:cc_toolchain_config_info.bzl", "CcToolchainConfigInfo")
load("//execution_tools:defs.bzl", "EXECUTION_TOOLS_TYPE")
load(":compiler.bzl", "KernelCompilerInfo")

TestCompilerInfo = provider("Action-built test compiler and its selected host toolchain.", fields = ["executable", "host"])

def _driver_impl(ctx):
    host = ctx.attr.host[KernelCompilerInfo]
    output = ctx.actions.declare_file(ctx.label.name)
    obj = ctx.actions.declare_file(ctx.label.name + ".o")
    ctx.actions.run(
        executable = host.roles["CC"],
        arguments = host.context.spec["compile_flags"] + ["-DREAL_CC=" + json.encode(host.roles["CC"]), "-c", ctx.file.src.path, "-o", obj.path],
        inputs = depset([ctx.file.src], transitive = [host.files]),
        outputs = [obj],
        tools = host.tools,
        env = host.context.spec["compile_env"],
        mnemonic = "BuildTestCompiler",
    )
    ctx.actions.run(
        executable = host.context.spec["ld"],
        arguments = host.context.spec["link_flags"] + [obj.path, "-o", output.path],
        inputs = depset([obj], transitive = [host.files]),
        outputs = [output],
        tools = host.tools,
        env = host.context.spec["link_env"],
        mnemonic = "BuildTestCompiler",
    )
    return [
        DefaultInfo(executable = output, files = depset([output], transitive = [host.files])),
        TestCompilerInfo(executable = output, host = host),
    ]

action_built_compiler = rule(implementation = _driver_impl, executable = True, attrs = {
    "src": attr.label(allow_single_file = True, mandatory = True),
    "host": attr.label(default = "//native:compiler", cfg = "exec", providers = [KernelCompilerInfo]),
}, toolchains = [EXECUTION_TOOLS_TYPE])

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
        features = [
            feature(
                name = name,
                enabled = True,
                flag_sets = [flag_set(actions = actions, flag_groups = [flag_group(flags = compiler.host.context.spec[flags])])],
                env_sets = [env_set(actions = actions, env_entries = [env_entry(key = key, value = value) for key, value in compiler.host.context.spec[environment].items()])],
            )
            for name, actions, flags, environment in [
                ("declared_compile_context", [ACTION_NAMES.c_compile, ACTION_NAMES.cpp_compile, ACTION_NAMES.assemble, ACTION_NAMES.preprocess_assemble], "compile_flags", "compile_env"),
                ("declared_link_context", [ACTION_NAMES.cpp_link_executable, ACTION_NAMES.cpp_link_dynamic_library], "link_flags", "link_env"),
            ]
        ],
    )]

action_built_toolchain_config = rule(implementation = _config_impl, attrs = {
    "driver": attr.label(providers = [TestCompilerInfo], mandatory = True),
}, provides = [CcToolchainConfigInfo])
