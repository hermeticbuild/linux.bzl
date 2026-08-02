"""Module extension and facade repositories for configured Linux images."""

load("@llvm//toolchain:selects.bzl", "platform_module_map")
load(
    ":linux_image_repository.bzl",
    _linux_image_repository = "linux_image",
    _linux_target_profile_for_platform = "linux_target_profile_for_platform",
)

visibility("//...")

_IMAGE_NAME_CHARS = "abcdefghijklmnopqrstuvwxyz0123456789_-"
_PROJECTION_TARGETS = [
    "config",
    "image",
    "kernel",
    "kernel_release",
    "module_symvers",
    "modules",
    "modules_builtin",
    "modules_builtin_modinfo",
    "modules_order",
    "system_map",
    "vmlinux",
]

def _llvm_probe_tools(module_ctx):
    os_name = module_ctx.os.name.lower()
    if os_name in ["mac os x", "macos"]:
        os_name = "macos"
    elif os_name.startswith("windows"):
        os_name = "windows"
    elif os_name != "linux":
        fail("unsupported LLVM probe host operating system %r" % module_ctx.os.name)

    arch = module_ctx.os.arch.lower()
    if arch in ["amd64", "x86_64", "x64"]:
        arch = "x86_64"
    elif arch in ["aarch64", "arm64"]:
        arch = "aarch64"
    else:
        fail("unsupported LLVM probe host architecture %r" % module_ctx.os.arch)

    anchor = platform_module_map(os_name, arch)
    suffix = ".exe" if os_name == "windows" else ""
    return struct(
        cc = anchor.relative(":bin/clang%s" % suffix),
        ld = anchor.relative(":bin/ld.lld%s" % suffix),
    )

def _validate_name(value, what):
    if not value:
        fail("%s must not be empty" % what)
    if value[0] not in "abcdefghijklmnopqrstuvwxyz":
        fail("%s %r must start with a lowercase ASCII letter" % (what, value))
    for index in range(len(value)):
        char = value[index]
        if char not in _IMAGE_NAME_CHARS:
            fail("%s %r contains unsupported character %r" % (what, value, char))

def _facade_build(graph_repo):
    lines = [
        'package(default_visibility = ["//visibility:public"])',
        "",
    ]
    for target in _PROJECTION_TARGETS:
        lines.extend([
            "alias(",
            '    name = "%s",' % target,
            '    actual = "@%s//:%s",' % (graph_repo, target),
            ")",
            "",
        ])
    return "\n".join(lines)

def _variant_facade_build(graph_repo, variant):
    lines = [
        'package(default_visibility = ["//visibility:public"])',
        "",
    ]
    for target in _PROJECTION_TARGETS:
        lines.extend([
            "alias(",
            '    name = "%s",' % target,
            '    actual = "@%s//variants/%s:%s",' % (graph_repo, variant, target),
            ")",
            "",
        ])
    return "\n".join(lines)

def _graph_facade_build(graph_repo):
    return "\n".join([
        'package(default_visibility = ["//visibility:public"])',
        "",
        "alias(",
        '    name = "metadata.json",',
        '    actual = "@%s//graph:metadata.json",' % graph_repo,
        ")",
        "",
    ])

def _linux_image_facade_repository_impl(rctx):
    rctx.file("BUILD.bazel", _facade_build(rctx.attr.graph_repo), executable = False)
    rctx.file(
        "graph/BUILD.bazel",
        _graph_facade_build(rctx.attr.graph_repo),
        executable = False,
    )
    for variant in sorted(rctx.attr.variants):
        rctx.file(
            "variants/%s/BUILD.bazel" % variant,
            _variant_facade_build(rctx.attr.graph_repo, variant),
            executable = False,
        )
    return rctx.repo_metadata(reproducible = True)

_linux_image_facade_repository = repository_rule(
    implementation = _linux_image_facade_repository_impl,
    attrs = {
        "graph_repo": attr.string(mandatory = True),
        "variants": attr.string_list(),
    },
)

def _root_tags(module_ctx):
    images = {}
    overlays = {}
    for module in module_ctx.modules:
        if not module.is_root and (module.tags.image or module.tags.overlay):
            fail("linux_images tags are root-module application choices")
        if not module.is_root:
            continue
        for tag in module.tags.image:
            _validate_name(tag.name, "Linux image name")
            if tag.name in images:
                fail("duplicate Linux image %r" % tag.name)
            images[tag.name] = tag
        for tag in module.tags.overlay:
            _validate_name(tag.image, "Linux overlay image name")
            _validate_name(tag.name, "Linux overlay name")
            if tag.name == "base":
                fail("Linux overlay name must not be \"base\"")
            key = (tag.image, tag.name)
            if key in overlays:
                fail("duplicate Linux overlay %r for image %r" % (tag.name, tag.image))
            overlays[key] = tag
    generated_repositories = {}
    for name in sorted(images):
        for repository in [
            name,
            name + "__linux_graph",
        ]:
            owner = generated_repositories.get(repository)
            if owner != None:
                fail(
                    "Linux images %r and %r generate conflicting repository %r" %
                    (owner, name, repository),
                )
            generated_repositories[repository] = name
    return images, overlays

def _linux_images_impl(module_ctx):
    images, overlays = _root_tags(module_ctx)
    probe_tools = _llvm_probe_tools(module_ctx)
    overlays_by_image = {}
    for (image, name), tag in overlays.items():
        if image not in images:
            fail("Linux overlay %r references undeclared image %r" % (name, image))
        overlays_by_image.setdefault(image, {})[name] = tag.config

    for name in sorted(images):
        image = images[name]
        graph_repo = name + "__linux_graph"
        image_overlays = overlays_by_image.get(name, {})
        target_profile = _linux_target_profile_for_platform(image.platform)
        _linux_image_repository(
            name = graph_repo,
            config = image.config,
            config_mode = image.config_mode,
            overlays = image_overlays,
            platform = image.platform,
            probe_cc = probe_tools.cc,
            probe_ld = probe_tools.ld,
            source = image.source,
            target_profile = target_profile.name,
            linux_arch = target_profile.linux_arch,
            target_triple = target_profile.target_triple,
        )
        _linux_image_facade_repository(
            name = name,
            graph_repo = graph_repo,
            variants = sorted(image_overlays.keys()),
        )

_image = tag_class(attrs = {
    "config": attr.label(mandatory = True),
    "config_mode": attr.string(default = "default", values = ["allnoconfig", "default"]),
    "name": attr.string(mandatory = True),
    "platform": attr.label(mandatory = True),
    "source": attr.label(mandatory = True),
})

_overlay = tag_class(attrs = {
    "config": attr.label(mandatory = True),
    "image": attr.string(mandatory = True),
    "name": attr.string(mandatory = True),
})

linux_images = module_extension(
    arch_dependent = True,
    implementation = _linux_images_impl,
    os_dependent = True,
    tag_classes = {
        "image": _image,
        "overlay": _overlay,
    },
)
