"""Declared execution utilities and action environment for native Kbuild recipes."""

EXECUTION_TOOLS_TYPE = Label("//execution_tools:toolchain_type")
KernelExecutionToolsInfo = provider(doc = "Kernel Execution Tools Info.", fields = ["commands", "files", "tools", "bin_dir", "shell", "python", "environment", "path_environment"])

def _alias(ctx, name, executable, commands, files, wrapper = False, arguments = ""):
    if name in commands:
        fail("Duplicate execution command: " + name)
    alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + name)
    if wrapper:
        ctx.actions.write(alias, "#!" + commands["sh"].path + '\nexec "${KBUILD_EXEC_ROOT:?}/' + executable.path + '" ' + arguments + ' "$@"\n', is_executable = True)
    else:
        ctx.actions.symlink(output = alias, target_file = executable, is_executable = True)
    commands[name] = alias
    files.extend([alias, executable])

def _impl(ctx):
    commands, files, tools = {}, [], []
    transitive = []
    for target, names in ctx.attr.commands.items():
        info = target[DefaultInfo]
        executable = info.files_to_run.executable
        if executable == None:
            candidates = info.files.to_list()
            if len(candidates) != 1:
                fail("Execution utility must expose one executable: %s" % target.label)
            executable = candidates[0]
        else:
            tools.append(info.files_to_run)
        transitive.extend([info.files, info.default_runfiles.files])
        for name in names.split(" "):
            if not name or "/" in name or name in commands:
                fail("Invalid or duplicate execution command: %s" % name)
            alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + name)
            ctx.actions.symlink(output = alias, target_file = executable, is_executable = True)
            commands[name] = alias
            files.append(alias)
    coreutils = ctx.toolchains["@bazel_lib//lib:coreutils_toolchain_type"].coreutils_info.bin
    files.append(coreutils)
    for name in ctx.attr.coreutils:
        if name in commands:
            fail("Duplicate execution command: %s" % name)
        alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + name)
        ctx.actions.symlink(output = alias, target_file = coreutils, is_executable = True)
        commands[name] = alias
        files.append(alias)
    runtime = ctx.attr._python_runtime[platform_common.ToolchainInfo].py3_runtime
    if runtime.interpreter == None:
        fail("Execution tools require a declared Python runtime, not interpreter_path")
    transitive.append(runtime.files)
    for name in ["python", "python3"]:
        alias = ctx.actions.declare_file(ctx.label.name + "/bin/" + name)
        ctx.actions.symlink(output = alias, target_file = runtime.interpreter, is_executable = True)
        commands[name] = alias
        files.append(alias)
    perl = ctx.toolchains["@rules_perl//perl:exec_toolchain_type"].perl_runtime
    if perl.perlopt:
        fail("Execution tools require a Perl executable wrapper when perlopt is nonempty")
    alias = ctx.actions.declare_file(ctx.label.name + "/bin/perl")
    ctx.actions.symlink(output = alias, target_file = perl.interpreter, is_executable = True)
    commands["perl"] = alias
    files.extend([alias, perl.interpreter])
    transitive.append(perl.runtime)
    for name in ["sh", "bash"]:
        if name not in commands:
            fail("Execution tools require a declared %s" % name)
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
        _alias(ctx, command, tool.executable, commands, files)
        for name, value in getattr(provider, env_field).items():
            if name in ("M4", "BISON_PKGDATADIR", "M4_SYSCMD_SHELL"):
                path_environment[name] = value
            else:
                environment[name] = value
    for type_name, names in [
        ("@rules_gzip//gzip/toolchain/gzip:type", ["gzip"]),
        ("@rules_xz//xz/toolchain/xz:type", ["xz", "lzma"]),
        ("@rules_bzip2//bzip2/toolchain/bzip2:type", ["bzip2"]),
    ]:
        provider = ctx.toolchains[type_name]
        tools.append(provider.run)
        transitive.extend([provider.default.files, provider.default.default_runfiles.files])
        for name in names:
            # APE launchers use argv[0] to locate their sibling payload. A
            # direct alias changes it, so invoke the provider executable intact.
            _alias(ctx, name, provider.executable, commands, files, wrapper = True, arguments = "--format=lzma" if name == "lzma" else "")
    zstd = ctx.toolchains["@bazel_lib//lib:zstd_toolchain_type"]
    transitive.append(zstd.default.files)
    _alias(ctx, "zstd", zstd.zstdinfo.binary, commands, files)

    # Use the selected executable directly rather than Bison's runfiles-relative
    # M4 hint. Preserve the rest of each provider's runtime environment.
    path_environment["M4"] = commands["m4"].path
    path_environment["M4_SYSCMD_SHELL"] = commands["sh"].path
    closure = depset(files + [runtime.interpreter], transitive = transitive)
    info = KernelExecutionToolsInfo(
        commands = commands,
        files = closure,
        tools = tools,
        bin_dir = commands["sh"].dirname,
        shell = commands["sh"],
        python = runtime.interpreter,
        environment = environment,
        path_environment = path_environment,
    )
    return [platform_common.ToolchainInfo(execution = info), DefaultInfo(files = closure)]

