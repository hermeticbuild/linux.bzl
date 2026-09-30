"""Exact runfiles aggregate for already-complete opaque Linux source inputs."""

visibility("public")

LinuxSourcePathsInfo = provider(
    doc = "Physical source paths relocated by the repository, mapped to their upstream names.",
    fields = {"renamed_paths": "Source-root-relative physical path -> logical path."},
)

def linux_source_path(file, source_prefix, renamed_paths):
    """Returns a File's logical name within the authenticated Linux source tree."""
    canonical = file.short_path
    if source_prefix:
        prefix = source_prefix + "/"
        if not canonical.startswith(prefix):
            fail("mapped Linux source input %s is outside %s" % (file, source_prefix))
        canonical = canonical[len(prefix):]
    return renamed_paths.get(canonical, canonical)

def add_linux_source_arg(args, flag, source_runfiles, path, prefix = ""):
    """Keeps the source-tree anchor typed so Bazel can path-map its runfiles."""
    args.add_joined(
        flag,
        [prefix, source_runfiles.executable, ".runfiles/kernel/", path],
        join_with = "",
        expand_directories = False,
    )

def _canonical_artifact_path(path, what):
    if not path or path.startswith("/") or "\\" in path or "\000" in path or "\n" in path or "\r" in path or "\t" in path:
        fail("%s has a non-canonical artifact path %r" % (what, path))
    parts = path.split("/")

    # Bazel's sibling-repository layout uses ../<canonical-repository>/... .
    # Only that leading parent component is allowed; no internal traversal.
    if parts[0] == "..":
        if len(parts) < 3:
            fail("%s has an incomplete sibling-repository artifact path %r" % (what, path))
        parts = parts[1:]
    for part in parts:
        if part in ["", ".", ".."]:
            fail("%s has a non-canonical artifact path %r" % (what, path))
    return path

def _source_runfiles_mapping(source_files, source_root, renamed_paths = {}):
    if source_root == None:
        fail("Linux source runfiles requires a source_root marker")
    if source_root.is_directory:
        fail("Linux source runfiles source_root must not be a directory artifact")
    marker_path = _canonical_artifact_path(source_root.path, "Linux source runfiles marker")
    root = marker_path.rsplit("/", 1)[0] if "/" in marker_path else ""
    prefix = root + "/" if root else ""
    by_relative = {}
    renamed = {}

    # Existing opaque actions already depend on source_files AND source_root.
    # Preserve that exact union even when the filegroup omits its marker.
    for file in source_files + [source_root]:
        if file.is_directory:
            fail("Linux source runfiles source_files contains a directory artifact %s" % file.path)
        artifact_path = _canonical_artifact_path(file.path, "Linux source runfiles input")
        if not artifact_path.startswith(prefix) or (not root and artifact_path.startswith("../")):
            fail("Linux source runfiles input %r is outside source root %r" % (artifact_path, root))
        relative = artifact_path[len(prefix):]
        if not relative:
            fail("Linux source runfiles input aliases the source root %r" % root)
        if relative in renamed_paths:
            renamed[relative] = True
            relative = _canonical_artifact_path(renamed_paths[relative], "Linux logical source path")
            if relative.startswith("../"):
                fail("Linux logical source path escapes the source root")
        previous = by_relative.get(relative)
        if previous != None and previous != file:
            fail("Linux source runfiles path %r names distinct artifacts" % relative)
        by_relative[relative] = file
    if len(renamed) != len(renamed_paths):
        fail("Linux source runfiles renamed_paths contains absent source files")
    result = {}
    for relative in sorted(by_relative):
        parts = relative.split("/")
        for depth in range(1, len(parts)):
            parent = "/".join(parts[:depth])
            if parent in by_relative:
                fail("Linux source runfiles has a file/directory collision at %r" % parent)
        result["kernel/" + relative] = by_relative[relative]
    return result

def linux_test_source_runfiles_mapping(source_files, source_root, renamed_paths = {}):
    """Returns exact original File mappings; accepts File-shaped test records."""
    return _source_runfiles_mapping(source_files, source_root, renamed_paths)

