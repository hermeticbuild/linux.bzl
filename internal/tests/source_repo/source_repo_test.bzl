"""Tests for Linux source repository validation."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//internal:linux_source_repository.bzl", "linux_test_source_overlay_package_marker_action")

def _source_overlay_package_marker_test_impl(ctx):
    env = unittest.begin(ctx)
    for path, action in {
        "BUILD": "ignore",
        "nested/BUILD.bazel": "ignore",
        "Build": "reject",
        "build": "reject",
        "nested/BUILD.BAZEL": "reject",
        "nested/bUiLd.BaZeL": "reject",
    }.items():
        asserts.equals(
            env,
            action,
            linux_test_source_overlay_package_marker_action(path),
            "%s must receive the expected cross-filesystem package-marker treatment" % path,
        )
    for path in [
        "BUILD.rules",
        "Kbuild",
        "nested/Makefile",
        "nested/build.bazel.patch",
    ]:
        asserts.equals(
            env,
            None,
            linux_test_source_overlay_package_marker_action(path),
            "%s is not a Bazel package marker" % path,
        )
    for path in [
        "BUILD",
        "nested/BUILD.bazel",
        "nested/Build",
    ]:
        asserts.equals(
            env,
            None,
            linux_test_source_overlay_package_marker_action(path, is_directory = True),
            "a directory named %s is not a Bazel package marker file" % path,
        )
    return unittest.end(env)

_source_overlay_package_marker_test = unittest.make(_source_overlay_package_marker_test_impl)

def source_overlay_package_marker_test(name):
    _source_overlay_package_marker_test(name = name)
