"""Semantic MODVERSIONS checks, in addition to the existing actual build tests."""

load("@rules_go//go:def.bzl", "go_test")

def modversions_metadata_test(name, kernel, modules, symvers_providers = [], dependency_consumer = None, required_imports = []):
    config = name + "_config"
    native.filegroup(name = config, srcs = [kernel], output_group = "config", testonly = True)
    symvers = []
    for index, provider in enumerate([kernel] + symvers_providers):
        selected = name + "_symvers_" + str(index)
        native.filegroup(name = selected, srcs = [provider], output_group = "module_symvers", testonly = True)
        symvers.append(":" + selected)
    if bool(dependency_consumer) != bool(required_imports):
        fail("dependency consumer and required imports must be supplied together")
    if dependency_consumer and dependency_consumer not in modules:
        fail("dependency consumer must be one of the validated modules")
    go_test(
        name = name,
        srcs = ["modversions_metadata_test.go"],
        data = modules + symvers + [":" + config],
        env = {
            "MODVERSIONS_CONFIG": "$(rlocationpath :%s)" % config,
            "MODVERSIONS_MODULES": " ".join(["$(rlocationpath %s)" % module for module in modules]),
            "MODVERSIONS_SYMVERS": " ".join(["$(rlocationpath %s)" % selected for selected in symvers]),
            "MODVERSIONS_DEPENDENCY_CONSUMER": "$(rlocationpath %s)" % dependency_consumer if dependency_consumer else "",
            "MODVERSIONS_REQUIRED_IMPORTS": " ".join(required_imports),
        },
        deps = [
            "//internal/modversions",
            "@rules_go//go/runfiles",
        ],
        pure = "on",
        rundir = ".",
    )
