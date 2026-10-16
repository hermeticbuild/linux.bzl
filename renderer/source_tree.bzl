"""Source tree with interpreter paths supplied by the execution toolchain."""

load(":execution_tools.bzl", "EXECUTION_TOOLS_TYPE", "execution_tools")

KernelSourceTreeInfo = provider(doc = "Kernel Source Tree Info.", fields = ["root", "logical", "files", "originals"])

def _impl(ctx):
    execution = execution_tools(ctx)
    originals = ctx.attr.sources[DefaultInfo].files
    prefix = ctx.file.anchor.dirname + "/"
    logical = {f.path[len(prefix):]: f for f in originals.to_list() if f.path.startswith(prefix)}
    manifest = ctx.actions.declare_file(ctx.label.name + ".json")
    output = ctx.actions.declare_directory(ctx.label.name + ".tree")
    ctx.actions.write(manifest, json.encode({
        "sources": {p: f.path for p, f in logical.items()},
        "commands": {n: f.path for n, f in execution.commands.items()},
    }))
    ctx.actions.run(
        executable = execution.python,
        arguments = [ctx.file._prepare.path, manifest.path, output.path],
        inputs = depset([manifest, ctx.file._prepare], transitive = [originals, execution.files]),
        outputs = [output],
        mnemonic = "KbuildSourceTree",
    )
    files = depset([output], transitive = [execution.files])
    return [KernelSourceTreeInfo(root = output, logical = logical, files = files, originals = originals), DefaultInfo(files = files)]

kernel_source_tree = rule(implementation = _impl, toolchains = [EXECUTION_TOOLS_TYPE], attrs = {
    "sources": attr.label(mandatory = True),
    "anchor": attr.label(allow_single_file = True, mandatory = True),
    "_prepare": attr.label(default = "//rules:prepare_source.py", allow_single_file = True, cfg = "exec"),
})
