"""Read declared LLVM release metadata; never execute a bootstrap compiler."""

load("@llvm-project//:vars.bzl", "LLVM_VERSION")

def _impl(ctx):
    index = json.decode(ctx.read(ctx.attr.index))
    seed = LLVM_VERSION if LLVM_VERSION in index["latest_by_llvm_version"] else index["default_prebuilt_llvm_version"]
    ctx.file("BUILD.bazel", 'exports_files(["versions.bzl"])\n')
    ctx.file("versions.bzl", "LLVM_SOURCE_VERSION = %r\nLLVM_SEED_VERSION = %r\n" % (LLVM_VERSION, seed))

llvm_compiler_versions = repository_rule(implementation = _impl, attrs = {
    "index": attr.label(default = "@llvm//extensions:llvm_toolchain_minimal_index.json", allow_single_file = True),
})
