"""Clang resource-header discovery for hermetic Linux compile actions."""

visibility("//internal/...")

def _is_clang_resource_include(path):
    """Whether path is Clang's compiler-provided resource header directory."""
    normalized = path.replace("\\", "/")
    marker = "/lib/clang/"
    marker_index = normalized.rfind(marker)
    if marker_index < 0:
        return False
    suffix = normalized[marker_index + len(marker):].split("/")
    return len(suffix) == 2 and bool(suffix[0]) and suffix[1] == "include"

def _has_clang_resource_include(flags):
    for index in range(len(flags) - 3):
        if (
            flags[index] == "-Xclang" and
            flags[index + 1] == "-internal-isystem" and
            flags[index + 2] == "-Xclang" and
            _is_clang_resource_include(flags[index + 3])
        ):
            return True
    return False

def _ensure_clang_resource_include(flags, toolchain_files):
    """Adds the unique declared Clang resource include when flags omit it."""
    if _has_clang_resource_include(flags):
        return flags

    resource_includes = sorted({
        file.path: True
        for file in toolchain_files
        if _is_clang_resource_include(file.path)
    }.keys())
    if len(resource_includes) > 1:
        fail(
            "selected C/C++ toolchain provides multiple Clang resource include directories: %s" %
            ", ".join(resource_includes),
        )
    if not resource_includes:
        return flags
    return flags + [
        "-Xclang",
        "-internal-isystem",
        "-Xclang",
        resource_includes[0],
    ]

clang_resource_headers = struct(
    ensure_include = _ensure_clang_resource_include,
    is_include = _is_clang_resource_include,
)
