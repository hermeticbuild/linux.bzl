"""Expose the configured LZ4 CLI under its legacy command name."""

def _lz4c_tool_impl(ctx):
    # argv[0] selects LZ4's legacy parser, including Linux 5.10's -c1 spelling.
    # https://github.com/lz4/lz4/blob/ebb370ca83af193212df4dcbadcc5d87bc0de2f0/programs/lz4cli.c#L445
    executable = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = executable,
        target_file = ctx.executable.binary,
        is_executable = True,
    )
    return [DefaultInfo(
        executable = executable,
        runfiles = ctx.runfiles(
            files = [ctx.executable.binary],
        ).merge(ctx.attr.binary[DefaultInfo].default_runfiles),
    )]

lz4c_tool = rule(
    implementation = _lz4c_tool_impl,
    attrs = {
        "binary": attr.label(
            default = Label("@lz4//programs:lz4"),
            executable = True,
            cfg = "exec",
        ),
    },
    executable = True,
)
