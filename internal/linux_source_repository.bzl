"""Hermetic Linux source repository rule."""

load(
    ":repository_utils.bzl",
    "LINUX_SOURCE_REPOSITORY_PROTOCOL",
    "linux_makefile_version",
    "repository_prefix",
)

visibility("//...")

_KERNEL_RELEASES = {
    "6.12.96": struct(
        integrity = "sha256-fS4bXVqzazoBhW5xeC2tKlTmNPsrN8CkKZje87v5V8E=",
        strip_prefix = "linux-6.12.96",
        urls = [
            "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.96.tar.xz",
        ],
    ),
    "6.18.39": struct(
        integrity = "sha256-p6fj0q6dledBlyI6jU619r56rCG25t4n6WhdABwfjLA=",
        strip_prefix = "linux-6.18.39",
        urls = [
            "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.39.tar.xz",
        ],
    ),
}

# Case-insensitive filesystems treat Linux's tools/**/Build make fragments as
# Bazel package markers. Relocate them on every host for identical repositories.
_TOOLS_BUILD_FILE = "Build"
_TOOLS_BUILD_FILE_RELOCATED = "Build.linux-bzl"
_TOOLS_MAX_DEPTH = 64
_SOURCE_OVERLAY_FILES_MARKER = "    # __LINUX_BZL_SOURCE_OVERLAY_FILES__"

def _in_tree_path(path, what):
    normalized = path.replace("\\", "/").strip("/")
    if not normalized or normalized == ".":
        fail("%s must be a non-empty source-root-relative path" % what)
    if normalized != path.replace("\\", "/") or "//" in normalized:
        fail("%s must be a normalized source-root-relative path, got %r" % (what, path))
    for component in normalized.split("/"):
        if component in ["", ".", ".."]:
            fail("%s must not escape or alias the Linux source root, got %r" % (what, path))
    return normalized

def _stage_source_overlays(rctx):
    destinations = []
    staged_files = []
    for destination in sorted(rctx.attr.source_overlays.keys()):
        normalized = _in_tree_path(destination, "source_overlays destination")
        marker = rctx.path(rctx.attr.source_overlays[destination])
        if not marker.exists or marker.is_dir:
            fail("source_overlays[%r] must name a marker file in the overlay root" % destination)
        source = marker.dirname
        rctx.watch_tree(source)
        output = rctx.path(normalized)
        if output.exists:
            fail("source_overlays destination %r already exists in the Linux source tree" % normalized)
        pending = [(source, "")]
        files = {}
        for _ in range(_TOOLS_MAX_DEPTH):
            if not pending:
                break
            current = pending
            pending = []
            for directory, relative in current:
                for name in sorted([child.basename for child in directory.readdir()]):
                    child = directory.get_child(name)
                    child_relative = relative + "/" + name if relative else name
                    if child.is_dir:
                        pending.append((child, child_relative))
                    else:
                        files[child_relative] = child
        if pending:
            fail("source_overlays[%r] exceeds maximum directory depth %d" % (destination, _TOOLS_MAX_DEPTH))
        if not files:
            fail("source_overlays[%r] identifies an empty source tree" % destination)
        for relative in sorted(files.keys()):
            staged = normalized + "/" + relative
            rctx.symlink(files[relative], staged)
            staged_files.append(staged)
        destinations.append(normalized)
    return struct(
        destinations = sorted(destinations),
        files = sorted(staged_files),
    )

def _module_kbuild_root_symbol(path):
    normalized = []
    previous_was_separator = False
    for character in path.upper().elems():
        if (
            (character >= "A" and character <= "Z") or
            (character >= "0" and character <= "9")
        ):
            normalized.append(character)
            previous_was_separator = False
        elif not previous_was_separator:
            normalized.append("_")
            previous_was_separator = True
    return "LINUX_BZL_MODULE_KBUILD_ROOT_" + "".join(normalized).strip("_")

def _module_kbuild_roots(rctx):
    roots = {}
    for raw_path, expression in rctx.attr.module_kbuild_roots.items():
        path = _in_tree_path(raw_path, "module_kbuild_roots key")
        if path in roots:
            fail("duplicate normalized module_kbuild_roots key %r" % path)
        if not expression.strip():
            fail("module_kbuild_roots[%r] must be a non-empty Kconfig expression" % raw_path)
        if "\n" in expression or "\r" in expression:
            fail("module_kbuild_roots[%r] must be a single-line Kconfig expression" % raw_path)
        if not rctx.path(path + "/Kbuild").exists and not rctx.path(path + "/Makefile").exists:
            fail("module Kbuild root %r contains neither Kbuild nor Makefile" % path)
        roots[path] = expression
    return roots

