"""Bzlmod extension creating thin configured Linux image repositories."""

load(
    ":image_name_validation.bzl",
    "validate_linux_image_name",
    "validate_linux_overlay_name",
)
load(":linux_image_repository.bzl", _linux_image_repository = "linux_image")

visibility("//...")

def _root_tags(module_ctx):
    images = {}
    overlays = {}
    for module in module_ctx.modules:
        if not module.is_root and (module.tags.image or module.tags.overlay):
            fail("linux_images tags are root-module application choices")
        if not module.is_root:
            continue
        for tag in module.tags.image:
            validate_linux_image_name(tag.name, "Linux image name")
            if tag.name in images:
                fail("duplicate Linux image %r" % tag.name)
            images[tag.name] = tag
        for tag in module.tags.overlay:
            validate_linux_image_name(tag.image, "Linux overlay image name")
            validate_linux_overlay_name(tag.name)
            key = (tag.image, tag.name)
            if key in overlays:
                fail("duplicate Linux overlay %r for image %r" % (tag.name, tag.image))
            overlays[key] = tag
    return images, overlays

def _linux_images_impl(module_ctx):
    images, overlays = _root_tags(module_ctx)
    overlays_by_image = {}
    for (image, name), tag in overlays.items():
        if image not in images:
            fail("Linux overlay %r references undeclared image %r" % (name, image))
        overlays_by_image.setdefault(image, {})[name] = tag.config
    for name in sorted(images):
        image = images[name]
        _linux_image_repository(
            name = name,
            config = image.config,
            config_mode = image.config_mode,
            overlays = overlays_by_image.get(name, {}),
            platform = image.platform,
            source = image.source,
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
    implementation = _linux_images_impl,
    tag_classes = {
        "image": _image,
        "overlay": _overlay,
    },
)
