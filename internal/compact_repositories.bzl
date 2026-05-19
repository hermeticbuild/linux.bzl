"""Repository rules for generated compact Linux Kbuild packages."""

def _run_compact_generator(rctx):
    root = rctx.path(rctx.attr.root)
    source_root = root.dirname
    output_build = rctx.path("BUILD.bazel")
    output_metadata = rctx.path("metadata.json")

    args = [
        str(rctx.path(rctx.attr.kconfig_parse_tool)),
        "-root",
        str(root),
        "-srctree",
        str(source_root),
        "-kbuild",
        str(rctx.path(rctx.attr.kbuild)),
        "-compact_metadata_out",
        str(output_metadata),
        "-compact_buildfile_out",
        str(output_build),
        "-compact_buildfile_export",
        "metadata.json",
        "-linux_objects_load",
        rctx.attr.linux_objects_load,
        "-config",
        "%s=%s" % (rctx.attr.config_name, rctx.path(rctx.attr.config)),
    ]
    if rctx.attr.allow_shell:
        args.append("-allow_shell")
        if rctx.attr.probe_model:
            args.extend(["-linux_probe_model", rctx.attr.probe_model])
    if rctx.attr.kbuild_tree:
        args.append("-compact_kbuild_tree")
    if rctx.attr.generated_headers:
        args.extend(["-generated_headers", rctx.attr.generated_headers])
    if rctx.attr.object_label_package:
        args.extend(["-object_label_package", rctx.attr.object_label_package])
    if rctx.attr.source_asn1_compiler:
        args.extend(["-source_asn1_compiler", rctx.attr.source_asn1_compiler])
    if rctx.attr.source_config:
        args.extend(["-source_config", rctx.attr.source_config])
    if rctx.attr.source_label_package:
        args.extend(["-source_label_package", rctx.attr.source_label_package])
    if rctx.attr.source_root_label:
        args.extend(["-source_root_label", rctx.attr.source_root_label])
    for label in rctx.attr.source_tree_labels:
        args.extend(["-source_tree_label", label])
    for key, value in sorted(rctx.attr.vars.items()):
        args.extend(["-var", "%s=%s" % (key, value)])
    for key, value in sorted(rctx.attr.env.items()):
        args.extend(["-env", "%s=%s" % (key, value)])
    for key, value in sorted(rctx.attr.probe_values.items()):
        args.extend(["-linux_probe_value", "%s=%s" % (key, value)])
    for visibility in rctx.attr.generated_visibility:
        args.extend(["-visibility", visibility])

    result = rctx.execute(args, quiet = False)
    if result.return_code != 0:
        fail("compact Linux BUILD generation failed for %s\nstdout:\n%s\nstderr:\n%s" % (
            rctx.attr.name,
            result.stdout,
            result.stderr,
        ))

def _linux_compact_repository_impl(rctx):
    _run_compact_generator(rctx)
    return rctx.repo_metadata(reproducible = True)

linux_compact_repository = repository_rule(
    implementation = _linux_compact_repository_impl,
    attrs = {
        "allow_shell": attr.bool(
            default = True,
            doc = "Allow deterministic Linux Kconfig probe shell expansion.",
        ),
        "config": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Raw Linux .config fragment used for compact metadata.",
        ),
        "config_name": attr.string(
            mandatory = True,
            doc = "Compact config name, usually the Linux architecture config name.",
        ),
        "env": attr.string_dict(
            doc = "Hermetic Kconfig preprocessor environment values.",
        ),
        "generated_headers": attr.string(
            doc = "Label emitted for generated Linux headers.",
        ),
        "generated_visibility": attr.string_list(
            default = ["//visibility:public"],
            doc = "Default visibility emitted into the generated BUILD file.",
        ),
        "kbuild": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Root Kbuild file.",
        ),
        "kbuild_tree": attr.bool(
            default = True,
            doc = "Follow active Kbuild directory descent when generating compact metadata.",
        ),
        "kconfig_parse_tool": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Host executable that generates compact Linux Kbuild metadata.",
        ),
        "linux_objects_load": attr.string(
            default = "@linux.bzl//internal:linux_objects.bzl",
            doc = "Load label emitted for generated linux_object/linux_compact_image rules.",
        ),
        "object_label_package": attr.string(
            doc = "Package label path used by generated image rules to reference object variants. Empty means the generated repo root.",
        ),
        "probe_model": attr.string(
            default = "linux_llvm",
            doc = "Hermetic Linux Kconfig probe model used when allow_shell is set.",
        ),
        "probe_values": attr.string_dict(
            doc = "Overrides for the selected Linux Kconfig probe model.",
        ),
        "root": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "Root Kconfig file.",
        ),
        "source_asn1_compiler": attr.string(
            doc = "Label emitted for the source tree's scripts/asn1_compiler tool.",
        ),
        "source_config": attr.string(
            doc = "Label emitted for the resolved LinuxConfigInfo target.",
        ),
        "source_label_package": attr.string(
            doc = "Package label path used to reference Linux source files.",
        ),
        "source_root_label": attr.string(
            doc = "Label emitted for a file in the Linux source root.",
        ),
        "source_tree_labels": attr.string_list(
            doc = "Labels emitted for source tree inputs.",
        ),
        "vars": attr.string_dict(
            doc = "Kconfig/Kbuild make variables.",
        ),
    },
    doc = "Generates a repository containing compact Linux Kbuild BUILD metadata.",
)
