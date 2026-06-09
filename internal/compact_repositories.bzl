"""Repository rules for generated compact Linux Kbuild packages."""

def _run_compact_generator(rctx):
    root = rctx.path(rctx.attr.root)
    config = rctx.path(rctx.attr.config)
    kbuild = rctx.path(rctx.attr.kbuild)
    kconfig_parse_tool = rctx.path(rctx.attr.kconfig_parse_tool)
    rctx.watch(config)
    rctx.watch(kbuild)
    rctx.watch(kconfig_parse_tool)
    rctx.watch(root)
    source_root = root.dirname
    output_build = rctx.path("BUILD.bazel")
    output_metadata = rctx.path("metadata.json")
    env = dict(rctx.attr.env)
    vars = dict(rctx.attr.vars)
    if "srctree" not in vars:
        vars["srctree"] = str(source_root).replace("\\", "/")
    _configure_probe_env(rctx.attr.allow_shell, env)

    args = [
        str(kconfig_parse_tool),
        "-root",
        str(root),
        "-srctree",
        str(source_root),
        "-kbuild",
        str(kbuild),
        "-compact_metadata_out",
        str(output_metadata),
        "-compact_buildfile_out",
        str(output_build),
        "-compact_buildfile_export",
        "metadata.json",
        "-linux_objects_load",
        rctx.attr.linux_objects_load,
        "-config",
        "%s=%s" % (rctx.attr.config_name, config),
    ]
    if rctx.attr.config_mode:
        args.extend(["-config_mode", rctx.attr.config_mode])
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
    for label, prefix in rctx.attr.extra_kconfigs.items():
        path = rctx.path(label)
        args.extend(["-source_root_map", "%s=%s" % (prefix, path.dirname)])
        args.extend(["-kconfig_extra", "%s=%s" % (prefix, path)])
    for label, prefix in rctx.attr.extra_kbuilds.items():
        path = rctx.path(label)
        args.extend(["-source_root_map", "%s=%s" % (prefix, path.dirname)])
        args.extend(["-kbuild_extra", "%s=%s" % (prefix, path)])
    for prefix, label_package in sorted(rctx.attr.extra_source_label_packages.items()):
        args.extend(["-source_label_map", "%s=%s" % (prefix, label_package)])
    for key, value in sorted(vars.items()):
        args.extend(["-var", "%s=%s" % (key, value)])
    for key, value in sorted(env.items()):
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
    if rctx.attr.source_relacheck:
        rctx.file(output_build, _add_relacheck_to_pi_objects(rctx.read(output_build), rctx.attr.source_relacheck))

def _add_relacheck_to_pi_objects(content, relacheck):
    lines = content.splitlines()
    out = []
    block = None
    object = ""
    has_relacheck = False
    for line in lines:
        if block == None:
            if line == "linux_object(":
                block = [line]
                object = ""
                has_relacheck = False
                continue
            out.append(line)
            continue

        if line == ")":
            if object.endswith(".pi.o") and not has_relacheck:
                block.append("    relacheck = \"%s\"," % relacheck)
            out.extend(block)
            out.append(line)
            block = None
            continue

        stripped = line.strip()
        if stripped.startswith("object = \"") and stripped.endswith("\","):
            object = stripped[len("object = \""):-len("\",")]
        if stripped.startswith("relacheck = "):
            has_relacheck = True
        block.append(line)

    if block != None:
        out.extend(block)
    return "\n".join(out) + "\n"

def _configure_probe_env(allow_shell, env):
    if not allow_shell:
        return
    env.setdefault("CC", "clang")
    env.setdefault("CC_VERSION_TEXT", "clang version 22.1.4None")
    env.setdefault("LD", "ld.lld")
    env.setdefault("CLANG_FLAGS", "-fintegrated-as")
    env.setdefault("RUSTC", "rustc")
    env.setdefault("PAHOLE", "pahole")
    env.setdefault("BINDGEN", "bindgen")

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
        "config_mode": attr.string(
            default = "default",
            doc = "Config resolver mode passed to kconfig_parse. Supported: default, allnoconfig.",
            values = [
                "default",
                "allnoconfig",
            ],
        ),
        "env": attr.string_dict(
            doc = "Hermetic Kconfig preprocessor environment values.",
        ),
        "extra_kbuilds": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "Map of extra Kbuild/Makefile labels to virtual Linux source prefixes.",
        ),
        "extra_kconfigs": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "Map of extra Kconfig labels to virtual Linux source prefixes.",
        ),
        "extra_source_label_packages": attr.string_dict(
            doc = "Map of virtual Linux source prefixes to Bazel label packages used for generated source labels.",
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
        "source_relacheck": attr.string(
            doc = "Label emitted for arch/arm64/kernel/pi/relacheck.",
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
