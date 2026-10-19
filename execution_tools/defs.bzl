"""Declared tools for native Kconfig bootstrap and probes."""

EXECUTION_TOOLS_TYPE = Label("//execution_tools:toolchain_type")

def _alias(ctx, name, executable, commands, files, arguments = None):
    if name in commands:
        fail("Duplicate execution command: " + name)
    alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + name)
    if arguments == None:
        ctx.actions.symlink(output = alias, target_file = executable, is_executable = True)
    else:
        # APE and interpreter launchers may locate runtime files relative to
        # their own path. Invoke the provider executable under its real name.
        ctx.actions.write(alias, "#!" + commands["sh"].path + '\nexec "${KBUILD_EXEC_ROOT:?}/' + executable.path + '" ' + arguments + ' "$@"\n', is_executable = True)
    commands[name] = alias
    files.extend([alias, executable])

def _impl(ctx):
    commands, files, tools, transitive = {}, [], [], []
    for target, names in ctx.attr.commands.items():
        info = target[DefaultInfo]
        executable = info.files_to_run.executable
        if executable == None:
            candidates = info.files.to_list()
            if len(candidates) != 1:
                fail("Execution command must supply one executable: %s" % target.label)
            executable = candidates[0]
        else:
            tools.append(info.files_to_run)
        transitive.extend([info.files, info.default_runfiles.files])
        for name in names.split(" "):
            if not name or "/" in name or name in commands:
                fail("Invalid or duplicate execution command: " + name)
            alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + name)
            ctx.actions.symlink(output = alias, target_file = executable, is_executable = True)
            commands[name] = alias
            files.append(alias)
    coreutils = ctx.toolchains["@bazel_lib//lib:coreutils_toolchain_type"].coreutils_info.bin
    files.append(coreutils)
    for name in ctx.attr.coreutils:
        if name in commands:
            fail("Duplicate execution command: " + name)
        alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + name)
        ctx.actions.symlink(output = alias, target_file = coreutils, is_executable = True)
        commands[name] = alias
        files.append(alias)
    runtime_commands, runtime_files = dict(commands), list(files)
    runtime_tools, runtime_transitive = list(tools), list(transitive)
    environment, path_environment = {}, {}
    for type_name, field, command, tool_field, env_field in [
        ("@rules_m4//m4:toolchain_type", "m4_toolchain", "m4", "m4_tool", "m4_env"),
        ("@rules_bison//bison:toolchain_type", "bison_toolchain", "bison", "bison_tool", "bison_env"),
        ("@rules_flex//flex:toolchain_type", "flex_toolchain", "flex", "flex_tool", "flex_env"),
    ]:
        provider = getattr(ctx.toolchains[type_name], field)
        tool = getattr(provider, tool_field)
        tools.append(tool)
        transitive.append(provider.all_files)
        alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + command)
        ctx.actions.symlink(output = alias, target_file = tool.executable, is_executable = True)
        commands[command] = alias
        files.append(alias)
        for name, value in getattr(provider, env_field).items():
            if name in ["M4", "BISON_PKGDATADIR", "M4_SYSCMD_SHELL"]:
                path_environment[name] = value
            else:
                environment[name] = value
    runtime = ctx.attr._python_runtime[platform_common.ToolchainInfo].py3_runtime
    if runtime.interpreter == None:
        fail("Native configuration requires a declared Python interpreter")
    files.append(runtime.interpreter)
    transitive.append(runtime.files)

    # Keep Python's runtime-relative standard library reachable through the
    # declared interpreter path, including nested native Make invocations.
    commands["python3"] = runtime.interpreter
    runtime_commands["python3"] = runtime.interpreter
    path_environment.update(M4 = commands["m4"].path, M4_SYSCMD_SHELL = commands["sh"].path)
    closure = depset(files, transitive = transitive)
    python_files = depset([runtime.interpreter], transitive = [runtime.files])
    runtime_closure = depset(runtime_files, transitive = runtime_transitive + [python_files])
    bootstrap_commands, bootstrap_tools = dict(commands), list(tools)
    for target, names in ctx.attr.build_commands.items():
        info = target[DefaultInfo]
        executable = info.files_to_run.executable
        if executable == None:
            candidates = info.files.to_list()
            if len(candidates) != 1:
                fail("Execution command must supply one executable: %s" % target.label)
            executable = candidates[0]
        else:
            tools.append(info.files_to_run)
        transitive.extend([info.files, info.default_runfiles.files])
        for name in names.split(" "):
            _alias(ctx, name, executable, commands, files)
    perl = ctx.toolchains["@rules_perl//perl:exec_toolchain_type"].perl_runtime
    if perl.perlopt:
        fail("Kernel tools require an interpreter with no implicit Perl options")
    _alias(ctx, "perl", perl.interpreter, commands, files, arguments = "")
    transitive.append(perl.runtime)
    for type_name, names in [
        ("@rules_gzip//gzip/toolchain/gzip:type", ["gzip"]),
        ("@rules_bzip2//bzip2/toolchain/bzip2:type", ["bzip2"]),
        ("@rules_xz//xz/toolchain/xz:type", ["xz", "lzma"]),
    ]:
        provider = ctx.toolchains[type_name]
        tools.append(provider.run)
        transitive.extend([provider.default.files, provider.default.default_runfiles.files])
        for name in names:
            _alias(ctx, name, provider.executable, commands, files, arguments = "--format=lzma" if name == "lzma" else "")
    zstd = ctx.toolchains["@bazel_lib//lib:zstd_toolchain_type"]
    transitive.append(zstd.default.files)
    _alias(ctx, "zstd", zstd.zstdinfo.binary, commands, files)
    return [platform_common.ToolchainInfo(execution = struct(
        commands = runtime_commands,
        files = runtime_closure,
        tools = runtime_tools,
        bin_dir = commands["sh"].dirname,
        shell = commands["sh"],
        python = runtime.interpreter,
        python_files = python_files,
        environment = {},
        path_environment = {},
        bootstrap_commands = bootstrap_commands,
        bootstrap_files = closure,
        bootstrap_tools = bootstrap_tools,
        bootstrap_environment = environment,
        bootstrap_path_environment = path_environment,
        build_commands = commands,
        build_files = depset(files, transitive = transitive),
        build_tools = tools,
    )), DefaultInfo(files = closure)]

kernel_execution_tools = rule(
    implementation = _impl,
    attrs = {
        "commands": attr.label_keyed_string_dict(cfg = "exec", allow_files = True, mandatory = True),
        "build_commands": attr.label_keyed_string_dict(cfg = "exec", allow_files = True),
        "coreutils": attr.string_list(),
        "_python_runtime": attr.label(default = "@rules_python//python:current_py_toolchain", cfg = "exec"),
    },
    toolchains = [
        "@bazel_lib//lib:coreutils_toolchain_type",
        "@bazel_lib//lib:zstd_toolchain_type",
        "@rules_perl//perl:exec_toolchain_type",
        "@rules_gzip//gzip/toolchain/gzip:type",
        "@rules_bzip2//bzip2/toolchain/bzip2:type",
        "@rules_xz//xz/toolchain/xz:type",
        "@rules_m4//m4:toolchain_type",
        "@rules_bison//bison:toolchain_type",
        "@rules_flex//flex:toolchain_type",
    ],
)
