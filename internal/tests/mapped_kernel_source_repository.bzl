"""Composes the small execution fixture with authentic Kconfig host sources."""

def _files(rctx, root):
    rctx.watch_tree(root)
    pending = [(root, "")]
    files = {}
    for _ in range(64):
        if not pending:
            return files
        current = pending
        pending = []
        for directory, prefix in current:
            for child in directory.readdir():
                relative = prefix + child.basename
                if child.is_dir:
                    pending.append((child, relative + "/"))
                elif child.basename not in ["BUILD", "BUILD.bazel"]:
                    files[relative] = child
    fail("mapped kernel fixture source exceeds maximum directory depth")

def _impl(rctx):
    # Whole source directories retain Linux's own object/generator selection.
    # The fixture deliberately overrides fixdep and link-vmlinux for its
    # execution assertions; every other host-tool source remains authentic.
    native = rctx.path(rctx.attr.native_source).dirname
    files = {"scripts/" + name: file for name, file in _files(rctx, native.get_child("scripts")).items()}
    files.update(_files(rctx, rctx.path(rctx.attr.fixture_source).dirname))
    for name, file in files.items():
        rctx.symlink(file, name)
    rctx.file("BUILD.bazel", """load(%r, "linux_source_runfiles")
package(default_visibility = ["//visibility:public"])
exports_files(glob(["**"]))
filegroup(name = "all_files", srcs = glob(["**"]))
linux_source_runfiles(name = "source_runfiles", source_root = "Kconfig", source_files = [":all_files"])
""" % str(rctx.attr._source_runfiles_bzl))

mapped_kernel_source_repository = repository_rule(
    implementation = _impl,
    attrs = {
        "fixture_source": attr.label(mandatory = True, allow_single_file = True),
        "native_source": attr.label(mandatory = True, allow_single_file = True),
        "_source_runfiles_bzl": attr.label(default = Label("//internal:linux_source_runfiles.bzl")),
    },
)
