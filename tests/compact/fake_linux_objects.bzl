"""Test-only consumers for the strict compact graph schema."""

visibility("public")

LinuxGeneratedHeadersInfo = provider(
    "Generated-header fixture data for strict compact graph tests.",
    fields = {
        "arch": "",
        "cflags": "",
        "families": "",
        "files": "",
        "include_dir_anchors": "",
        "include_dirs": "",
        "srcarch": "",
        "vdsomunge": "",
    },
)

LinuxImageInfo = provider(
    "Compact image fixture data for strict compact graph tests.",
    fields = {
        "module_objects": "",
        "objects": "",
        "output": "",
    },
)

LinuxObjectInfo = provider(
    "Object fixture data for strict compact graph tests.",
    fields = {
        "content_id": "",
        "object": "",
        "output": "",
    },
)

def _linux_compile_environment_index_impl(ctx):
    outputs = []
    payloads_by_bucket = {}
    for content_id in sorted(ctx.attr.config_payloads.keys()):
        bucket = content_id[0]
        payloads_by_bucket.setdefault(bucket, []).append(content_id)
    for bucket in sorted(payloads_by_bucket.keys()):
        bucket_outputs = []
        for content_id in payloads_by_bucket[bucket]:
            for suffix in [
                ".config",
                "auto.conf",
                "auto.conf.cmd",
                "autoconf.h",
                "bazel_kbuild_aflags.rsp",
                "bazel_kbuild_cflags.rsp",
                "integer-wrap.h",
                "kernel.release",
                "rustc_cfg",
            ]:
                output = ctx.actions.declare_file(
                    "%s.payloads/%s/%s" % (ctx.label.name, content_id, suffix),
                )
                bucket_outputs.append(output)
                outputs.append(output)
        args = ctx.actions.args()
        args.add_all(bucket_outputs)
        ctx.actions.run_shell(
            outputs = bucket_outputs,
            arguments = [args],
            command = "for output in \"$@\"; do touch \"$output\"; done",
            mnemonic = "LinuxConfigPayloads",
        )
    return [DefaultInfo(files = depset(outputs))]

linux_compile_environment_index = rule(
    implementation = _linux_compile_environment_index_impl,
    attrs = {
        "arch": attr.string(),
        "compile_environments": attr.string_dict(mandatory = True),
        "config_payloads": attr.string_dict(mandatory = True),
        "expected_abi": attr.string(mandatory = True),
        "generated_headers": attr.label_list(),
        "version": attr.string(),
    },
)

def _linux_source_tree_impl(ctx):
    return [DefaultInfo(files = depset([ctx.file.root]))]

linux_source_tree = rule(
    implementation = _linux_source_tree_impl,
    attrs = {
        "root": attr.label(allow_single_file = True, mandatory = True),
    },
)

def _linux_source_input_index_impl(ctx):
    return [DefaultInfo(files = depset(ctx.files.srcs))]

linux_source_input_index = rule(
    implementation = _linux_source_input_index_impl,
    attrs = {
        "groups": attr.string_list(mandatory = True),
        "source_tree_info": attr.label(mandatory = True),
        "srcs": attr.label_list(allow_files = True, mandatory = True),
    },
)

def _linux_object_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".o")
    ctx.actions.write(output, "")
    return [
        DefaultInfo(files = depset([output])),
        LinuxObjectInfo(
            content_id = ctx.attr.content_id,
            object = ctx.attr.object,
            output = output,
        ),
    ]

linux_object = rule(
    implementation = _linux_object_impl,
    attrs = {
        "arch": attr.string(),
        "compile_environment_id": attr.string(mandatory = True),
        "compile_environment_index": attr.label(mandatory = True),
        "content_id": attr.string(mandatory = True),
        "deps": attr.label_list(),
        "flags": attr.string_list(),
        "genksyms": attr.label(executable = True, cfg = "exec"),
        "include_dirs": attr.string_list(),
        "mode": attr.string(mandatory = True),
        "modname": attr.string(),
        "module_root": attr.bool(),
        "object": attr.string(mandatory = True),
        "objtool": attr.label(),
        "objtool_args": attr.string_list(),
        "objtool_force": attr.bool(),
        "remove_flags": attr.string_list(),
        "source_input_file": attr.int(mandatory = True),
        "source_input_group": attr.int(mandatory = True),
        "source_input_index": attr.label(mandatory = True),
        "srcarch": attr.string(),
        "symversion_flags": attr.string_list(),
        "symversion_remove_flags": attr.string_list(),
        "symversions": attr.bool(),
        "version": attr.string(),
    },
)

def _linux_compact_image_impl(ctx):
    output = ctx.actions.declare_file(ctx.label.name + ".image")
    ctx.actions.write(output, "")
    return [
        DefaultInfo(files = depset([output])),
        LinuxImageInfo(
            module_objects = [target[LinuxObjectInfo] for target in ctx.attr.module_objects],
            objects = [target[LinuxObjectInfo] for target in ctx.attr.objects],
            output = output,
        ),
    ]

linux_compact_image = rule(
    implementation = _linux_compact_image_impl,
    attrs = {
        "arch": attr.string(),
        "module_objects": attr.label_list(providers = [LinuxObjectInfo]),
        "objects": attr.label_list(providers = [LinuxObjectInfo]),
    },
)

def _ordered_objects(base, added, removed, order):
    by_id = {
        obj.content_id: obj
        for obj in base + added
        if obj.content_id not in removed
    }
    return [by_id[content_id] for content_id in order]

def _linux_compact_delta_image_impl(ctx):
    base = ctx.attr.base_image[LinuxImageInfo]
    objects = _ordered_objects(
        base.objects,
        [target[LinuxObjectInfo] for target in ctx.attr.add_objects],
        ctx.attr.remove_content_ids,
        ctx.attr.ordered_content_ids,
    )
    module_objects = _ordered_objects(
        base.module_objects,
        [target[LinuxObjectInfo] for target in ctx.attr.add_module_objects],
        ctx.attr.remove_module_content_ids,
        ctx.attr.ordered_module_content_ids,
    )
    output = ctx.actions.declare_file(ctx.label.name + ".image")
    ctx.actions.write(output, "")
    return [
        DefaultInfo(files = depset([output])),
        LinuxImageInfo(
            module_objects = module_objects,
            objects = objects,
            output = output,
        ),
    ]

linux_compact_delta_image = rule(
    implementation = _linux_compact_delta_image_impl,
    attrs = {
        "add_module_objects": attr.label_list(providers = [LinuxObjectInfo]),
        "add_objects": attr.label_list(providers = [LinuxObjectInfo]),
        "arch": attr.string(),
        "base_image": attr.label(mandatory = True, providers = [LinuxImageInfo]),
        "ordered_content_ids": attr.string_list(mandatory = True),
        "ordered_module_content_ids": attr.string_list(mandatory = True),
        "remove_content_ids": attr.string_list(),
        "remove_module_content_ids": attr.string_list(),
    },
)
