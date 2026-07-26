"""Helpers for actions that support Bazel's stripped output paths."""

visibility("//internal/...")

def _mapped_directory_arg(anchor):
    path = anchor.file.dirname
    for _ in range(anchor.parents):
        if "/" not in path:
            fail("cannot walk above execroot while formatting %s" % anchor.file)
        path = path.rsplit("/", 1)[0]
    return anchor.format % path

def _path_arg_format(value, path):
    return value.replace("%", "%%").replace(path, "%s")

def directory_anchor(file, directory = None):
    """Returns a File-backed reference to an ancestor directory."""
    if directory == None:
        directory = file.dirname
    current = file.dirname
    if current != directory and not current.startswith(directory + "/"):
        fail("%s is not an ancestor directory of %s" % (directory, file))
    parents = len(current.split("/")) - len(directory.split("/"))
    return struct(
        file = file,
        parents = parents,
    )

def add_directory_arg(args, anchor, format = "%s"):
    """Adds an anchored directory argument that Bazel can path-map."""
    args.add_all(
        [struct(
            file = anchor.file,
            format = format,
            parents = anchor.parents,
        )],
        map_each = _mapped_directory_arg,
    )

def add_directory_args(args, anchors, format = "%s"):
    """Adds anchored directory arguments that Bazel can path-map."""
    args.add_all(
        [
            struct(
                file = anchor.file,
                format = format,
                parents = anchor.parents,
            )
            for anchor in anchors
        ],
        map_each = _mapped_directory_arg,
    )

def add_mapped_values(args, values, files = [], directory_anchors = {}):
    """Adds strings while retaining File-backed path segments for mapping."""
    files_by_path = {file.path: file for file in files}
    for value in values:
        best_file_path = ""
        for path in files_by_path:
            if path in value and len(path) > len(best_file_path):
                best_file_path = path
        if best_file_path:
            args.add_all(
                [files_by_path[best_file_path]],
                expand_directories = False,
                format_each = _path_arg_format(value, best_file_path),
            )
            continue
        best_directory = ""
        for path in directory_anchors:
            if path in value and len(path) > len(best_directory):
                best_directory = path
        if best_directory:
            add_directory_arg(
                args,
                directory_anchors[best_directory],
                format = _path_arg_format(value, best_directory),
            )
            continue
        args.add(value)

def path_mapped_run(actions, **kwargs):
    execution_requirements = dict(kwargs.get("execution_requirements", {}))
    execution_requirements["supports-path-mapping"] = "1"
    kwargs["execution_requirements"] = execution_requirements
    actions.run(**kwargs)
