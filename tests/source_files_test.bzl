"""Native Build files must not hide host-tool sources from Bazel."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")

def _impl(ctx):
    env = unittest.begin(ctx)
    prefix = ctx.file.anchor.dirname + "/"
    paths = {file.path.removeprefix(prefix): True for file in ctx.files.sources}
    for path in [
        "tools/bpf/resolve_btfids/Build",
        "tools/bpf/resolve_btfids/main.c",
        "tools/lib/bpf/Build",
        "tools/lib/bpf/btf.c",
        "tools/lib/subcmd/Build",
        "tools/lib/subcmd/exec-cmd.c",
        "tools/objtool/Build",
        "tools/objtool/check.c",
        "tools/objtool/arch/x86/Build",
        "tools/objtool/arch/x86/decode.c",
    ]:
        asserts.true(env, path in paths, "Missing native source: " + path)
    asserts.equals(env, [], [path for path in paths if path.rsplit("/", 1)[-1] == "BUILD.bazel"])
    return unittest.end(env)

source_files_test = unittest.make(_impl, attrs = {
    "anchor": attr.label(mandatory = True, allow_single_file = True),
    "sources": attr.label(mandatory = True),
})
