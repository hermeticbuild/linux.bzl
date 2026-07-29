"""Minimal executable test wrapper for the nested bzlmod fixture."""

def _integration_test_impl(ctx):
    executable = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.symlink(
        output = executable,
        target_file = ctx.file.src,
        is_executable = True,
    )
    runfiles = ctx.runfiles(files = ctx.files.data)
    for dependency in ctx.attr.data:
        runfiles = runfiles.merge(dependency[DefaultInfo].default_runfiles)
    return [
        DefaultInfo(
            executable = executable,
            runfiles = runfiles,
        ),
        RunEnvironmentInfo(
            inherited_environment = [
                "BAZEL",
                "PATH",
                "USE_BAZEL_VERSION",
            ],
        ),
    ]

integration_test = rule(
    implementation = _integration_test_impl,
    attrs = {
        "data": attr.label_list(allow_files = True),
        "src": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
    },
    test = True,
)
