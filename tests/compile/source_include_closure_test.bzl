"""Analysis test for cache-granular quoted source include inputs."""

load(
    "@bazel_skylib//lib:unittest.bzl",
    "analysistest",
    "asserts",
)

visibility("private")

def _has_suffix(paths, suffix):
    return any([path.endswith(suffix) for path in paths])

def _source_include_closure_test_impl(ctx):
    env = analysistest.begin(ctx)
    actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxObjectCompile"
    ]
    asserts.equals(env, 1, len(actions))
    if actions:
        inputs = [file.short_path for file in actions[0].inputs.to_list()]
        asserts.true(env, _has_suffix(inputs, "tests/compile/source/shared/first.inc"))
        asserts.true(env, _has_suffix(inputs, "tests/compile/source/shared/nested/second.inc"))
        asserts.false(env, _has_suffix(inputs, "tests/compile/source/cross_tree/unrelated.inc"))
    return analysistest.end(env)

source_include_closure_test = analysistest.make(_source_include_closure_test_impl)
