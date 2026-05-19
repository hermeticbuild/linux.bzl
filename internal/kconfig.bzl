"""Kconfig import primitives."""

load("@bazel_lib//lib:write_source_files.bzl", "write_source_file")

KconfigInfo = provider(
    doc = "Parsed Linux Kconfig values.",
    fields = {
        "base": "Optional debug label of the base KconfigInfo this config overlays.",
        "config": "The Linux .config file backing this config.",
        "config_flags": "Dictionary mapping explicit CONFIG_* assignments to string values.",
        "set": "Dictionary of explicit values set by this config layer.",
        "unset": "List of CONFIG_* symbols explicitly removed by this config layer.",
    },
)

def _validate_config_key(key, attr_name):
    if not key.startswith("CONFIG_") or len(key) <= len("CONFIG_"):
        fail("%s key must start with CONFIG_: %s" % (attr_name, key))

def _overlay_config_content(config_flags, unset):
    lines = []
    for key in sorted(config_flags.keys()):
        lines.append("%s=%s" % (key, config_flags[key]))
    for key in sorted(unset):
        lines.append("# %s is not set" % key)
    return "\n".join(lines) + "\n"

def _kconfig_import_impl(ctx):
    updated_buildfile = ctx.actions.declare_file(ctx.label.name + ".BUILD.bazel")

    args = ctx.actions.args()
    args.add("-kconfig", ctx.file.config)
    args.add("-buildfile", ctx.file.buildfile)
    args.add("-rule", ctx.attr.import_name)
    args.add("-out", updated_buildfile)

    ctx.actions.run(
        executable = ctx.executable._kconfig,
        inputs = [
            ctx.file.config,
            ctx.file.buildfile,
        ],
        outputs = [updated_buildfile],
        arguments = [args],
        mnemonic = "UpdateKconfigImport",
        progress_message = "Updating Kconfig import %{label}",
    )

    return [
        DefaultInfo(files = depset([updated_buildfile])),
        KconfigInfo(
            base = None,
            config = ctx.file.config,
            config_flags = dict(ctx.attr.config_flags),
            set = dict(ctx.attr.config_flags),
            unset = [],
        ),
    ]

def _kconfig_info(ctx):
    return [
        DefaultInfo(files = depset([ctx.file.config])),
        KconfigInfo(
            base = None,
            config = ctx.file.config,
            config_flags = dict(ctx.attr.config_flags),
            set = dict(ctx.attr.config_flags),
            unset = [],
        ),
    ]

_kconfig_file = rule(
    implementation = _kconfig_info,
    attrs = {
        "config": attr.label(
            doc = "Linux .config file used as the source of truth.",
            allow_single_file = True,
            mandatory = True,
        ),
        "config_flags": attr.string_dict(
            doc = "Parsed explicit CONFIG_* assignments. Comment-only unset symbols are omitted.",
            default = {},
        ),
    },
    provides = [KconfigInfo],
    doc = "Provider-only imported Linux .config used by generated config BUILD files.",
)

_kconfig_import = rule(
    implementation = _kconfig_import_impl,
    attrs = {
        "buildfile": attr.label(
            doc = "BUILD file containing this kconfig_import call.",
            allow_single_file = True,
            mandatory = True,
        ),
        "config": attr.label(
            doc = "Linux .config file used as the source of truth for config_flags.",
            allow_single_file = True,
            mandatory = True,
        ),
        "config_flags": attr.string_dict(
            doc = "Parsed explicit CONFIG_* assignments. Comment-only unset symbols are omitted.",
            default = {},
        ),
        "import_name": attr.string(
            doc = "Name of the kconfig_import call to update in buildfile.",
            mandatory = True,
        ),
        "_kconfig": attr.label(
            default = "//internal/cmd/kconfig",
            executable = True,
            cfg = "exec",
        ),
    },
    provides = [KconfigInfo],
    doc = "Imports parsed Linux Kconfig values for later kernel build transitions.",
)

def kconfig_file(name, config, config_flags = {}, visibility = None, **kwargs):
    """Declare a provider-only imported Linux .config.

    This is intended for generated BUILD files and repository-rule output.
    Hand-written packages should prefer `kconfig_import` when they intentionally
    want the in-place updater.
    """
    _kconfig_file(
        name = name,
        config = config,
        config_flags = config_flags,
        visibility = visibility,
        **kwargs
    )

def kconfig_import(name, config, config_flags = {}, buildfile = None, visibility = None, update = True, **kwargs):
    """Declare an imported Linux .config.

    By default, creates two targets:

    * `name` — a write_source_file target. Running `bazel run :name` refreshes
      this BUILD file's `kconfig_import` call to reflect the current `config`.
      The matching `:name_test` target validates that the in-tree BUILD file is
      up to date.
    * `name_info` — exposes [`KconfigInfo`](#KconfigInfo) for internal rules
      that need the parsed config values or the source .config file as a
      Bazel-tracked input.

    Set `update = False` for examples or tests that only need the provider and
    should not create source-update targets.
    """
    if buildfile == None:
        buildfile = "//{}:BUILD.bazel".format(native.package_name())

    info_name = name + "_info"

    _kconfig_import(
        name = info_name,
        buildfile = buildfile,
        config = config,
        config_flags = config_flags,
        import_name = name,
        visibility = visibility,
    )

    if update:
        write_source_file(
            name = name,
            in_file = ":" + info_name,
            out_file = buildfile,
            visibility = visibility,
            **kwargs
        )

    return struct(
        config_flags = config_flags,
        label = ":" + info_name,
    )

def _kconfig_overlay_impl(ctx):
    base = ctx.attr.base[KconfigInfo]
    set_values = dict(ctx.attr.set)
    unset_values = sorted(ctx.attr.unset)

    unset_seen = {}
    for key in unset_values:
        _validate_config_key(key, "unset")
        unset_seen[key] = True

    merged = dict(base.config_flags)
    for key, value in set_values.items():
        _validate_config_key(key, "set")
        if key in unset_seen:
            fail("%s cannot be present in both set and unset" % key)
        merged[key] = value

    for key in unset_values:
        merged.pop(key, None)

    out = ctx.actions.declare_file(ctx.label.name + ".config")
    ctx.actions.write(
        output = out,
        content = _overlay_config_content(merged, unset_values),
    )

    return [
        DefaultInfo(files = depset([out])),
        KconfigInfo(
            base = str(ctx.attr.base.label),
            config = out,
            config_flags = merged,
            set = set_values,
            unset = unset_values,
        ),
    ]

kconfig_overlay = rule(
    implementation = _kconfig_overlay_impl,
    attrs = {
        "base": attr.label(
            doc = "Base Linux KconfigInfo target to overlay.",
            providers = [KconfigInfo],
            mandatory = True,
        ),
        "set": attr.string_dict(
            doc = "Explicit CONFIG_* raw user values to set. Empty string is a value, not an unset marker.",
            default = {},
        ),
        "unset": attr.string_list(
            doc = "CONFIG_* symbols to remove from raw user intent.",
            default = [],
        ),
    },
    provides = [KconfigInfo],
    doc = "Applies declarative Linux Kconfig raw-value overlays without hiding them behind generated files.",
)
