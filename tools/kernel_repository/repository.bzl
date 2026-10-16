"""Generate a kernel repository using one prebuilt, statically linked tool."""

def _kernel_repository_impl(ctx):
    source = ctx.path(ctx.attr.source).realpath.dirname
    tool = ctx.path(ctx.attr.generator)

    # These are the actual inputs to the native parsers and source catalogue.
    # A local checkout changing must invalidate generation, not keep stale BUILD
    # files. Tool-label changes are tracked automatically by repository rules.
    ctx.watch_tree(source)
    arguments = [tool, "generate", "--linux", source, "--out", ctx.path(".")]
    if ctx.attr.all_backends:
        arguments.append("--all-backends")
    result = ctx.execute(
        arguments,
        timeout = ctx.attr.timeout,
        environment = {
            "PATH": "",
            "PYTHONPATH": "",
            "PYTHONHOME": "",
            "LC_ALL": "C",
        },
        quiet = False,
    )
    if result.return_code:
        fail("Kernel repository generation failed:\n" + result.stderr)

kernel_repository = repository_rule(
    implementation = _kernel_repository_impl,
    attrs = {
        "source": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "The kernel checkout's root Makefile. Its containing tree is watched.",
        ),
        "generator": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "Prebuilt kernel-repository executable, not a build-action output.",
        ),
        "all_backends": attr.bool(default = False),
        "timeout": attr.int(default = 600),
    },
    doc = "Parse Make/Kconfig and render symbolic BUILD files at repository fetch time.",
)
