"""Pinned generic exporter and source-only, lazily loaded Kbuild syntax."""

def _source_archive_impl(ctx):
    ctx.download_and_extract(
        url = ctx.attr.urls,
        integrity = ctx.attr.integrity,
        stripPrefix = ctx.attr.strip_prefix,
        type = ctx.attr.type,
    )
    for patch in ctx.attr.patches:
        ctx.patch(ctx.path(patch), strip = ctx.attr.patch_strip)

    # Build is a native Make input, but also matches BUILD on case-insensitive
    # filesystems. Give each such directory an explicit Bazel package without
    # changing its native input. POSIX find supplies recursive traversal on the
    # supported Linux/macOS hosts; repository path.readdir is shallow.
    find = ctx.path("/usr/bin/find")
    if not find.exists:
        fail("Kernel source packaging requires /usr/bin/find on the repository host")
    result = ctx.execute([find, "-L", ".", "-type", "f", "-iname", "build", "-print0"])
    if result.return_code:
        fail("Cannot enumerate native Build packages with %s (exit %d):\n%s" % (find, result.return_code, result.stderr))
    packages = {path.removeprefix("./").rpartition("/")[0]: True for path in result.stdout.split("\000") if path}
    root = str(ctx.path(".").realpath)
    written = {}
    for package in sorted(packages):
        if not package:
            continue
        parent = str(ctx.path(package).realpath)
        if not parent.startswith(root + "/"):
            fail("Native Build package escapes its source repository: " + package)
        if parent in written:
            continue
        build = package + "/BUILD.bazel"
        if ctx.path(build).exists:
            fail("Kernel source already contains Bazel package metadata: " + build)
        ctx.file(build, "load(%r, \"kernel_source_files\")\nkernel_source_files(name = \"sources\")\n" % str(ctx.attr._source_files), executable = False)
        written[parent] = True
    if ctx.path("BUILD.bazel").exists:
        fail("Kernel source already contains root Bazel package metadata")
    ctx.symlink(ctx.path(ctx.attr.build_file), "BUILD.bazel")

kernel_source_archive = repository_rule(
    implementation = _source_archive_impl,
    attrs = {
        "urls": attr.string_list(mandatory = True),
        "integrity": attr.string(mandatory = True),
        "strip_prefix": attr.string(),
        "type": attr.string(),
        "patches": attr.label_list(),
        "patch_strip": attr.int(default = 0),
        "build_file": attr.label(mandatory = True, allow_single_file = True),
        "_source_files": attr.label(default = "//rules:source_files.bzl"),
    },
)

def _tool_impl(ctx):
    if not ctx.attr.manifest:
        fail("Build //native:exporter_archive and supply its directory with --override_repository=" + ctx.name + "=<output>; released consumers must provide a manifest")
    manifest = json.decode(ctx.read(ctx.attr.manifest))
    if type(manifest) != "dict" or manifest.get("format") != 1 or type(manifest.get("platforms")) != "dict":
        fail("Exporter manifest must have format 1 and a platforms dictionary")
    os = {"linux": "linux", "mac os x": "macos"}.get(ctx.os.name.lower())
    cpu = {"amd64": "x86_64", "x86_64": "x86_64", "aarch64": "aarch64", "arm64": "aarch64"}.get(ctx.os.arch)
    platform = str(os) + "_" + str(cpu)
    if platform not in ["linux_aarch64", "linux_x86_64", "macos_aarch64", "macos_x86_64"]:
        fail("Unsupported exporter host: %s/%s" % (ctx.os.name, ctx.os.arch))
    artifact = manifest["platforms"].get(platform)
    if type(artifact) != "dict" or not artifact.get("url") or not artifact.get("integrity"):
        fail("Exporter manifest must pin a URL and integrity for " + platform)
    ctx.download_and_extract(url = artifact["url"], integrity = artifact["integrity"])
    if not ctx.path("kbuild-exporter").exists:
        fail("Exporter archive must contain kbuild-exporter at its root")
    ctx.file("BUILD.bazel", 'exports_files(["kbuild-exporter"], visibility = ["//visibility:public"])\n')

kernel_syntax_tool = repository_rule(
    implementation = _tool_impl,
    attrs = {"manifest": attr.label(allow_single_file = [".json"], doc = "Format 1 host URL/integrity manifest; omit only when bootstrapping with --override_repository.")},
)

def _syntax_impl(ctx):
    source = ctx.path(ctx.attr.source).realpath.dirname
    tool = ctx.path(ctx.attr.tool)
    ctx.watch_tree(source)
    ctx.watch(tool)
    result = ctx.execute(
        [tool, "--linux", source, "--out", ctx.path("export")],
        timeout = ctx.attr.timeout,
        environment = {"PATH": "", "PYTHONPATH": "", "PYTHONHOME": "", "LC_ALL": "C"},
        quiet = False,
    )
    if result.return_code:
        fail("Kbuild syntax export failed:\n" + result.stderr)
    ctx.symlink("export/program.bzl", "program.bzl")
    ctx.file("syntax.bzl", """load(%r, "expand_kernel")
load(%r, "KernelSyntaxInfo")
load(":program.bzl", "KBUILD")

# The callback is top-level; its immutable source syntax is loaded only here.
def _expand(tctx, input_directories, output_directories, tools, additional_inputs, additional_params):
    expand_kernel(tctx, input_directories, output_directories, tools, additional_inputs, additional_params, KBUILD)

def _impl(ctx):
    return [KernelSyntaxInfo(anchor = ctx.file.anchor, expand = _expand)]

program = rule(implementation = _impl, attrs = {"anchor": attr.label(mandatory = True, allow_single_file = True)})
""" % (str(ctx.attr._kernel), str(ctx.attr._syntax)))
    ctx.file("BUILD.bazel", """load(":syntax.bzl", "program")
program(name = "program", anchor = %r, visibility = ["//visibility:public"])
exports_files(["export/report.json"], visibility = ["//visibility:public"])
""" % str(ctx.attr.source))

kernel_syntax_repository = repository_rule(
    implementation = _syntax_impl,
    attrs = {
        "source": attr.label(mandatory = True, allow_single_file = True, doc = "Original kernel root Makefile; its complete source tree is watched."),
        "tool": attr.label(mandatory = True, allow_single_file = True, doc = "Pinned generic exporter source executable, never an action output."),
        "timeout": attr.int(default = 600),
        "_kernel": attr.label(default = "//rules:kernel.bzl"),
        "_syntax": attr.label(default = "//rules:syntax.bzl"),
    },
)
