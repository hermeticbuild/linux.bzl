"""Tests the generated one-owner Linux image repository facade."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//internal:linux_image_repository.bzl", "linux_image_repository_build_files_for_test")

visibility("private")

def _linux_image_repository_test_impl(ctx):
    env = unittest.begin(ctx)
    files = linux_image_repository_build_files_for_test(
        rules_repo = "@linux.bzl",
        source_repo = "@linux_source",
        config = "//configs:base",
        config_mode = "default",
        platform = "//platforms:x86_64",
        overlays = {
            "debug": "//configs:debug",
            "lz4": "//configs:lz4",
        },
    )

    asserts.true(env, 'load("@linux.bzl//internal:mapped_kernel.bzl", "linux_mapped_image_family_targets")' in files.root)
    asserts.true(env, "linux_mapped_image_family_targets(" in files.root)
    asserts.true(env, 'config = "//configs:base"' in files.root)
    asserts.true(env, '"debug": "//configs:debug"' in files.root)
    asserts.true(env, '"lz4": "//configs:lz4"' in files.root)
    asserts.false(env, "linux_mapped_image_targets(" in files.root)
    asserts.equals(env, ["debug", "lz4"], sorted(files.variants))

    for variant in sorted(files.variants):
        content = files.variants[variant]
        asserts.true(env, "linux_mapped_image_variant_targets(" in content)
        asserts.true(env, 'family = "//:kernel__family"' in content)
        asserts.true(env, "variant = %s" % repr(variant) in content)
        asserts.false(env, "linux_mapped_image_family_targets(" in content)

    return unittest.end(env)

linux_image_repository_test = unittest.make(_linux_image_repository_test_impl)
