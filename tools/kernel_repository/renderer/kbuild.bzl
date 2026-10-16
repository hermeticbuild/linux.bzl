"""Unevaluated Kbuild declarations, objects, and directory references."""

KbuildFileInfo = provider(doc = "Kbuild File Info.", fields = ["path", "statements"])
KbuildObjectInfo = provider(doc = "Kbuild Object Info.", fields = ["path", "sources", "definitions", "members", "occurrences"])
KbuildDirectoryInfo = provider(doc = "Kbuild Directory Info.", fields = ["path", "definitions", "child", "occurrences"])

def assignment(**fields):
    return dict(fields, kind = "assignment")

def define(**fields):
    return dict(fields, kind = "define")

def conditional(**fields):
    return dict(fields, kind = "conditional")

def make_rule(**fields):
    return dict(fields, kind = "rule")

def target_assignment(**fields):
    return dict(fields, kind = "target_assignment")

def directive(**fields):
    return dict(fields, kind = "directive")

def recipe(**fields):
    return dict(fields, kind = "recipe")

def expression(**fields):
    return dict(fields, kind = "expression")

def undefine(**fields):
    return dict(fields, kind = "undefine")

def _file_impl(ctx):
    return [
        KbuildFileInfo(path = ctx.attr.path, statements = json.decode(ctx.attr.statements)),
        DefaultInfo(files = depset([ctx.file.src])),
    ]

_file = rule(
    implementation = _file_impl,
    attrs = {
        "path": attr.string(mandatory = True),
        "src": attr.label(mandatory = True, allow_single_file = True),
        "statements": attr.string(mandatory = True),
    },
)

def kbuild_file(name, path, src, statements, **kwargs):
    _file(name = name, path = path, src = src, statements = json.encode(statements), **kwargs)

def _object_impl(ctx):
    return [KbuildObjectInfo(
        path = ctx.attr.path,
        sources = [f.short_path for f in ctx.files.srcs],
        definitions = [str(t.label) for t in ctx.attr.definitions],
        members = ctx.attr.members,
        occurrences = json.decode(ctx.attr.occurrences),
    ), DefaultInfo(files = depset(ctx.files.srcs))]

_object = rule(
    implementation = _object_impl,
    attrs = {
        "path": attr.string(mandatory = True),
        "srcs": attr.label_list(allow_files = True),
        "definitions": attr.label_list(providers = [KbuildFileInfo]),
        # Edges remain symbolic until Make conditions/assignment order resolve.
        "members": attr.string_list(),
        "occurrences": attr.string(mandatory = True),
    },
)

def kbuild_object(name, path, definitions, srcs = [], members = [], occurrences = [], **kwargs):
    _object(
        name = name,
        path = path,
        definitions = definitions,
        srcs = srcs,
        members = members,
        occurrences = json.encode(occurrences),
        **kwargs
    )

def _directory_impl(ctx):
    return [KbuildDirectoryInfo(
        path = ctx.attr.path,
        definitions = [str(t.label) for t in ctx.attr.definitions],
        child = str(ctx.attr.child.label) if ctx.attr.child else None,
        occurrences = json.decode(ctx.attr.occurrences),
    ), DefaultInfo()]

_directory = rule(
    implementation = _directory_impl,
    attrs = {
        "path": attr.string(mandatory = True),
        "definitions": attr.label_list(providers = [KbuildFileInfo]),
        "child": attr.label(providers = [KbuildFileInfo]),
        "occurrences": attr.string(mandatory = True),
    },
)

def kbuild_directory(name, path, definitions, child = None, occurrences = [], **kwargs):
    _directory(
        name = name,
        path = path,
        definitions = definitions,
        child = child,
        occurrences = json.encode(occurrences),
        **kwargs
    )

def _graph_impl(ctx):
    objects = [t[KbuildObjectInfo] for t in ctx.attr.objects]
    directories = [t[KbuildDirectoryInfo] for t in ctx.attr.directories]
    files = [t[KbuildFileInfo] for t in ctx.attr.makefiles]
    out = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(out, json.encode({"objects": objects, "directories": directories, "makefiles": files}))
    return [DefaultInfo(files = depset([out]))]

kbuild_graph = rule(
    implementation = _graph_impl,
    attrs = {
        "makefiles": attr.label_list(providers = [KbuildFileInfo]),
        "objects": attr.label_list(providers = [KbuildObjectInfo]),
        "directories": attr.label_list(providers = [KbuildDirectoryInfo]),
    },
)
