"""Declare all raw sources across native Build package boundaries."""

def kernel_source_files(name):
    """Aggregate each immediate source package once, preserving native names."""
    package = native.package_name()
    prefix = "//" + (package + "/" if package else "")
    native.filegroup(
        name = name,
        srcs = native.glob(["**"], exclude = [".git/**", "BUILD.bazel"]) + [
            prefix + child + ":sources"
            for child in native.subpackages(include = ["**"], allow_empty = True)
        ],
        visibility = ["//visibility:public"],
    )
