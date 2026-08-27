"""Thin repository facade for one configured Linux image."""

load(":image_name_validation.bzl", "validate_linux_overlay_name")
load(":repository_utils.bzl", _repository_prefix = "repository_prefix")

visibility("//...")

def _build_file(rules_repo, source_repo, config, config_mode, platform, overlay):
    return """load({source_info}, "LINUX_MODULE_MAKE_VARS", "LINUX_MODULE_TARGETS", "LINUX_SOURCE_VERSION")
load({mapped_kernel}, "linux_mapped_image_targets")

package(default_visibility = ["//visibility:public"])

linux_mapped_image_targets(
    name = "kernel",
    config = {config},
    config_mode = {config_mode},
    module_make_vars = LINUX_MODULE_MAKE_VARS,
    module_targets = LINUX_MODULE_TARGETS,
    overlay = {overlay},
    platform = {platform},
    source_repo = {source_repo},
    version = LINUX_SOURCE_VERSION,
)
""".format(
        config = repr(str(config)),
        config_mode = repr(config_mode),
        mapped_kernel = repr(rules_repo + "//internal:mapped_kernel.bzl"),
        overlay = repr(str(overlay)) if overlay else "None",
        platform = repr(str(platform)),
        source_info = repr(source_repo + "//:source_info.bzl"),
        source_repo = repr(source_repo),
    )

def _linux_image_impl(rctx):
    source = rctx.attr.source
    if source.name != "Kconfig" or source.package != "" or source.repo_name == "":
        fail("source must be the root Kconfig label from linux_source_repository")
    source_repo = _repository_prefix(source)
    rules_repo = _repository_prefix(rctx.attr._self_linux_bzl)
    rctx.file(
        "BUILD.bazel",
        _build_file(
            rules_repo = rules_repo,
            source_repo = source_repo,
            config = rctx.attr.config,
            config_mode = rctx.attr.config_mode,
            platform = rctx.attr.platform,
            overlay = None,
        ),
        executable = False,
    )
    for name in sorted(rctx.attr.overlays):
        validate_linux_overlay_name(name)
        rctx.file(
            "variants/%s/BUILD.bazel" % name,
            _build_file(
                rules_repo = rules_repo,
                source_repo = source_repo,
                config = rctx.attr.config,
                config_mode = rctx.attr.config_mode,
                platform = rctx.attr.platform,
                overlay = rctx.attr.overlays[name],
            ),
            executable = False,
        )
    return rctx.repo_metadata(reproducible = True)

linux_image = repository_rule(
    implementation = _linux_image_impl,
    attrs = {
        "config": attr.label(allow_single_file = True, mandatory = True),
        "config_mode": attr.string(default = "default", values = ["allnoconfig", "default"]),
        "overlays": attr.string_keyed_label_dict(allow_files = True),
        "platform": attr.label(mandatory = True),
        "source": attr.label(allow_single_file = True, mandatory = True),
        "_self_linux_bzl": attr.label(default = Label("//:linux.bzl")),
    },
    doc = "Creates stable labels for an execution-time mapped Linux build.",
)
