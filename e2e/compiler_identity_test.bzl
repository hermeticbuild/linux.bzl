"""Test-only projection of the compiler evidence from a mapped kernel."""

_COMPILER_PROBE_SUFFIXES = [
    ".probe-plan",
    ".probe-results-target",
    ".kconfig-probe-plan",
    ".kconfig-probe-results-target",
]

def _compiler_probe_artifacts_impl(ctx):
    selected = []
    for suffix in _COMPILER_PROBE_SUFFIXES:
        matches = [
            artifact
            for artifact in ctx.attr.kernel[OutputGroupInfo].probes.to_list()
            if artifact.basename.endswith(suffix)
        ]
        if len(matches) != 1:
            fail("%s: got %d probe artifacts ending in %r, want 1" % (ctx.label, len(matches), suffix))
        selected.append(matches[0])
    return [DefaultInfo(
        files = depset(selected),
        runfiles = ctx.runfiles(files = selected),
    )]

compiler_probe_artifacts = rule(
    implementation = _compiler_probe_artifacts_impl,
    attrs = {
        "kernel": attr.label(mandatory = True),
    },
)
