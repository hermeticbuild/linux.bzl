"""Repository rules for generated Kconfig metadata."""

load("//internal:kconfig_tool_releases.bzl", "KCONFIG_TOOL_RELEASES", "KCONFIG_TOOL_VERSION")

_KCONFIG_TOOL_BUILD = """\
package(default_visibility = ["//visibility:public"])

exports_files({tools})
"""

_OS_NAMES = {
    "linux": "linux",
    "mac os x": "darwin",
    "windows": "windows",
}

_ARCH_NAMES = {
    "aarch64": "arm64",
    "amd64": "amd64",
    "arm64": "arm64",
    "x86_64": "amd64",
}

def _host_platform(rctx):
    os_name = _OS_NAMES.get(rctx.os.name)
    arch_name = _ARCH_NAMES.get(rctx.os.arch)
    if not os_name or not arch_name:
        fail("unsupported kconfig host platform: os=%r arch=%r" % (rctx.os.name, rctx.os.arch))
    return "{}_{}".format(os_name, arch_name)

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
        if not rctx.path(tool).exists:
            fail("kconfig prebuilt archive for %s did not contain required tool %s; publish a new %s archive with all required repo-rule tools" % (
                platform,
                tool,
                KCONFIG_TOOL_VERSION,
            ))

    rctx.file(
        "BUILD.bazel",
        _KCONFIG_TOOL_BUILD.format(tools = repr(rctx.attr.tools)),
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
