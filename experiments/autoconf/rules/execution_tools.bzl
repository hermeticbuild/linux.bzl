EXECUTION_TOOLS_TYPE = "//:unused_execution_type"

def execution_tools(ctx):
    return ctx.execution

def run_recipe(ctx, execution, command, arguments = [], tools = [], **kwargs):
    env = {"LC_ALL": "C", "LANG": "C", "TZ": "UTC", "PYTHONNOUSERSITE": "1"}
    env.update(execution.environment)
    env.update(kwargs.pop("env", {}))
    prefix = 'set -eu\nexport KBUILD_EXEC_ROOT="$PWD"\nexport PATH="$PWD/%s"\nexport CONFIG_SHELL="$PWD/%s"\nexport TMPDIR="$PWD"\n' % (execution.bin_dir, execution.shell.path)
    ctx.actions.run(
        executable = execution.shell,
        arguments = ["-c", prefix + command, "kbuild"] + arguments,
        tools = tools + execution.tools + execution.files.to_list(),
        env = env,
        use_default_shell_env = False,
        **kwargs
    )
