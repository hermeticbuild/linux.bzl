"""Rule for constructing a Linux initramfs archive."""

load(":path_mapping.bzl", "path_mapped_run")

visibility("//...")

def _archive_name(path, attribute):
    if not path.startswith("/"):
        fail("%s path must be absolute: %s" % (attribute, path))
    if path == "/" or path.endswith("/"):
        fail("%s path must name an archive entry: %s" % (attribute, path))
    components = path[1:].split("/")
    if "" in components or "." in components or ".." in components:
        fail("%s path is not canonical: %s" % (attribute, path))
    if components[0] == "TRAILER!!!":
        fail("%s path is reserved: %s" % (attribute, path))
    return path[1:]

def _record_paths(entry_types, values, attribute):
    for path in sorted(values):
        name = _archive_name(path, attribute)
        if name in entry_types:
            fail("archive path appears more than once: /%s" % name)
        entry_types[name] = attribute

def _add_parent_directories(directory_names, entry_types, name):
    components = name.split("/")
    for count in range(1, len(components)):
        parent = "/".join(components[:count])
        attribute = entry_types.get(parent)
        if attribute != None and attribute != "directories":
            fail("/%s from %s is not a directory" % (parent, attribute))
        directory_names[parent] = None

def _add_files(args, inputs, entries, option, attribute):
    for path in sorted(entries):
        target = entries[path]
        files = target[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("%s[%s] must provide exactly one file, got %d" % (attribute, path, len(files)))
        source = files[0]
        if source.is_directory:
            fail("%s[%s] must provide a regular file" % (attribute, path))
        inputs.append(source)
        args.add_all([option, path[1:], source])

def _initramfs_impl(ctx):
    entry_types = {}
    _record_paths(entry_types, ctx.attr.directories, "directories")
    _record_paths(entry_types, ctx.attr.files, "files")
    _record_paths(entry_types, ctx.attr.executables, "executables")
    _record_paths(entry_types, ctx.attr.symlinks, "symlinks")
    _record_paths(entry_types, ctx.attr.character_devices, "character_devices")

    directory_names = {}
    for name, entry_type in entry_types.items():
        _add_parent_directories(directory_names, entry_types, name)
        if entry_type == "directories":
            directory_names[name] = None

    out = ctx.outputs.archive
    args = ctx.actions.args()
    args.add(out)

    for name in sorted(directory_names):
        args.add_all(["--directory", name])

    inputs = []
    _add_files(args, inputs, ctx.attr.files, "--file", "files")
    _add_files(args, inputs, ctx.attr.executables, "--executable", "executables")

    for path in sorted(ctx.attr.symlinks):
        link_target = ctx.attr.symlinks[path]
        if not link_target:
            fail("symlinks[%s] must not be empty" % path)
        args.add_all(["--symlink", path[1:], link_target])

    for path in sorted(ctx.attr.character_devices):
        device = ctx.attr.character_devices[path]
        fields = device.split(":")
        if len(fields) != 2:
            fail("character_devices[%s] must be MAJOR:MINOR, got %s" % (path, device))
        major = int(fields[0])
        minor = int(fields[1])
        if major < 0 or major > 0xffffffff or minor < 0 or minor > 0xffffffff:
            fail("character_devices[%s] numbers must be unsigned 32-bit integers, got %s" % (path, device))
        args.add_all(["--character-device", path[1:], major, minor])

    path_mapped_run(
        ctx.actions,
        arguments = [args],
        executable = ctx.executable._generator,
        inputs = depset(inputs),
        mnemonic = "Initramfs",
        outputs = [out],
        progress_message = "Building initramfs %{label}",
    )
    return [DefaultInfo(files = depset([out]))]

initramfs = rule(
    implementation = _initramfs_impl,
    attrs = {
        "character_devices": attr.string_dict(
            doc = "Character devices as a canonical absolute archive path to MAJOR:MINOR map. Devices use mode 0666.",
        ),
        "directories": attr.string_list(
            doc = "Canonical absolute directory paths to include in the archive. Directories use mode 0755.",
        ),
        "executables": attr.string_keyed_label_dict(
            allow_files = True,
            doc = "Executable files as a canonical absolute archive path to single-file target map. Executables use mode 0755.",
        ),
        "files": attr.string_keyed_label_dict(
            allow_files = True,
            doc = "Regular files as a canonical absolute archive path to single-file target map. Files use mode 0644.",
        ),
        "symlinks": attr.string_dict(
            doc = "Symbolic links as a canonical absolute archive path to link-target map. Symbolic links use mode 0777.",
        ),
        "_generator": attr.label(
            cfg = "exec",
            default = Label("//internal/cmd/initramfs"),
            executable = True,
        ),
    },
    doc = "Constructs a deterministic, root-owned newc initramfs archive.",
    outputs = {"archive": "%{name}.cpio"},
)
