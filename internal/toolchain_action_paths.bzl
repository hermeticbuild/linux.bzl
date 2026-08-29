"""Path-mapping-safe rendering for configured toolchain action contracts."""

visibility("//internal/...")

EXECUTION_ROOT_MARKER = "__LINUX_BZL_EXECROOT__"
DEFAULT_DIRECTORY_ARGUMENT_MARKER = "__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__"

def default_directory_action_argument(option, anchor):
    """Encodes a configured option whose default value is an artifact dirname."""
    if len(option) == 2 or not option.startswith("--") or "=" in option or any([character in option for character in " /\\\t\r\n".elems()]):
        fail("configured default directory option is invalid: %r" % option)
    return DEFAULT_DIRECTORY_ARGUMENT_MARKER + option + "=" + anchor.path

def _validate_path(value, what):
    if not value or value.startswith("/") or value.endswith("/") or "\\" in value:
        fail("%s has invalid relative path %r" % (what, value))
    if any([part in ["", ".", ".."] for part in value.split("/")]):
        fail("%s has invalid relative path %r" % (what, value))

def _canonical_artifact_path(value, what):
    if value.startswith("../"):
        value = "external/" + value[3:]
    if value.startswith("bazel-out/"):
        parts = value.split("/")
        if len(parts) < 4 or parts[2] not in ["bin", "genfiles"]:
            fail("%s has unrecognized Bazel output path %r" % (what, value))
        value = "/".join(parts[3:])
    _validate_path(value, what)
    return value

def _canonical_file_path(file, what):
    canonical_path = _canonical_artifact_path(file.path, what + " path")
    canonical_short_path = _canonical_artifact_path(file.short_path, what + " short_path")
    if canonical_path != canonical_short_path:
        fail("%s path %r and short_path %r map to different canonical paths %r and %r" % (
            what,
            file.path,
            file.short_path,
            canonical_path,
            canonical_short_path,
        ))
    return canonical_path

def _record_directory_path(paths, alias, canonical, kind):
    existing = paths.get(alias)
    if existing != None and existing != canonical:
        fail("toolchain action-contract %s directory %r maps to distinct canonical paths %r and %r" % (
            kind,
            alias,
            existing,
            canonical,
        ))
    paths[alias] = canonical

def _record_artifact_directory_paths(index, alias, canonical, artifact):
    """Records corresponding directory ancestors without inventing a root."""
    alias_directory = alias
    canonical_directory = canonical
    for _index in range(len(alias.split("/"))):
        if "/" not in alias_directory or "/" not in canonical_directory:
            break
        alias_directory = alias_directory.rsplit("/", 1)[0]
        canonical_directory = canonical_directory.rsplit("/", 1)[0]

        # `external` names Bazel's complete repository namespace, while `..`
        # names its sibling-layout parent. Neither proves that an arbitrary
        # directory below that root belongs to the selected toolchain.
        if alias_directory in ["", ".", ".."] or canonical_directory == "external":
            break
        directories = index.source_directories if artifact.is_source else index.output_directories
        _record_directory_path(
            directories,
            alias_directory,
            canonical_directory,
            "source" if artifact.is_source else "generated-output",
        )

def toolchain_action_path_index_from_list(toolchain_files):
    """Indexes exact Files and directory ancestors proven by their closure."""
    index = struct(
        artifacts = {},
        output_directories = {},
        source_directories = {},
    )
    for artifact in toolchain_files:
        canonical = _canonical_file_path(artifact, "toolchain action-contract artifact")
        aliases = []
        for path in [artifact.path, artifact.short_path, canonical]:
            if path not in aliases:
                aliases.append(path)
        for path in aliases:
            if not path or EXECUTION_ROOT_MARKER in path:
                fail("toolchain action-contract artifact has invalid path %r" % path)
            existing = index.artifacts.get(path)
            if existing != None and existing != artifact and existing.path != artifact.path:
                fail("toolchain action-contract path %r is provided by distinct artifacts %s and %s" % (
                    path,
                    existing,
                    artifact,
                ))
            index.artifacts[path] = artifact
            _record_artifact_directory_paths(index, path, canonical, artifact)
    return index

def toolchain_action_path_index(toolchain_files):
    return toolchain_action_path_index_from_list(toolchain_files.to_list())

def _path_occurrence(value, start, path, allow_descendant, allow_runfiles):
    end = start + len(path)
    if end < len(value) and value[end] not in "/=,:; \t":
        # Bazel maps a runfiles tree by mapping its owning executable and
        # reattaching the `.runfiles` suffix. Retaining that exact executable
        # File in Args gives the command line the same typed anchor as
        # SpawnInputExpander, without pretending an arbitrary generated
        # directory has one unique mapping.
        runfiles_end = end + len(".runfiles")
        if not (
            allow_runfiles and
            value.startswith(".runfiles", end) and
            (runfiles_end == len(value) or value[runfiles_end] in "/=,:; \t")
        ):
            return False
    if end < len(value) and value[end] == "/" and not allow_descendant:
        return False
    if start == 0 or value[start - 1] in " =,:;\t@":
        return True

    # Attached options such as -I<path> are still exact path occurrences.
    # Recognize the generic option-token shape without compiler flag names.
    token_start = 0
    for index in range(start):
        if value[index] in " =,:;\t@":
            token_start = index + 1
    option_prefix = value[token_start:start]
    return option_prefix.startswith("-") and "/" not in option_prefix