kernel_execution_tools = rule(implementation = _impl, attrs = {
    "commands": attr.label_keyed_string_dict(cfg = "exec", allow_files = True, mandatory = True),
    "coreutils": attr.string_list(),
    "_python_runtime": attr.label(default = "@rules_python//python:current_py_toolchain", cfg = "exec"),
}, toolchains = [
    "@bazel_lib//lib:coreutils_toolchain_type",
    "@bazel_lib//lib:zstd_toolchain_type",
    "@rules_perl//perl:exec_toolchain_type",
    "@rules_m4//m4:toolchain_type",
    "@rules_bison//bison:toolchain_type",
    "@rules_flex//flex:toolchain_type",
    "@rules_gzip//gzip/toolchain/gzip:type",
    "@rules_xz//xz/toolchain/xz:type",
    "@rules_bzip2//bzip2/toolchain/bzip2:type",
])

def execution_tools(ctx):
    return ctx.toolchains[EXECUTION_TOOLS_TYPE].execution

def run_recipe(ctx, execution, command, arguments = [], tools = [], **kwargs):
    """Run with a declared shell and utilities; never inherit the host PATH."""
    env = {"LC_ALL": "C", "LANG": "C", "TZ": "UTC", "PYTHONNOUSERSITE": "1"}
    env.update(execution.environment)
    env.update(kwargs.pop("env", {}))
    prefix = 'set -eu\nexport KBUILD_EXEC_ROOT="$PWD"\nexport PATH="$PWD/%s"\nexport CONFIG_SHELL="$PWD/%s"\nexport TMPDIR="$PWD"\n' % (execution.bin_dir, execution.shell.path)
    for name, path in execution.path_environment.items():
        prefix += 'export %s="%s%s"\n' % (name, "" if path.startswith("/") else "$PWD/", path)
    ctx.actions.run(
        executable = execution.shell,
        arguments = ["-c", prefix + command, "kbuild"] + arguments,
        tools = tools + execution.tools + execution.files.to_list(),
        env = env,
        use_default_shell_env = False,
        **kwargs
    )

def _check_impl(ctx):
    execution = execution_tools(ctx)
    macro = ctx.actions.declare_file(ctx.label.name + ".m4")
    ctx.actions.write(macro, "syscmd(`printf m4-shell-ok')\n")
    grammar = ctx.actions.declare_file(ctx.label.name + ".y")
    lexer = ctx.actions.declare_file(ctx.label.name + ".l")
    parser = ctx.actions.declare_file(ctx.label.name + ".parser.c")
    scanner = ctx.actions.declare_file(ctx.label.name + ".scanner.c")
    report = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(grammar, "%token WORD\n%%\ninput: WORD;\n%%\n")
    ctx.actions.write(lexer, "%%\n[a-z]+ return 1;\n%%\n")
    run_recipe(
        ctx,
        execution,
        command = '''
test "$PATH" = "$PWD/%s"
printf 'declared tools\\n' | awk '{print $1}' | sed 's/declared/hermetic/' | grep -qx hermetic
printf '1 + 2\\n' | bc | grep -qx 3
printf 'payload\\n' | gzip -n -f -9 | gzip -dc | grep -qx payload
printf 'payload\\n' | xz -c | xz -dc | grep -qx payload
printf 'payload\\n' | lzma -c | xz --format=lzma -dc | grep -qx payload
printf 'payload\\n' | bzip2 -c | bzip2 -dc | grep -qx payload
printf 'payload\\n' | lzop -c | lzop -dc | grep -qx payload
printf 'payload\\n' | zstd -q -c | zstd -q -dc | grep -qx payload
python3 -I -c 'import json, pathlib; assert json.loads("[1]") == [1]'
perl -Mstrict -Mwarnings -e 'print "declared perl\\n"' | grep -qx 'declared perl'
m4 %s | grep -qx m4-shell-ok
bison -o %s %s
flex -o %s %s
test -s %s
test -s %s
printf 'PASS: declared utilities, Python runtime, Bison/Flex runtime data\\n' > %s
''' % (execution.bin_dir, macro.path, parser.path, grammar.path, scanner.path, lexer.path, parser.path, scanner.path, report.path),
        inputs = [macro, grammar, lexer],
        outputs = [parser, scanner, report],
        mnemonic = "KernelExecutionToolsCheck",
    )
    return [DefaultInfo(files = depset([report, parser, scanner]))]

kernel_execution_tools_check = rule(implementation = _check_impl, toolchains = [EXECUTION_TOOLS_TYPE])
