"""Test-only grouped object consumers for compact graph fixtures."""

load(
    ":fake_linux_objects.bzl",
    "LinuxImageInfo",
    "LinuxObjectInfo",
)

visibility("public")

LinuxObjectActionGroupInfo = provider(
    doc = "Grouped Linux object fixture data.",
    fields = {
        "mode": "",
        "object_targets": "",
        "objects": "",
        "reachable_configs": "",
        "reachability_id": "",
        "recipe_id": "",
    },
)

def _group_info(ctx, objects):
    return [
        DefaultInfo(files = depset([info.output for info in objects.values()])),
        LinuxObjectActionGroupInfo(
            mode = ctx.attr.mode,
            object_targets = sorted(objects.keys()),
            objects = objects,
            reachable_configs = ctx.attr.reachable_configs,
            reachability_id = ctx.attr.reachability_id,
            recipe_id = ctx.attr.recipe_id,
        ),
    ]

def _linux_object_action_group_impl(ctx):
    objects = {}
    for target, encoded in ctx.attr.objects.items():
        spec = json.decode(encoded)
        output = ctx.actions.declare_file(ctx.label.name + ".objects/" + spec["content_id"] + "/" + spec["object"])
        ctx.actions.write(output, "")
        objects[target] = LinuxObjectInfo(
            content_id = spec["content_id"],
            object = spec["object"],
            output = output,
        )
    return _group_info(ctx, objects)

linux_object_action_group = rule(
    implementation = _linux_object_action_group_impl,
    attrs = {
        "arch": attr.string(),
        "compile_environment_index": attr.label(mandatory = True),
        "flags": attr.string_list(),
        "include_dirs": attr.string_list(),
        "language": attr.string(mandatory = True),
        "mode": attr.string(mandatory = True),
        "modname": attr.string(),
        "module_root": attr.bool(),
        "objects": attr.string_dict(mandatory = True),
        "objtool": attr.label(),
        "objtool_args": attr.string_list(),
        "objtool_force": attr.bool(),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
        "source_input_index": attr.label(mandatory = True),
        "srcarch": attr.string(),
    },
)

def _linux_object_action_group_import_impl(ctx):
    objects = {}
    for index in range(len(ctx.attr.object_targets)):
        objects[ctx.attr.object_targets[index]] = ctx.attr.objects[index][LinuxObjectInfo]
    return _group_info(ctx, objects)

linux_object_action_group_import = rule(
    implementation = _linux_object_action_group_import_impl,
    attrs = {
        "mode": attr.string(mandatory = True),
        "object_targets": attr.string_list(mandatory = True),
        "objects": attr.label_list(mandatory = True, providers = [LinuxObjectInfo]),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
    },
)

def _linux_composite_object_action_group_impl(ctx):
    available = {}
    for dep in ctx.attr.member_groups:
        available.update(dep[LinuxObjectActionGroupInfo].objects)
    objects = {}
    pending = {
        target: json.decode(encoded)
        for target, encoded in ctx.attr.objects.items()
    }
    for _ in range(len(pending)):
        progressed = False
        for target in sorted(pending.keys()):
            spec = pending[target]
            if any([member not in available for member in spec["members"]]):
                continue
            output = ctx.actions.declare_file(ctx.label.name + ".objects/" + spec["content_id"] + "/" + spec["object"])
            ctx.actions.write(output, "")
            info = LinuxObjectInfo(
                content_id = spec["content_id"],
                object = spec["object"],
                output = output,
            )
            available[target] = info
            objects[target] = info
            pending.pop(target)
            progressed = True
        if not pending or not progressed:
            break
    if pending:
        fail("%s has unresolved composite members: %s" % (ctx.label, ", ".join(sorted(pending.keys()))))
    return _group_info(ctx, objects)

linux_composite_object_action_group = rule(
    implementation = _linux_composite_object_action_group_impl,
    attrs = {
        "arch": attr.string(),
        "member_groups": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectActionGroupInfo],
        ),
        "mode": attr.string(mandatory = True),
        "module_root": attr.bool(),
        "objects": attr.string_dict(mandatory = True),
        "objtool_args": attr.string_list(),
        "objtool_force": attr.bool(),
        "reachable_configs": attr.string_list(mandatory = True),
        "reachability_id": attr.string(mandatory = True),
        "recipe_id": attr.string(mandatory = True),
    },
)

def _linux_grouped_compact_image_impl(ctx):
    available = {}
    for dep in ctx.attr.groups:
        available.update(dep[LinuxObjectActionGroupInfo].objects)
    objects = [available[target] for target in ctx.attr.object_targets]
    module_objects = [available[target] for target in ctx.attr.module_object_targets]
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

linux_grouped_compact_image = rule(
    implementation = _linux_grouped_compact_image_impl,
    attrs = {
        "arch": attr.string(),
        "config": attr.string(mandatory = True),
        "groups": attr.label_list(
            mandatory = True,
            providers = [LinuxObjectActionGroupInfo],
        ),
        "module_object_targets": attr.string_list(),
        "object_targets": attr.string_list(mandatory = True),
    },
)