def _path_priority(kind):
    if kind == "artifact":
        return 2
    if kind == "output_directory":
        return 1
    return 0

def _select_next_path(value, cursor, paths, kind, selected):
    for path, item in paths.items():
        start = -1
        search_cursor = cursor
        exhausted = False
        for _index in range(len(value) + 1):
            if start < 0 and not exhausted:
                candidate_start = value.find(path, search_cursor)
                if candidate_start < 0:
                    exhausted = True
                elif _path_occurrence(
                    value,
                    candidate_start,
                    path,
                    # Repository rules may expose an opaque source directory
                    # as one source File (for example a prebuilt Clang
                    # resource root) even though File.is_directory is false.
                    # Its exact typed path still owns descendants in the
                    # sandbox; generated regular files do not.
                    kind == "output_directory" or (kind == "artifact" and (item.is_directory or item.is_source)),
                    kind == "artifact" and not item.is_directory,
                ):
                    start = candidate_start
                else:
                    search_cursor = candidate_start + 1
        candidate = struct(
            artifact = item if kind == "artifact" else None,
            kind = kind,
            path = path,
            start = start,
        )
        if start >= 0 and (
            selected == None or
            start < selected.start or
            (start == selected.start and len(path) > len(selected.path)) or
            (start == selected.start and len(path) == len(selected.path) and _path_priority(kind) > _path_priority(selected.kind))
        ):
            selected = candidate
    return selected

def _next_path(value, cursor, index):
    selected = _select_next_path(value, cursor, index.artifacts, "artifact", None)
    selected = _select_next_path(value, cursor, index.source_directories, "source_directory", selected)
    return _select_next_path(value, cursor, index.output_directories, "output_directory", selected)

def render_toolchain_action_value(value, path_index):
    """Returns string/File fragments that retain Bazel's typed path mapping."""
    if EXECUTION_ROOT_MARKER in value:
        fail("configured toolchain action value contains reserved execution-root marker")
    fragments = []
    cursor = 0
    substituted = False
    exhausted = False
    for _index in range(len(value) + 1):
        if cursor < len(value) and not exhausted:
            selected = _next_path(value, cursor, path_index)
            if selected == None:
                exhausted = True
            else:
                if selected.start > cursor:
                    fragments.append(value[cursor:selected.start])
                if selected.kind == "output_directory":
                    fail("configured toolchain action value %r references generated directory %r only through descendant artifacts; the selected toolchain must expose that directory as an exact File or TreeArtifact for path-mapped execution" % (
                        value,
                        selected.path,
                    ))
                fragments.extend([
                    EXECUTION_ROOT_MARKER + "/",
                    selected.artifact if selected.kind == "artifact" else selected.path,
                ])
                cursor = selected.start + len(selected.path)
                substituted = True
    if cursor < len(value):
        fragments.append(value[cursor:])
    if not fragments:
        fragments = [""]
    return struct(fragments = fragments, substituted = substituted)

def canonicalize_toolchain_action_value(value, path_index):
    """Rewrites typed action paths to the stable toolset-manifest namespace."""
    if EXECUTION_ROOT_MARKER in value:
        fail("configured toolchain action value contains reserved execution-root marker")
    fragments = []
    cursor = 0
    exhausted = False
    for _index in range(len(value) + 1):
        if cursor < len(value) and not exhausted:
            selected = _next_path(value, cursor, path_index)
            if selected == None:
                exhausted = True
            else:
                if selected.start > cursor:
                    fragments.append(value[cursor:selected.start])
                if selected.kind == "output_directory":
                    fail("configured toolchain action value %r references generated directory %r only through descendant artifacts; the selected toolchain must expose that directory as an exact File or TreeArtifact for path-mapped execution" % (
                        value,
                        selected.path,
                    ))
                if selected.kind == "artifact":
                    fragments.append(_canonical_file_path(selected.artifact, "toolchain action-contract artifact"))
                else:
                    fragments.append(path_index.source_directories[selected.path])
                cursor = selected.start + len(selected.path)
    if cursor < len(value):
        fragments.append(value[cursor:])
    return "".join(fragments)

def add_rendered_toolchain_action_value(args, flag, rendered, prefix = ""):
    if not rendered.substituted:
        args.add(flag, prefix + rendered.fragments[0])
        return
    fragments = ([prefix] if prefix else []) + rendered.fragments
    args.add_joined(
        flag,
        fragments,
        expand_directories = False,
        join_with = "",
    )
