"""Exact runfiles aggregate for already-complete opaque Linux source inputs."""

visibility("public")

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

def _source_runfiles_mapping(source_files, source_root):
    if source_root == None:
        fail("Linux source runfiles requires a source_root marker")
    if source_root.is_directory:
        fail("Linux source runfiles source_root must not be a directory artifact")
    marker_path = _canonical_artifact_path(source_root.path, "Linux source runfiles marker")
    root = marker_path.rsplit("/", 1)[0] if "/" in marker_path else ""
    prefix = root + "/" if root else ""
    by_relative = {}

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
        previous = by_relative.get(relative)
        if previous != None and previous != file:
            fail("Linux source runfiles path %r names distinct artifacts" % relative)
        by_relative[relative] = file
    result = {}
    for relative in sorted(by_relative):
        parts = relative.split("/")
        for depth in range(1, len(parts)):
            parent = "/".join(parts[:depth])
            if parent in by_relative:
                fail("Linux source runfiles has a file/directory collision at %r" % parent)
        result["kernel/" + relative] = by_relative[relative]
    return result

def linux_test_source_runfiles_mapping(source_files, source_root):
    """Returns exact original File mappings; accepts File-shaped test records."""
    return _source_runfiles_mapping(source_files, source_root)

def validate_linux_source_runfiles(info, source_files, source_root):
    """Authenticates an aggregate against the caller's exact original Files.

    Args:
      info: The wrapper target's DefaultInfo, not a pathname assertion.
      source_files: The caller's original complete source File list.
      source_root: The caller's original source-root marker File.

    Returns:
      The validated wrapper's FilesToRunProvider, to pass without flattening.
    """
    expected = _source_runfiles_mapping(source_files, source_root)
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
    mapping = _source_runfiles_mapping(ctx.files.source_files, ctx.file.source_root)
    anchor = ctx.actions.declare_file(ctx.label.name + ".anchor")

    # Data-only anchor: not a script, interpreter, or source-processing tool.
    # It is executable solely to obtain Bazel's FilesToRunProvider/runfiles tree.
    # Direct execution has no valid executable format; consumers only use its
    # typed path to locate <anchor>.runfiles/kernel, never execute this file.
    ctx.actions.write(anchor, "linux-source-runfiles-anchor-v1\n", is_executable = True)
    return [DefaultInfo(
        executable = anchor,
        files = depset([anchor]),
        runfiles = ctx.runfiles(root_symlinks = mapping),
    )]

linux_source_runfiles = rule(
    implementation = _linux_source_runfiles_impl,
    attrs = {
        "source_files": attr.label_list(allow_files = True, mandatory = True),
        "source_root": attr.label(allow_single_file = True, mandatory = True),
    },
    executable = True,
    doc = "Aggregates the exact complete Linux source closure without copying source bytes.",
)
