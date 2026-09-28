"""Focused exact-source aggregate mapping and provider-shape tests."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:linux_source_runfiles.bzl", "linux_source_runfiles", "linux_test_source_runfiles_mapping", "validate_linux_source_runfiles")

visibility("//internal/tests/...")

def _file(path, identity = "", directory = False, source = True):
    return struct(path = path, identity = identity, is_directory = directory, is_source = source)

def _mapping_test_impl(ctx):
    env = unittest.begin(ctx)
    for root in ["source", "external/linux", "../linux", "bazel-out/cfg/bin/source", "bazel-out/other/bin/source"]:
        marker = _file(root + "/.root")
        source = _file(root + "/kernel/input.S")
        header = _file(root + "/include/a header.h")
        expected = {"kernel/.root": marker, "kernel/include/a header.h": header, "kernel/kernel/input.S": source}
        actual = linux_test_source_runfiles_mapping([source, header, source], marker)
        asserts.equals(env, expected, actual)
        asserts.equals(env, sorted(expected), actual.keys())
        asserts.equals(env, actual, linux_test_source_runfiles_mapping([marker, header, source], marker))
    marker = _file(".root")
    source = _file("kernel/input.c")
    asserts.equals(env, {"kernel/.root": marker, "kernel/kernel/input.c": source}, linux_test_source_runfiles_mapping([source], marker))
    asserts.equals(env, {"kernel/.root": marker}, linux_test_source_runfiles_mapping([], marker))

    # Do not infer File authority from is_source: generated inputs beneath an
    # exact generated marker root are retained too, not silently dropped.
    generated_root = _file("bazel-out/cfg/bin/generated/.root", source = False)
    generated = _file("bazel-out/cfg/bin/generated/input.c", source = False)
    asserts.equals(env, generated, linux_test_source_runfiles_mapping([generated], generated_root)["kernel/input.c"])
    return unittest.end(env)

_mapping_test = unittest.make(_mapping_test_impl)

def _fake_info(anchor, mapping, files = None, ordinary = None, symlinks = [], empty = [], duplicate = False):
    entries = [struct(path = path, target_file = file) for path, file in mapping.items()]
    if duplicate:
        entry = entries[0]
        entries.append(struct(path = entry.path, target_file = entry.target_file, distinct_entry = True))
    return struct(
        files = depset([anchor] if files == None else files),
        files_to_run = struct(executable = anchor),
        default_runfiles = struct(
            files = depset([anchor] if ordinary == None else ordinary),
            root_symlinks = depset(entries),
            symlinks = depset(symlinks),
            empty_filenames = depset(empty),
        ),
    )

def _validation_test_impl(ctx):
    env = unittest.begin(ctx)
    anchor = _file("bazel-out/exec/bin/aggregate.anchor", source = False)
    for root in ["external/linux", "../linux", "bazel-out/target/bin/generated"]:
        marker = _file(root + "/.root")
        source = _file(root + "/input.c")
        mapping = linux_test_source_runfiles_mapping([source], marker)
        info = _fake_info(anchor, mapping)
        actual = validate_linux_source_runfiles(info, [source, source], marker)
        asserts.equals(env, info.files_to_run, actual)
        asserts.equals(env, mapping, linux_test_source_runfiles_mapping([marker, source], marker))
    return unittest.end(env)

_validation_test = unittest.make(_validation_test_impl)

def _invalid_provider_impl(ctx):
    marker = _file("bazel-out/target/bin/generated/.root", source = False)
    source = _file("bazel-out/target/bin/generated/input.c", source = False)
    anchor = _file("bazel-out/exec/bin/aggregate.anchor", source = False)
    mapping = linux_test_source_runfiles_mapping([source], marker)
    files = None
    ordinary = None
    symlinks = []
    empty = []
    duplicate = False
    if ctx.attr.case == "missing_source":
        mapping.pop("kernel/input.c")
    elif ctx.attr.case == "missing_marker":
        mapping.pop("kernel/.root")
    elif ctx.attr.case == "extra_mapping":
        mapping["kernel/extra.c"] = _file("external/other/extra.c")
    elif ctx.attr.case == "same_path_distinct_file":
        mapping["kernel/input.c"] = _file(source.path, identity = "different-owner", source = False)
    elif ctx.attr.case == "different_configuration":
        mapping["kernel/input.c"] = _file("bazel-out/exec/bin/generated/input.c", source = False)
    elif ctx.attr.case == "different_marker_configuration":
        mapping["kernel/.root"] = _file("bazel-out/exec/bin/generated/.root", source = False)
    elif ctx.attr.case == "duplicate_mapping":
        duplicate = True
    elif ctx.attr.case == "extra_files_to_build":
        files = [anchor, source]
    elif ctx.attr.case == "missing_files_to_build":
        files = []
    elif ctx.attr.case == "directory_anchor":
        anchor = _file(anchor.path, directory = True)
    elif ctx.attr.case == "extra_ordinary_runfiles":
        ordinary = [anchor, source]
    elif ctx.attr.case == "extra_symlink":
        symlinks = [struct(path = "unexpected", target_file = source)]
    elif ctx.attr.case == "extra_empty_file":
        empty = ["kernel/unexpected.h"]
    elif ctx.attr.case not in ["missing_provider", "missing_executable", "missing_runfiles"]:
        fail("unknown provider test case %s" % ctx.attr.case)
    info = _fake_info(anchor, mapping, files, ordinary, symlinks, empty, duplicate)
    if ctx.attr.case == "missing_provider":
        info = None
    elif ctx.attr.case == "missing_executable":
        info = struct(files_to_run = struct(executable = None))
    elif ctx.attr.case == "missing_runfiles":
        info = struct(files = info.files, files_to_run = info.files_to_run, default_runfiles = None)
    validate_linux_source_runfiles(info, [source], marker)
    return []

_invalid_provider = rule(implementation = _invalid_provider_impl, attrs = {"case": attr.string()})

def _generated_sources_impl(ctx):
    ctx.actions.write(ctx.outputs.marker, "Generated source root.\n")
    ctx.actions.write(ctx.outputs.source, "#define GENERATED_SOURCE_RUNFILES 1\n")
    return [DefaultInfo(files = depset([ctx.outputs.marker, ctx.outputs.source]))]

_generated_sources = rule(
    implementation = _generated_sources_impl,
    attrs = {"marker": attr.output(mandatory = True), "source": attr.output(mandatory = True)},
)

def _validate_wrapper_impl(ctx):
    validate_linux_source_runfiles(ctx.attr.wrapper[DefaultInfo], ctx.files.source_files, ctx.file.source_root)
    return []

_validate_wrapper = rule(
    implementation = _validate_wrapper_impl,
    attrs = {
        "source_files": attr.label_list(allow_files = True),
        "source_root": attr.label(allow_single_file = True),
        "wrapper": attr.label(mandatory = True),
    },
)

_validate_exec_wrapper = rule(
    implementation = _validate_wrapper_impl,
    attrs = {
        "source_files": attr.label_list(allow_files = True),
        "source_root": attr.label(allow_single_file = True),
        "wrapper": attr.label(cfg = "exec", mandatory = True),
    },
)

def _successful_validation_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.equals(env, [], analysistest.target_under_test(env)[DefaultInfo].files.to_list())
    return analysistest.end(env)

_successful_validation_test = analysistest.make(_successful_validation_test_impl)

def _invalid_mapping_impl(ctx):
    marker = _file("external/linux/.root")
    inputs = [_file("external/linux/input.c")]
    if ctx.attr.case == "missing_marker":
        marker = None
    elif ctx.attr.case == "directory_marker":
        marker = _file("external/linux/.root", directory = True)
    elif ctx.attr.case == "directory_input":
        inputs = [_file("external/linux/include", directory = True)]
    elif ctx.attr.case == "outside_root":
        inputs = [_file("external/linux-other/input.c")]
    elif ctx.attr.case == "parent_escape":
        inputs = [_file("external/linux/../other/input.c")]
    elif ctx.attr.case == "dot_alias":
        inputs = [_file("external/linux/./input.c")]
    elif ctx.attr.case == "double_separator":
        inputs = [_file("external/linux//input.c")]
    elif ctx.attr.case == "backslash":
        inputs = [_file("external/linux/dir\\input.c")]
    elif ctx.attr.case == "newline":
        inputs = [_file("external/linux/line\ninput.c")]
    elif ctx.attr.case == "nul":
        inputs = [_file("external/linux/line\000input.c")]
    elif ctx.attr.case == "absolute_marker":
        marker = _file("/external/linux/.root")
    elif ctx.attr.case == "short_sibling_marker":
        marker = _file("../.root")
    elif ctx.attr.case == "workspace_escape":
        marker = _file(".root")
        inputs = [_file("../linux/input.c")]
    elif ctx.attr.case == "root_alias":
        inputs = [_file("external/linux")]
    elif ctx.attr.case == "distinct_artifacts":
        inputs = [_file("external/linux/input.c", "first"), _file("external/linux/input.c", "second")]
    elif ctx.attr.case == "distinct_marker":
        inputs = [_file("external/linux/.root", "not-the-marker")]
    elif ctx.attr.case == "file_directory_collision":
        inputs = [_file("external/linux/include"), _file("external/linux/include/input.h")]
    elif ctx.attr.case == "nonadjacent_collision":
        inputs = [_file("external/linux/include"), _file("external/linux/include-extra"), _file("external/linux/include/input.h")]
    else:
        fail("unknown mapping test case %s" % ctx.attr.case)
    linux_test_source_runfiles_mapping(inputs, marker)
    return []

_invalid_mapping = rule(implementation = _invalid_mapping_impl, attrs = {"case": attr.string()})

def _failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected)
    return analysistest.end(env)

_failure_test = analysistest.make(
    _failure_test_impl,
    attrs = {"expected": attr.string(mandatory = True)},
    expect_failure = True,
)

def _provider_test_impl(ctx):
    env = analysistest.begin(ctx)
    subject = analysistest.target_under_test(env)
    info = subject[DefaultInfo]
    anchor = info.files_to_run.executable
    validated = validate_linux_source_runfiles(info, ctx.files.source_files, ctx.file.source_root)
    asserts.equals(env, anchor, validated.executable)
    asserts.equals(env, [anchor], info.files.to_list())
    asserts.equals(env, True, anchor != None)
    asserts.equals(env, False, anchor.is_directory)
    actual = {entry.path: entry.target_file for entry in info.default_runfiles.root_symlinks.to_list()}
    expected = linux_test_source_runfiles_mapping(ctx.files.source_files, ctx.file.source_root)
    asserts.equals(env, expected, actual)

    # No parallel ordinary-runfiles copy of the source closure: only the
    # standard executable may appear there through DefaultInfo processing.
    ordinary = [file for file in info.default_runfiles.files.to_list() if file != anchor]
    asserts.equals(env, [], ordinary)
    actions = analysistest.target_actions(env)
    anchors = [action for action in actions if anchor in action.outputs.to_list()]
    asserts.equals(env, 1, len(anchors))
    if anchors:
        asserts.equals(env, "FileWrite", anchors[0].mnemonic)
        asserts.equals(env, [], anchors[0].inputs.to_list())
    return analysistest.end(env)

_provider_test = analysistest.make(
    _provider_test_impl,
    attrs = {
        "source_files": attr.label_list(allow_files = True),
        "source_root": attr.label(allow_single_file = True),
    },
)

def linux_source_runfiles_test(name, source_files, source_root):
    """Registers unit/negative mapping tests plus a real provider-shape test.

    Args:
      name: Name prefix and resulting test suite.
      source_files: Exact fixture source labels, intentionally omitting the marker.
      source_root: Fixture source-root marker label.
    """
    tests = []
    _mapping_test(name = name + "_mapping")
    tests.append(name + "_mapping")
    _validation_test(name = name + "_validation")
    tests.append(name + "_validation")
    linux_source_runfiles(name = name + "_subject", source_files = source_files, source_root = source_root)
    _provider_test(
        name = name + "_provider",
        source_files = source_files,
        source_root = source_root,
        target_under_test = ":" + name + "_subject",
    )
    tests.append(name + "_provider")
    marker = name + "_generated/SOURCE_ROOT"
    source = name + "_generated/include/input.h"
    _generated_sources(name = name + "_generated_sources", marker = marker, source = source)
    linux_source_runfiles(name = name + "_generated_subject", source_files = [source], source_root = marker)
    _validate_wrapper(
        name = name + "_generated_validation_subject",
        wrapper = ":" + name + "_generated_subject",
        source_files = [source],
        source_root = marker,
    )
    _successful_validation_test(
        name = name + "_generated_validation",
        target_under_test = ":" + name + "_generated_validation_subject",
    )
    tests.append(name + "_generated_validation")
    _validate_exec_wrapper(
        name = name + "_exec_source_validation_subject",
        wrapper = ":" + name + "_subject",
        source_files = source_files,
        source_root = source_root,
    )
    _successful_validation_test(
        name = name + "_exec_source_validation",
        target_under_test = ":" + name + "_exec_source_validation_subject",
    )
    tests.append(name + "_exec_source_validation")
    _validate_exec_wrapper(
        name = name + "_exec_generated_mismatch_subject",
        wrapper = ":" + name + "_generated_subject",
        source_files = [source],
        source_root = marker,
        tags = ["manual"],
    )
    _failure_test(
        name = name + "_exec_generated_mismatch",
        target_under_test = ":" + name + "_exec_generated_mismatch_subject",
        expected = "does not match original source Files",
    )
    tests.append(name + "_exec_generated_mismatch")
    for case, expected in {
        "different_configuration": "does not match original source Files",
        "different_marker_configuration": "does not match original source Files",
        "directory_anchor": "files_to_build must contain only its executable anchor",
        "duplicate_mapping": "duplicate root mapping",
        "extra_empty_file": "extra symlinks or empty runfiles",
        "extra_files_to_build": "files_to_build must contain only its executable anchor",
        "extra_mapping": "does not match original source Files",
        "extra_ordinary_runfiles": "extra ordinary runfiles",
        "extra_symlink": "extra symlinks or empty runfiles",
        "missing_executable": "requires an executable anchor provider",
        "missing_files_to_build": "files_to_build must contain only its executable anchor",
        "missing_marker": "missing original source Files",
        "missing_provider": "requires an executable anchor provider",
        "missing_runfiles": "requires default runfiles",
        "missing_source": "missing original source Files",
        "same_path_distinct_file": "does not match original source Files",
    }.items():
        subject = name + "_provider_" + case + "_subject"
        test = name + "_provider_" + case
        _invalid_provider(name = subject, case = case, tags = ["manual"])
        _failure_test(name = test, target_under_test = ":" + subject, expected = expected)
        tests.append(test)
    for case, expected in {
        "absolute_marker": "non-canonical artifact path",
        "backslash": "non-canonical artifact path",
        "directory_input": "contains a directory artifact",
        "directory_marker": "source_root must not be a directory artifact",
        "distinct_artifacts": "names distinct artifacts",
        "distinct_marker": "names distinct artifacts",
        "dot_alias": "non-canonical artifact path",
        "double_separator": "non-canonical artifact path",
        "file_directory_collision": "file/directory collision",
        "missing_marker": "requires a source_root marker",
        "newline": "non-canonical artifact path",
        "nonadjacent_collision": "file/directory collision",
        "nul": "non-canonical artifact path",
        "outside_root": "outside source root",
        "parent_escape": "non-canonical artifact path",
        "root_alias": "outside source root",
        "short_sibling_marker": "incomplete sibling-repository artifact path",
        "workspace_escape": "outside source root",
    }.items():
        subject = name + "_" + case + "_subject"
        test = name + "_" + case
        _invalid_mapping(name = subject, case = case, tags = ["manual"])
        _failure_test(name = test, target_under_test = ":" + subject, expected = expected)
        tests.append(test)
    native.test_suite(name = name, tests = tests)