def validate_linux_source_runfiles(info, source_files, source_root, renamed_paths = {}):
    """Authenticates an aggregate against the caller's exact original Files.

    Args:
      info: The wrapper target's DefaultInfo, not a pathname assertion.
      source_files: The caller's original complete source File list.
      source_root: The caller's original source-root marker File.
      renamed_paths: Repository-owned physical to logical source path mapping.

    Returns:
      The validated wrapper's FilesToRunProvider, to pass without flattening.
    """
    expected = _source_runfiles_mapping(source_files, source_root, renamed_paths)
    if info == None or info.files_to_run == None or info.files_to_run.executable == None:
        fail("Linux source runfiles requires an executable anchor provider")
    executable = info.files_to_run.executable
    if executable.is_directory or info.files.to_list() != [executable]:
        fail("Linux source runfiles files_to_build must contain only its executable anchor")
    runfiles = info.default_runfiles
    if runfiles == None:
        fail("Linux source runfiles requires default runfiles")
    for file in runfiles.files.to_list():
        if file != executable:
            fail("Linux source runfiles contains extra ordinary runfiles")
    if runfiles.symlinks.to_list() or runfiles.empty_filenames.to_list():
        fail("Linux source runfiles contains extra symlinks or empty runfiles")
    seen = {}
    for entry in runfiles.root_symlinks.to_list():
        if entry.path in seen:
            fail("Linux source runfiles contains duplicate root mapping %r" % entry.path)
        seen[entry.path] = True

        # A cfg=exec wrapper can resolve generated source labels to a different
        # File from the original target configuration. Equal relative paths or
        # content do not prove that the original dependency closure is retained.
        if expected.get(entry.path) != entry.target_file:
            fail("Linux source runfiles mapping does not match original source Files at %r" % entry.path)
    if len(seen) != len(expected):
        fail("Linux source runfiles mapping is missing original source Files")
    return info.files_to_run

def _linux_source_runfiles_impl(ctx):
    mapping = _source_runfiles_mapping(ctx.files.source_files, ctx.file.source_root, ctx.attr.renamed_paths)
    anchor = ctx.actions.declare_file(ctx.label.name + ".anchor")

    # Data-only anchor: not a script, interpreter, or source-processing tool.
    # It is executable solely to obtain Bazel's FilesToRunProvider/runfiles tree.
    # Direct execution has no valid executable format; consumers only use its
    # typed path to locate <anchor>.runfiles/kernel, never execute this file.
    ctx.actions.write(anchor, "linux-source-runfiles-anchor-v1\n", is_executable = True)
    return [LinuxSourcePathsInfo(renamed_paths = ctx.attr.renamed_paths), DefaultInfo(
        executable = anchor,
        files = depset([anchor]),
        runfiles = ctx.runfiles(root_symlinks = mapping),
    )]

linux_source_runfiles = rule(
    implementation = _linux_source_runfiles_impl,
    attrs = {
        "renamed_paths": attr.string_dict(),
        "source_files": attr.label_list(allow_files = True, mandatory = True),
        "source_root": attr.label(allow_single_file = True, mandatory = True),
    },
    executable = True,
    doc = "Aggregates the exact complete Linux source closure without copying source bytes.",
)

def _linux_source_directory_impl(ctx):
    output = ctx.actions.declare_directory(ctx.label.name + ".linux-bzl-directory")
    args = ctx.actions.args()
    args.add("-tree_out")
    args.add_all([output], expand_directories = False)
    args.add("-preserve_mode")
    args.use_param_file("@%s", use_always = True)
    args.set_param_file_format("multiline")
    prefix = "kernel/" + ctx.attr.path + "/"
    inputs = []
    for entry in ctx.attr.source[DefaultInfo].default_runfiles.root_symlinks.to_list():
        if entry.path.startswith(prefix):
            args.add_joined("-copy", [entry.path[len(prefix):] + "=", entry.target_file], join_with = "", expand_directories = False)
            inputs.append(entry.target_file)
    ctx.actions.run(
        executable = ctx.executable._copy,
        arguments = [args],
        inputs = inputs,
        outputs = [output],
        execution_requirements = {"supports-path-mapping": "1"},
        mnemonic = "LinuxSourceDirectory",
    )
    return [DefaultInfo(files = depset([output]))]

_linux_source_directory = rule(
    implementation = _linux_source_directory_impl,
    attrs = {
        "path": attr.string(mandatory = True),
        "source": attr.label(mandatory = True, providers = [LinuxSourcePathsInfo]),
        "_copy": attr.label(default = Label("//internal/cmd/actionfile"), cfg = "exec", executable = True),
    },
)

def linux_source_directory(name, source, path, **kwargs):
    """Materializes an exported source directory only when a consumer needs it."""
    _linux_source_directory(
        name = name,
        source = source,
        path = path,
        exec_compatible_with = [Label("@platforms//os:linux")],
        **kwargs
    )