def _integrate_in_tree_modules(rctx):
    kbuild_roots = _module_kbuild_roots(rctx)

    kconfig_roots = []
    for path in rctx.attr.module_kconfig_roots:
        path = _in_tree_path(path, "module_kconfig_roots entry")
        if path in kconfig_roots:
            fail("duplicate module_kconfig_roots entry %r" % path)
        if not rctx.path(path).exists:
            fail("module Kconfig root %r does not exist" % path)
        kconfig_roots.append(path)

    if kbuild_roots:
        root = rctx.path("Kbuild")
        content = rctx.read(root).rstrip() + "\n\n# In-tree module roots added by linux.bzl.\n"
        root_lines = []
        selectors = []
        selector_paths = {}
        for path in sorted(kbuild_roots.keys()):
            expression = kbuild_roots[path]
            if expression == "y":
                root_lines.append("obj-y += %s/" % path)
                continue
            selector = _module_kbuild_root_symbol(path)
            previous_path = selector_paths.get(selector)
            if previous_path != None:
                fail(
                    "module_kbuild_roots keys %r and %r generate the same hidden Kconfig symbol %s" %
                    (previous_path, path, selector),
                )
            selector_paths[selector] = path
            selectors.append((selector, expression))
            root_lines.append("obj-$(CONFIG_%s) += %s/" % (selector, path))
        content += "\n".join(root_lines) + "\n"
        rctx.file(root, content, executable = False)

        if selectors:
            root = rctx.path("Kconfig")
            content = rctx.read(root).rstrip() + "\n\n# In-tree module root selectors added by linux.bzl.\n"
            content += "\n\n".join([
                "config %s\n\tdef_tristate %s" % (selector, expression)
                for selector, expression in selectors
            ]) + "\n"
            rctx.file(root, content, executable = False)

    if kconfig_roots:
        root = rctx.path("Kconfig")
        content = rctx.read(root).rstrip() + "\n\n# In-tree module Kconfig roots added by linux.bzl.\n"
        content += "\n".join(['source "%s"' % path for path in kconfig_roots]) + "\n"
        rctx.file(root, content, executable = False)

def _relocate_tools_build_files(rctx, tools):
    relocated = 0
    directories = [tools]
    for _ in range(_TOOLS_MAX_DEPTH):
        if not directories:
            break
        next_directories = []
        for directory in directories:
            for entry in directory.readdir(watch = "no"):
                if entry.is_dir:
                    next_directories.append(entry)
                elif entry.basename == _TOOLS_BUILD_FILE:
                    rctx.rename(
                        entry,
                        entry.dirname.get_child(_TOOLS_BUILD_FILE_RELOCATED),
                    )
                    relocated += 1
        directories = next_directories
    if directories:
        fail("Linux tools directory exceeds the supported traversal depth")
    return relocated

def _normalize_tools_build_files(rctx):
    tools = rctx.path("tools")
    if not tools.exists:
        return

    relocated = _relocate_tools_build_files(rctx, tools)
    if relocated == 0:
        return

    makefile = rctx.path("tools/build/Makefile.build")
    if not makefile.exists:
        fail("Linux tools contain Build files but no tools/build/Makefile.build")

    build_file_assignment = "build-file := $(dir)/Build\n"
    relocated_assignment = "build-file := $(dir)/%s\n" % _TOOLS_BUILD_FILE_RELOCATED
    content = rctx.read(makefile)
    if build_file_assignment not in content:
        fail(
            "Linux tools/build/Makefile.build does not contain the expected Build assignment",
        )
    rctx.file(
        makefile,
        content.replace(build_file_assignment, relocated_assignment),
        executable = False,
    )

