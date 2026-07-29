"""Bzlmod extensions for configured Linux kernel images."""

load("//internal:linux_image_extension.bzl", _linux_images = "linux_images")

visibility("public")

linux_images = _linux_images
