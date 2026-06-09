"""Repository rules for generated Kconfig metadata."""

load("@bazel_lib//lib:repo_utils.bzl", "repo_utils")
load("//internal:kconfig_tool_releases.bzl", "KCONFIG_TOOL_RELEASES", "KCONFIG_TOOL_VERSION")

def kconfig_tool_filename(platform, tool):
    if platform.startswith("windows_"):
        return tool + ".exe"
    return tool

def _host_platform(rctx):
    return repo_utils.platform(rctx)

def _kconfig_tool_build(platform, tools):
    filenames = [kconfig_tool_filename(platform, tool) for tool in tools]
    lines = [
        'package(default_visibility = ["//visibility:public"])',
        "",
        "exports_files({})".format(repr(filenames)),
    ]
    for tool, filename in zip(tools, filenames):
        if tool != filename:
            lines.extend([
                "",
                "alias(",
                '    name = "{}",'.format(tool),
                '    actual = "{}",'.format(filename),
                ")",
            ])
    return "\n".join(lines) + "\n"

def _kconfig_tool_repository_impl(rctx):
    platform = _host_platform(rctx)
    release = KCONFIG_TOOL_RELEASES.get(platform)
    if not release:
        fail("no kconfig tool prebuilt metadata for host platform %s" % platform)
    if not release.integrity:
        fail("kconfig tool prebuilt metadata for %s is missing an integrity value; update //internal:kconfig_tool_releases.bzl after publishing kconfig %s prebuilts" % (
            platform,
            KCONFIG_TOOL_VERSION,
        ))

    download_kwargs = {
        "output": ".",
        "url": release.urls,
    }
    download_kwargs["integrity"] = release.integrity
    rctx.download_and_extract(**download_kwargs)

    for tool in rctx.attr.tools:
        filename = kconfig_tool_filename(platform, tool)
        if not rctx.path(filename).exists:
            fail("kconfig prebuilt archive for %s did not contain required tool %s; publish a new %s archive with all required repo-rule tools" % (
                platform,
                filename,
                KCONFIG_TOOL_VERSION,
            ))

    rctx.file(
        "BUILD.bazel",
        _kconfig_tool_build(platform, rctx.attr.tools),
    )
    return rctx.repo_metadata(reproducible = True)

kconfig_tool_repository = repository_rule(
    implementation = _kconfig_tool_repository_impl,
    attrs = {
        "tools": attr.string_list(
            default = ["kconfig"],
            doc = "Tool filenames that must be present in the selected prebuilt archive.",
        ),
    },
    doc = "Downloads the host kconfig executable used by repository rules.",
)

def _write_build_file(rctx, config_path, out_path):
    args = [
        str(rctx.path(rctx.attr.kconfig_tool)),
        "-generate_buildfile",
        "-kconfig",
        str(config_path),
        "-rule",
        rctx.attr.kconfig_name,
        "-config_label",
        ":config",
        "-out",
        out_path,
    ]
    for visibility in rctx.attr.generated_visibility:
        args.extend(["-visibility", visibility])

    result = rctx.execute(args, quiet = False)
    if result.return_code != 0:
        fail("kconfig BUILD generation failed for %s\nstdout:\n%s\nstderr:\n%s" % (
            rctx.attr.config,
            result.stdout,
            result.stderr,
        ))

def _kconfig_repository_impl(rctx):
    config_path = rctx.path(rctx.attr.config)
    rctx.watch(config_path)
    rctx.symlink(config_path, "config")
    _write_build_file(rctx, config_path, "BUILD.bazel")
    return rctx.repo_metadata(reproducible = True)

kconfig_repository = repository_rule(
    implementation = _kconfig_repository_impl,
    attrs = {
        "config": attr.label(
            allow_single_file = True,
            doc = "Linux .config file used as the source of truth.",
            mandatory = True,
        ),
        "generated_visibility": attr.string_list(
            default = ["//visibility:public"],
            doc = "Visibility emitted on the generated kconfig_file target.",
        ),
        "kconfig_name": attr.string(
            default = "kconfig",
            doc = "Name of the generated kconfig_file target.",
        ),
        "kconfig_tool": attr.label(
            allow_single_file = True,
            doc = "Host executable that generates the Kconfig BUILD file.",
            mandatory = True,
        ),
    },
    doc = "Generates a repository containing parsed Kconfig metadata.",
)
