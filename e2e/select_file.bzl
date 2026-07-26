"""Private projections used by the end-to-end test workspace."""

visibility("private")

def _select_file_impl(ctx):
    matches = [
        file
        for file in ctx.attr.src[DefaultInfo].files.to_list()
        if file.basename == ctx.attr.basename
    ]
    if len(matches) != 1:
        fail(
            "%s expected exactly one file named %s from %s, got %d" %
            (ctx.label, ctx.attr.basename, ctx.attr.src.label, len(matches)),
        )
    return [DefaultInfo(files = depset(matches))]

select_file = rule(
    implementation = _select_file_impl,
    attrs = {
        "basename": attr.string(mandatory = True),
        "src": attr.label(mandatory = True),
    },
)
