"""Build a root-owned newc archive with a declared native gen_init_cpio tool."""

def _manifest_token(value, description):
    for character in [" ", "\t", "\n", "\r", "\f", "\v", "\000"]:
        if character in value:
            fail("%s contains whitespace or NUL, which the native manifest cannot represent: %r" % (description, value))

def _archive_path(path, attribute):
    _manifest_token(path, attribute + " path")
    if not path.startswith("/") or path == "/":
        fail("%s path must be an absolute archive filename: %s" % (attribute, path))
    components = path[1:].split("/")
    if "" in components or "." in components or ".." in components:
        fail("%s path must be canonical: %s" % (attribute, path))
    if components[0] == "TRAILER!!!":
        fail("%s path uses the reserved archive trailer name: %s" % (attribute, path))

def _collect_files(entries, targets, attribute, mode):
    for path, target in targets.items():
        _archive_path(path, attribute)
        if path in entries:
            fail("archive destination appears more than once: %s" % path)
        files = target[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("%s[%s] must provide exactly one file, got %d" % (attribute, path, len(files)))
        source = files[0]
        if source.is_directory:
            fail("%s[%s] must provide a file, not a directory" % (attribute, path))
        _manifest_token(source.path, "%s[%s] source" % (attribute, path))

        # gen_init_cpio expands ${...} in source operands; these must stay
        # literal paths to the declared input Files.
        if "${" in source.path:
            fail("source path contains native manifest environment expansion syntax: %s" % source.path)
        entries[path] = (source, mode)

def _initramfs_impl(ctx):
    entries = {}
    _collect_files(entries, ctx.attr.files, "files", "0644")
    _collect_files(entries, ctx.attr.executables, "executables", "0755")

    directories = {}
    for path in entries:
        components = path[1:].split("/")
        for count in range(1, len(components)):
            parent = "/" + "/".join(components[:count])
            if parent in entries:
                fail("archive file is also used as a parent directory: %s" % parent)
            directories[parent] = None

    lines = ["dir %s 0755 0 0" % path for path in sorted(directories)]
    inputs = []
    for path in sorted(entries):
        source, mode = entries[path]
        inputs.append(source)
        lines.append("file %s %s %s 0 0" % (path, source.path, mode))

    manifest = ctx.actions.declare_file(ctx.label.name + ".list")
    ctx.actions.write(manifest, "\n".join(lines) + "\n")
    archive = ctx.actions.declare_file(ctx.label.name + ".cpio")
    ctx.actions.run(
        executable = ctx.attr.generator[DefaultInfo].files_to_run,
        arguments = ["-t", "0", "-o", archive.path, manifest.path],
        inputs = depset([manifest] + inputs),
        outputs = [archive],
        mnemonic = "Initramfs",
        progress_message = "Building initramfs %{label}",
    )
    return [DefaultInfo(files = depset([archive]))]

initramfs = rule(
    implementation = _initramfs_impl,
    attrs = {
        "generator": attr.label(
            cfg = "exec",
            executable = True,
            mandatory = True,
            doc = "Native gen_init_cpio supporting -t for all timestamps and -o for output.",
        ),
        "files": attr.string_keyed_label_dict(
            allow_files = True,
            doc = "Canonical absolute archive paths to single-file targets, stored with mode 0644.",
        ),
        "executables": attr.string_keyed_label_dict(
            allow_files = True,
            doc = "Canonical absolute archive paths to single-file targets, stored with mode 0755.",
        ),
    },
    doc = "Builds an uncompressed root-owned newc archive with zero timestamps and parent directories of mode 0755.",
)