def _linux_source_repository_impl(rctx):
    catalog = _KERNEL_RELEASES.get(rctx.attr.version)
    has_urls = len(rctx.attr.urls) != 0
    has_integrity = rctx.attr.integrity != ""
    if has_urls != has_integrity:
        fail("linux_source_repository %s requires urls and integrity together" % rctx.original_name)

    if has_urls:
        urls = rctx.attr.urls
        integrity = rctx.attr.integrity
        strip_prefix = rctx.attr.strip_prefix or (catalog.strip_prefix if catalog != None else "")
    elif catalog != None:
        urls = catalog.urls
        integrity = catalog.integrity
        strip_prefix = rctx.attr.strip_prefix or catalog.strip_prefix
    else:
        fail(
            "Linux %s is not in the maintained source catalog; set both urls and integrity" %
            rctx.attr.version,
        )
    if not integrity.startswith("sha256-"):
        fail("linux_source_repository integrity must be a sha256 SRI digest")

    if rctx.attr.patch_strip < 0:
        fail("patch_strip must be non-negative")

    rctx.download_and_extract(
        url = urls,
        integrity = integrity,
        strip_prefix = strip_prefix,
        canonical_id = "linux.bzl-source-%s-%s" % (rctx.attr.version, integrity),
    )
    for patch in rctx.attr.patches:
        rctx.patch(patch, strip = rctx.attr.patch_strip)
    _normalize_tools_build_files(rctx)
    overlays = _stage_source_overlays(rctx)
    _integrate_in_tree_modules(rctx)

    makefile = rctx.path("Makefile")
    kconfig = rctx.path("Kconfig")
    if not makefile.exists or not kconfig.exists:
        fail(
            "Linux source archive for %s must contain root Makefile and Kconfig files" %
            rctx.attr.version,
        )
    actual_version = linux_makefile_version(rctx.read(makefile))
    if actual_version != rctx.attr.version:
        fail(
            "Linux source archive version mismatch: requested %s, Makefile reports %s" %
            (rctx.attr.version, actual_version),
        )

    source_build = rctx.read(rctx.attr._source_build_file)
    source_build = source_build.replace(
        "@rules_cc",
        repository_prefix(rctx.attr._rules_cc_defs),
    )
    source_build = source_build.replace(
        "@platforms",
        repository_prefix(rctx.attr._platforms_x86_64),
    )
    if _SOURCE_OVERLAY_FILES_MARKER not in source_build:
        fail("source repository BUILD template is missing the overlay files marker")
    source_build = source_build.replace(
        _SOURCE_OVERLAY_FILES_MARKER,
        "\n".join(["    %r," % path for path in overlays.files]),
    )
    rctx.file("BUILD.bazel", source_build, executable = False)
    rctx.file(
        ".linux-bzl-source.json",
        json.encode({
            "integrity": integrity,
            "module_make_vars": rctx.attr.module_make_vars,
            "overlay_destinations": overlays.destinations,
            "protocol": LINUX_SOURCE_REPOSITORY_PROTOCOL,
            "version": actual_version,
        }) + "\n",
        executable = False,
    )
    return rctx.repo_metadata(reproducible = True)

linux_source_repository = repository_rule(
    implementation = _linux_source_repository_impl,
    attrs = {
        "integrity": attr.string(
            doc = "SHA-256 SRI digest required with explicit urls.",
        ),
        "module_kbuild_roots": attr.string_dict(
            doc = "In-tree Kbuild or Makefile roots mapped to Kconfig expressions controlling their inclusion; use y for unconditional roots.",
        ),
        "module_kconfig_roots": attr.string_list(
            doc = "In-tree Kconfig files sourced by the kernel root Kconfig.",
        ),
        "module_make_vars": attr.string_dict(
            doc = "Additional deterministic Make variables used while parsing overlaid Kconfig and Kbuild files.",
        ),
        "patch_strip": attr.int(
            default = 1,
            doc = "Number of leading path components stripped from patches.",
        ),
        "patches": attr.label_list(
            allow_files = True,
            doc = "Deterministic unified-diff patches applied with Bazel's native patcher.",
        ),
        "source_overlays": attr.string_keyed_label_dict(
            allow_files = True,
            doc = "Map from in-tree destination directories to marker files identifying source overlay roots.",
        ),
        "strip_prefix": attr.string(
            doc = "Archive directory prefix to remove. Catalog entries provide a default.",
        ),
        "urls": attr.string_list(
            doc = "Explicit mirrors for a source not selected solely from the catalog.",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Exact upstream Linux version.",
        ),
        "_source_build_file": attr.label(
            allow_single_file = True,
            default = Label("//:source_repo.BUILD.bazel"),
        ),
        "_rules_cc_defs": attr.label(
            allow_single_file = True,
            default = Label("@rules_cc//cc:defs.bzl"),
        ),
        "_platforms_x86_64": attr.label(
            default = Label("@platforms//cpu:x86_64"),
        ),
    },
    doc = "Downloads an integrity-pinned, complete upstream Linux source tree.",
)
