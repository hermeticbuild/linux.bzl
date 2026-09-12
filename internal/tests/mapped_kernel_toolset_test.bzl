"""Tests for mapped-kernel toolsets and recipe-opaque input binding."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("@rules_cc//cc:find_cc_toolchain.bzl", "CC_TOOLCHAIN_TYPE")
load("//internal:execution_platform.bzl", "linux_execution_platform_attr", "linux_execution_platform_label")
load(
    "//internal:mapped_kernel.bzl",
    "expand_linux_family_plan",
    "linux_map_directory_tools",
    "linux_test_add_family_input_set_bindings",
    "linux_test_add_family_source_binding",
    "linux_test_add_input_set_args",
    "linux_test_artifact_root_relative",
    "linux_test_bind_input_sets",
    "linux_test_canonical_file_path",
    "linux_test_canonical_rust_source_root",
    "linux_test_canonicalize_toolchain_action_value",
    "linux_test_companion_tool_bindings",
    "linux_test_composed_tree_base_paths",
    "linux_test_declare_working_output",
    "linux_test_decode_packed_node_inputs",
    "linux_test_decode_packed_source_projections",
    "linux_test_execution_root_marker",
    "linux_test_family_cut_dependencies",
    "linux_test_family_execution_input_sets",
    "linux_test_family_execution_segments",
    "linux_test_family_kernel_tree_binding",
    "linux_test_family_pinned_outputs",
    "linux_test_family_segment_prior_outputs",
    "linux_test_family_store_path",
    "linux_test_family_tree_input",
    "linux_test_filtered_toolchain_closure_files",
    "linux_test_host_dependency_compile_flags",
    "linux_test_host_dependency_library_search_flags",
    "linux_test_host_library_artifact",
    "linux_test_input_set_dependencies",
    "linux_test_kbuild_action_name",
    "linux_test_merge_action_arguments",
    "linux_test_node_action_contract_role",
    "linux_test_node_input_artifact_tree_roots",
    "linux_test_node_source_closure_keys",
    "linux_test_node_uses_runtime_toolset",
    "linux_test_parse_family_execution",
    "linux_test_parse_plan_marker_paths",
    "linux_test_record_family_source_child",
    "linux_test_register_family_execution_segment",
    "linux_test_render_toolchain_action_contracts",
    "linux_test_render_toolchain_action_value",
    "linux_test_resolve_node_input_bindings",
    "linux_test_rewrite_host_dependency_link_flag",
    "linux_test_runtime_tool_bindings",
    "linux_test_selected_family_prior_outputs",
    "linux_test_selected_input_sets",
    "linux_test_selected_prior_outputs",
    "linux_test_source_input_namespace_names",
    "linux_test_tool_file",
    "linux_test_toolset_execution_requirements",
    "linux_test_tree_input_directory_name",
    "linux_test_validate_family_execution_platforms",
    "linux_test_validate_host_dependency_artifact",
    "linux_test_with_compile_action_arguments",
    "linux_test_with_link_runtime_arguments",
    "linux_test_with_rust_toolchain",
)
load("//internal:probe_map_directory.bzl", "linux_probe_map_directory_tools")
load("//internal:providers.bzl", "LinuxKernelInfo", "LinuxMappedKernelFamilyInfo", "LinuxModuleInfo", "LinuxModuleSdkInfo", "LinuxModuleTreeInfo")

visibility("private")

_PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE = str(Label("@rules_python//python:exec_tools_toolchain_type"))
_FIRST_PIN_TOOLCHAIN_TYPE = str(Label("//internal/tests:rust_guard_pin_toolchain_type"))
_SECOND_PIN_TOOLCHAIN_TYPE = str(Label("//internal/tests:rust_guard_second_pin_toolchain_type"))
_FIRST_EXECUTION_PLATFORM = Label("//internal/tests:rust_guard_first_platform")
_SECOND_EXECUTION_PLATFORM = Label("//internal/tests:rust_guard_second_platform")
_EXEC_PYTHON_INTERPRETER = "mapped_kernel_exec_python_interpreter.sh"

def _exec_python_interpreter_impl(ctx):
    interpreter = ctx.actions.declare_file(_EXEC_PYTHON_INTERPRETER)
    ctx.actions.write(
        output = interpreter,
        content = "#!/bin/sh\n",
        is_executable = True,
    )
    runtime = struct(
        files = depset([interpreter]),
        interpreter = interpreter,
    )
    return [
        DefaultInfo(
            executable = interpreter,
            files = depset([interpreter]),
        ),
        platform_common.ToolchainInfo(py3_runtime = runtime),
    ]

_exec_python_interpreter = rule(
    implementation = _exec_python_interpreter_impl,
    executable = True,
)

def _exec_python_toolchain_impl(ctx):
    return [platform_common.ToolchainInfo(
        exec_tools = struct(exec_interpreter = ctx.attr.interpreter),
    )]

_exec_python_toolchain = rule(
    implementation = _exec_python_toolchain_impl,
    attrs = {
        "interpreter": attr.label(
            cfg = "exec",
            mandatory = True,
        ),
    },
)

def _flag_values(argv, flag):
    return [argv[index + 1] for index in range(len(argv) - 1) if argv[index] == flag]

def _action_path_names_artifact(value, artifact):
    # Analysis tests see path-mapped argv under bazel-out/cfg while Artifact.path
    # retains its configuration-specific directory. The repository-relative
    # suffix is the stable identity shared by both views.
    return value == artifact.path or value.endswith("/" + artifact.short_path)

def _named_action_path_names_artifact(value, name, artifact):
    prefix = name + "="
    return value.startswith(prefix) and _action_path_names_artifact(value[len(prefix):], artifact)

def _assert_manifest_derived_rust(env, action):
    variables = _flag_values(action.argv, "-var")
    asserts.equals(env, 0, len([arg for arg in action.argv if arg == "-rust_toolchain_contract"]))
    asserts.equals(env, 1, len([value for value in variables if value.startswith("RUST_LIB_SRC=")]))
    for prefix in ["BINDGEN=", "HOSTRUSTC=", "RUSTC=", "RUSTC_OR_CLIPPY=", "RUSTC_VERSION_TEXT="]:
        asserts.equals(
            env,
            0,
            len([value for value in variables if value.startswith(prefix)]),
            "%s must come from the identity-bound toolset manifest or an execution-time probe" % prefix[:-1],
        )

def _assert_selected_rust_sources_are_inputs(env, action):
    roots = [
        value[len("RUST_LIB_SRC="):]
        for value in _flag_values(action.argv, "-var")
        if value.startswith("RUST_LIB_SRC=")
    ]
    asserts.equals(env, 1, len(roots))
    if roots:
        prefix = roots[0] + "/"
        asserts.true(
            env,
            any([file.path == roots[0] or file.path.startswith(prefix) for file in action.inputs.to_list()]),
            "%s must declare the selected Rust source closure below %s" % (action.mnemonic, roots[0]),
        )

def _fake_declare_file(filename, directory):
    root = getattr(directory, "path", str(directory))
    path = root + "/" + filename
    return struct(kind = "file", name = filename, parent = directory, path = path, short_path = path, tree_relative_path = filename)

def _fake_args(values):
    typed_values = []
    param_file = {
        "argument": None,
        "format": None,
        "use_always": False,
    }

    def add(*args):
        values.extend(args)

    def add_all(args, expand_directories = True, format_each = None):
        if expand_directories not in [True, False]:
            fail("fake Args.add_all requires a boolean expand_directories value")
        for value in args:
            if type(value) != "string":
                typed_values.append(value)
            values.append(format_each % value if format_each != None else value)

    def add_joined(flag, args, expand_directories = True, join_with = ""):
        if expand_directories:
            fail("fake joined artifact binding must not expand directories")
        typed_values.extend([value for value in args if type(value) != "string"])
        values.extend([flag, join_with.join([str(value) for value in args])])

    def use_param_file(argument, use_always = False):
        param_file["argument"] = argument
        param_file["use_always"] = use_always

    def set_param_file_format(format):
        param_file["format"] = format

    return struct(
        add = add,
        add_all = add_all,
        add_joined = add_joined,
        param_file = param_file,
        set_param_file_format = set_param_file_format,
        typed_values = typed_values,
        use_param_file = use_param_file,
        values = values,
    )

def _fake_map_directory_context():
    actions = []

    def args():
        return _fake_args([])

    def run(**kwargs):
        actions.append(kwargs)

    return struct(
        actions = actions,
        template_ctx = struct(
            args = args,
            declare_file = _fake_declare_file,
            run = run,
        ),
    )

def _fake_tree_child(path):
    return struct(
        path = path,
        short_path = path,
        tree_relative_path = path,
    )

def _family_view_batch_fixture():
    node = "a" * 64
    future_producer = "c" * 64
    future_consumer = "d" * 64
    recipe = "b" * 64
    bindings = "c" * 64
    input_set = "f" * 64
    future_input_set = "4" * 64
    input_set_source = "src-" + ("1" * 64)
    input_set_source_artifact = _fake_tree_child("include/family-closure.h")
    host_identity = "sha256-" + ("d" * 64)
    target_identity = "sha256-" + ("e" * 64)
    outputs = [
        struct(path = "built/first.o", slot = "00000000", tree = "objects"),
        struct(path = "built/second.o", slot = "00000001", tree = "objects"),
        struct(path = "kernel", slot = "00000002", tree = "image"),
        struct(path = "Module.symvers", slot = "00000003", tree = "metadata"),
        struct(path = "modules.builtin", slot = "00000004", tree = "metadata"),
    ]
    paths = [
        "schema/linux-kernel-family-plan-v7",
        "index/00000000/" + node,
        "index/00000001/" + future_producer,
        "index/00000002/" + future_consumer,
        "toolsets/host/" + host_identity,
        "toolsets/target/" + target_identity,
        "recipes/" + recipe + ".json",
        "sources/%s/kernel/%s" % (input_set_source, input_set_source_artifact.tree_relative_path),
        "input-sets/%s/manifest/%s.json" % (input_set, input_set),
        "input-sets/%s/in/source/00000000/%s" % (input_set, input_set_source),
        "input-sets/%s/manifest/%s.json" % (future_input_set, future_input_set),
        "input-sets/%s/in/node/00000000/%s/00000000" % (future_input_set, future_producer),
        "nodes/target/%s/kind/copy" % node,
        "nodes/target/%s/product/image" % node,
        "nodes/target/%s/recipe/%s" % (node, recipe),
        "nodes/target/%s/tool/actionfile" % node,
        "nodes/target/%s/in/bindings/%s.json" % (node, bindings),
        "nodes/target/%s/in/input-set/%s" % (node, input_set),
        "nodes/host/%s/kind/generate" % future_producer,
        "nodes/host/%s/product/sdk" % future_producer,
        "nodes/host/%s/recipe/%s" % (future_producer, recipe),
        "nodes/host/%s/tool/generated" % future_producer,
        "nodes/host/%s/in/bindings/%s.json" % (future_producer, bindings),
        "nodes/host/%s/out/host/00000000/at/future-producer" % future_producer,
        "nodes/host/%s/kind/generate" % future_consumer,
        "nodes/host/%s/product/sdk" % future_consumer,
        "nodes/host/%s/recipe/%s" % (future_consumer, recipe),
        "nodes/host/%s/tool/generated" % future_consumer,
        "nodes/host/%s/in/bindings/%s.json" % (future_consumer, bindings),
        "nodes/host/%s/in/input-set/%s" % (future_consumer, future_input_set),
        "nodes/host/%s/out/host/00000000/at/future-consumer" % future_consumer,
    ]
    for output in outputs:
        paths.append("nodes/target/%s/out/%s/%s/at/%s" % (
            node,
            output.tree,
            output.slot,
            output.path,
        ))
    for variant in ["base", "debug"]:
        for output in outputs:
            paths.append("variants/%s/view/%s/from/%s/%s/at/%s" % (
                variant,
                output.tree,
                node,
                output.slot,
                output.path,
            ))

    input_directories = {
        "host_toolset_identity": struct(children = [_fake_tree_child(host_identity)]),
        "plan": struct(
            children = [_fake_tree_child(path) for path in paths],
            directory = "family-plan",
        ),
        "target_toolset_identity": struct(children = [_fake_tree_child(target_identity)]),
    }
    output_directories = {
        "image": "image-store",
        "metadata": "metadata-store",
        "objects": "objects-store",
        "work": "work-tree",
    }
    for variant in ["base", "debug"]:
        for tree in ["image", "metadata", "objects"]:
            output_directories["view@%s@%s" % (variant, tree)] = "%s-%s-view" % (variant, tree)
    return struct(
        additional_inputs = {
            "source_files": depset([input_set_source_artifact]),
            "source_root": "source-root",
        },
        additional_params = {
            "family_emit_views": True,
            "family_scope": "target",
            "family_stages": "prep,target",
            "source_prefix": "",
        },
        input_directories = input_directories,
        input_set = input_set,
        future_input_set = future_input_set,
        input_set_source = input_set_source,
        input_set_source_artifact = input_set_source_artifact,
        node_id = node,
        output_directories = output_directories,
        tools = {
            "runner": "recipe-runner",
            "target@actionfile": _fake_tree_child("actionfile"),
        },
    )

def _family_execution_markers(mode, node_ids, seal = "9" * 64):
    return struct(children = [_fake_tree_child(marker) for marker in [
        "schema/linux-kernel-family-execution-v1",
        "mode/" + mode,
        "seal/" + seal,
    ] + [mode + "/" + node_id for node_id in node_ids]])

def _family_execution_action_signature(action):
    inputs = action["inputs"]
    if type(inputs) == "depset":
        inputs = inputs.to_list()
    return struct(
        inputs = sorted([str(file) for file in inputs]),
        arguments = [args.values for args in action["arguments"]],
        outputs = sorted([str(file) for file in action["outputs"]]),
    )

def _family_source_aggregate_cases(env):
    fixture = _family_view_batch_fixture()
    anchor = _fake_tree_child("bazel-out/exec/bin/linux-source.anchor")
    aggregate = struct(executable = anchor)
    tools = dict(fixture.tools)
    tools["source_runfiles"] = aggregate
    unrelated = _fake_tree_child("not-read-by-precise-source.c")
    additional_inputs = dict(fixture.additional_inputs)
    additional_inputs["source_files"] = depset([fixture.input_set_source_artifact, unrelated])
    prefix = "nodes/target/" + fixture.node_id + "/in/"
    for precise in [False, True]:
        markers = [
            prefix + "tree/kernel",
            prefix + "source/kernel-source/00000000/" + fixture.input_set_source,
        ]
        if precise:
            markers.extend([
                prefix + "source-tree-bindings/" + ("7" * 64) + ".json",
                prefix + "source-tree-pack/kernel/00000000.0",
            ])
        directories = dict(fixture.input_directories)
        directories["plan"] = struct(
            children = fixture.input_directories["plan"].children + [_fake_tree_child(marker) for marker in markers],
            directory = fixture.input_directories["plan"].directory,
        )
        directories["execution"] = _family_execution_markers("cut", [fixture.node_id])
        params = dict(fixture.additional_params)
        params["family_emit_views"] = False
        fake = _fake_map_directory_context()
        expand_linux_family_plan(fake.template_ctx, directories, fixture.output_directories, additional_inputs, tools, params)
        asserts.equals(env, 1, len(fake.actions))
        if not fake.actions:
            continue
        action = fake.actions[0]
        inputs = action["inputs"].to_list()
        argv = action["arguments"][0].values
        asserts.false(env, unrelated in inputs, "neither path should flatten the complete source filegroup")
        asserts.true(env, fixture.input_set_source_artifact in inputs, "exact source proof File stays declared")
        asserts.equals(env, not precise, aggregate in action["tools"])
        if precise:
            asserts.equals(env, ["kernel"], _flag_values(argv, "-private_input_tree"))
            asserts.false(env, anchor in action["arguments"][0].typed_values)
        else:
            root = str(anchor) + ".runfiles/kernel"
            source = root + "/" + fixture.input_set_source_artifact.tree_relative_path
            asserts.equals(env, ["kernel=" + root], _flag_values(argv, "-input_tree"))
            asserts.equals(env, ["kernel-source:00000000=" + source], _flag_values(argv, "-source"))
            asserts.equals(env, [fixture.input_set_source + "=" + source], _flag_values(argv, "-input_set_source"))
            asserts.equals(env, [], _flag_values(argv, "-private_input_tree"))
            asserts.false(env, "source-root" in inputs)

def _family_execution_success_cases(env):
    fixture = _family_view_batch_fixture()

    # The complete plan/index is unchanged. Only the current target node runs;
    # host nodes listed in the same cut do not run in this expansion segment.
    input_directories = dict(fixture.input_directories)

    # The unselected future consumer is also in the target segment, so the
    # cut filter must be conjoined with (not substituted for) the stage filter.
    input_directories["plan"] = struct(
        children = [_fake_tree_child(child.tree_relative_path.replace(
            "nodes/host/" + ("d" * 64),
            "nodes/target/" + ("d" * 64),
        ).replace(
            "out/host/00000000/at/future-consumer",
            "out/metadata/00000000/at/future-consumer",
        )) for child in fixture.input_directories["plan"].children],
        directory = fixture.input_directories["plan"].directory,
    )
    input_directories["execution"] = _family_execution_markers("cut", [fixture.node_id, "c" * 64])
    params = dict(fixture.additional_params)
    params["family_emit_views"] = False
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, input_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, params)
    asserts.equals(env, 1, len(fake.actions))
    if fake.actions:
        action = fake.actions[0]
        asserts.true(env, action["progress_message"].startswith("Building Linux target/"))
        for marker in input_directories["execution"].children:
            if marker.tree_relative_path.startswith("seal/"):
                asserts.false(env, marker in action["inputs"].to_list())
            elif marker.tree_relative_path != "cut/" + ("c" * 64):
                asserts.true(env, marker in action["inputs"].to_list())

    # The callback still requires a valid seal, but changing only the complete
    # family contract must not salt an otherwise identical generated spawn.
    before = fake.actions
    input_directories["execution"] = _family_execution_markers("cut", [fixture.node_id, "c" * 64], seal = "8" * 64)
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, input_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, params)
    asserts.equals(env, [_family_execution_action_signature(action) for action in before], [_family_execution_action_signature(action) for action in fake.actions])

    # A segment shard still carries the complete family index. A selected
    # producer from another segment need not have a local node declaration.
    input_directories["plan"] = struct(
        children = [
            child
            for child in fixture.input_directories["plan"].children
            if not child.tree_relative_path.startswith("nodes/host/") and
               not child.tree_relative_path.startswith("input-sets/" + fixture.future_input_set + "/")
        ],
        directory = fixture.input_directories["plan"].directory,
    )
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, input_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, params)
    asserts.equals(env, 1, len(fake.actions))

    # A deliberately empty cut still has an explicit authenticated mode and
    # produces no actions or facade copies.
    input_directories["execution"] = _family_execution_markers("cut", [])
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, input_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, params)
    asserts.equals(env, [], fake.actions)

    # All current slots, across three stores, are copied and keep final views.
    # An ordinary recipe/private-work action must not be registered for a pin.
    input_directories = dict(fixture.input_directories)
    input_directories["plan"] = struct(
        children = fixture.input_directories["plan"].children + [_fake_tree_child(
            "nodes/target/%s/out/objects/00000005/at/observed/.vmlinux.export.c" % fixture.node_id,
        )],
        directory = fixture.input_directories["plan"].directory,
    )
    input_directories["execution"] = _family_execution_markers("pinned", [fixture.node_id])
    slots_by_tree = {"objects": ["00000000", "00000001", "00000005"], "image": ["00000002"], "metadata": ["00000003", "00000004"]}
    for tree, slots in slots_by_tree.items():
        input_directories["cut-" + tree] = struct(children = [_fake_tree_child(linux_test_family_store_path(fixture.node_id, slot)) for slot in slots])
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, input_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, fixture.additional_params)
    copies = [action for action in fake.actions if action["progress_message"].startswith("Importing pinned Linux ")]

    # Slot 5 has no facade view. Copying only viewed/header slots would drop the
    # generator's observed-state sidecar and invalidate later recipe inputs.
    asserts.equals(env, 6, len(copies))
    asserts.equals(env, 6, len([action for action in fake.actions if action["progress_message"].startswith("Projecting Linux ")]))
    asserts.equals(env, [], [action for action in fake.actions if action["progress_message"].startswith("Building Linux ")])
    for action in copies:
        asserts.equals(env, 1, len(action["outputs"]))
        asserts.true(env, "-preserve_mode" in action["arguments"][0].values)
        asserts.equals(env, 4, len(action["inputs"]))
        for marker in input_directories["execution"].children:
            if marker.tree_relative_path.startswith("seal/"):
                asserts.false(env, marker in action["inputs"])
            else:
                asserts.true(env, marker in action["inputs"])
        asserts.false(env, any([output.parent == fixture.output_directories["work"] for output in action["outputs"]]))

    before = fake.actions
    input_directories["execution"] = _family_execution_markers("pinned", [fixture.node_id], seal = "8" * 64)
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, input_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, fixture.additional_params)
    asserts.equals(env, [_family_execution_action_signature(action) for action in before], [_family_execution_action_signature(action) for action in fake.actions])

    # An unpinned consumer in the same expansion must bind the newly copied
    # current-store artifact, not look for a prior-segment or original cut leaf.
    consumer = "e" * 64
    consumer_set = "2" * 64
    mixed_directories = dict(input_directories)
    mixed_directories["plan"] = struct(
        children = input_directories["plan"].children + [_fake_tree_child(path) for path in [
            "index/00000003/" + consumer,
            "input-sets/%s/manifest/%s.json" % (consumer_set, consumer_set),
            "input-sets/%s/in/node/00000000/%s/00000000" % (consumer_set, fixture.node_id),
            "nodes/target/%s/kind/copy" % consumer,
            "nodes/target/%s/product/image" % consumer,
            "nodes/target/%s/recipe/%s" % (consumer, "b" * 64),
            "nodes/target/%s/tool/actionfile" % consumer,
            "nodes/target/%s/in/bindings/%s.json" % (consumer, "c" * 64),
            "nodes/target/%s/in/input-set/%s" % (consumer, consumer_set),
            "nodes/target/%s/out/objects/00000000/at/consumer.o" % consumer,
        ]],
        directory = input_directories["plan"].directory,
    )
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, mixed_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, params)
    builds = [action for action in fake.actions if action["progress_message"].startswith("Building Linux ")]
    asserts.equals(env, 1, len(builds))
    copied_inputs = [
        action["outputs"][0]
        for action in fake.actions
        if action["progress_message"].startswith("Importing pinned Linux ") and
           action["outputs"][0].name == linux_test_family_store_path(fixture.node_id, "00000000")
    ]
    asserts.equals(env, 1, len(copied_inputs))
    if builds and copied_inputs:
        asserts.true(env, copied_inputs[0] in builds[0]["inputs"].to_list())
        asserts.false(env, input_directories["cut-objects"].children[0] in builds[0]["inputs"].to_list())

    # Pinned nodes need not retain their original source-only InputSet runtime
    # bindings, while an empty pin set runs the original recipe normally.
    input_directories = dict(fixture.input_directories)
    input_directories["execution"] = _family_execution_markers("pinned", [])
    fake = _fake_map_directory_context()
    expand_linux_family_plan(fake.template_ctx, input_directories, fixture.output_directories, fixture.additional_inputs, fixture.tools, params)
    asserts.equals(env, 1, len(fake.actions))
    asserts.true(env, fake.actions[0]["progress_message"].startswith("Building Linux "))
    for marker in input_directories["execution"].children:
        if marker.tree_relative_path.startswith("seal/"):
            asserts.false(env, marker in fake.actions[0]["inputs"].to_list())
        else:
            asserts.true(env, marker in fake.actions[0]["inputs"].to_list())

    # Shared persistent subtrees are selected once across all consumer roots;
    # unrelated sets are not retained or checked as cut dependencies.
    leaf = {"children": {}, "entries": {0: struct(kind = "source", source = "header")}}
    input_sets = struct(
        nodes = {
            "first": {"children": {"0": "shared"}, "entries": {}},
            "second": {"children": {"1": "shared"}, "entries": {}},
            "shared": leaf,
            "unselected": {"children": {}, "entries": {0: struct(kind = "node", producer = "not-in-cut", slot = "00000000")}},
        },
        postorder = ["shared", "unselected", "first", "second"],
    )
    selected = linux_test_family_execution_input_sets(input_sets, {
        "one": {"input_set": "first"},
        "two": {"input_set": "second"},
        "duplicate-root": {"input_set": "first"},
    })
    asserts.equals(env, ["first", "second", "shared"], sorted(selected.nodes))
    asserts.equals(env, ["shared", "first", "second"], selected.postorder)
    linux_test_family_cut_dependencies(struct(mode = "cut", nodes = {}), {}, selected)

def _family_execution_failure_probe_impl(ctx):
    node_id = "a" * 64
    other = "c" * 64
    index = struct(ordinals = {node_id: 0, other: 1})
    mode = ctx.attr.mode
    markers = ["schema/linux-kernel-family-execution-v1", "mode/pinned", "seal/" + ("9" * 64), "pinned/" + node_id]
    if mode == "mixed_mode":
        markers[-1] = "cut/" + node_id
    elif mode == "unknown_node":
        markers[-1] = "pinned/" + ("b" * 64)
    elif mode == "invalid_seal":
        markers[2] = "seal/" + ("A" * 64)
    elif mode == "duplicate_mode":
        markers.append("mode/pinned")
    elif mode == "duplicate_schema":
        markers.append(markers[0])
    elif mode == "duplicate_node":
        markers.append(markers[-1])
    elif mode == "duplicate_seal":
        markers.append("seal/" + ("8" * 64))
    elif mode == "missing_seal":
        markers.pop(2)
    elif mode == "missing_schema":
        markers.pop(0)
    elif mode == "missing_mode":
        markers.pop(1)
    elif mode == "unknown_marker":
        markers.append("extra/manifest")
    elif mode == "node_bound":
        markers = ["unused" for _ in range(4100)]
    execution = linux_test_parse_family_execution(struct(children = [_fake_tree_child(marker) for marker in markers]), index)
    if mode in ["direct_not_closed", "set_not_closed"]:
        execution = linux_test_parse_family_execution(_family_execution_markers("cut", [node_id]), index)
        decoded = {node_id: {"input:00000000": struct(producer = other, slot = "00000000")}} if mode == "direct_not_closed" else {}
        sets = struct(nodes = {"shared": {"entries": {0: struct(kind = "node", producer = other, slot = "00000000")}}}) if mode == "set_not_closed" else struct(nodes = {})
        linux_test_family_cut_dependencies(execution, decoded, sets)
    elif mode in ["missing_slot", "extra_slot", "wrong_tree", "duplicate_slot", "unknown_slot_node", "invalid_slot_path", "slot_bound"]:
        nodes = {node_id: {"outputs": {"00000000": struct(tree = "objects"), "00000001": struct(tree = "objects")}}}
        slots = ["00000000"] if mode == "missing_slot" else ["00000000", "00000001"]
        if mode == "extra_slot":
            slots.append("00000002")
        if mode == "duplicate_slot":
            slots.append("00000001")
        tree = "metadata" if mode == "wrong_tree" else "objects"
        children = [_fake_tree_child(linux_test_family_store_path(node_id, slot)) for slot in slots]
        if mode == "unknown_slot_node":
            children.append(_fake_tree_child(linux_test_family_store_path(other, "00000000")))
        elif mode == "invalid_slot_path":
            children.append(_fake_tree_child("nodes/" + node_id + "/0"))
        elif mode == "slot_bound":
            nodes[node_id]["outputs"] = {"00000000"[:8 - len(str(slot))] + str(slot): struct(tree = "objects") for slot in range(16385)}
        linux_test_family_pinned_outputs(execution, nodes, {"cut-" + tree: struct(children = children)})
    elif mode == "cut_views":
        fixture = _family_view_batch_fixture()
        inputs = dict(fixture.input_directories)
        inputs["execution"] = _family_execution_markers("cut", [fixture.node_id])
        fake = _fake_map_directory_context()
        expand_linux_family_plan(fake.template_ctx, inputs, fixture.output_directories, fixture.additional_inputs, fixture.tools, fixture.additional_params)
    elif mode in ["execution_source", "cut_store_source"]:
        fixture = _family_view_batch_fixture()
        namespace = "execution" if mode == "execution_source" else "cut-objects"
        marker_prefix = "sources/" + fixture.input_set_source + "/"
        inputs = dict(fixture.input_directories)
        inputs[namespace] = struct(children = [fixture.input_set_source_artifact])
        inputs["plan"] = struct(
            children = [_fake_tree_child(child.tree_relative_path.replace(
                marker_prefix + "kernel/",
                marker_prefix + namespace + "/",
            )) for child in fixture.input_directories["plan"].children],
            directory = fixture.input_directories["plan"].directory,
        )
        fake = _fake_map_directory_context()
        expand_linux_family_plan(fake.template_ctx, inputs, fixture.output_directories, fixture.additional_inputs, fixture.tools, fixture.additional_params)
    return []

_family_execution_failure_probe = rule(
    implementation = _family_execution_failure_probe_impl,
    attrs = {"mode": attr.string(mandatory = True)},
)

def _family_variant_output_names(owner, variants, initial):
    names = []
    for variant in variants:
        prefix = owner + "." + variant
        config_prefix = prefix + ".initial" if initial else prefix
        for suffix in [".arch", ".config", ".auto.conf", ".auto.conf.cmd", ".autoconf.h", ".rustc_cfg", ".kernel.release"]:
            names.append(config_prefix + suffix)
        names.append(prefix + (".action-plan.json.gz" if initial else ".observed-action-plan.json.gz"))
    return names

def _assert_exact_action_inputs(env, expected, action, description):
    actual = {file.path: True for file in action.inputs.to_list()}
    missing = sorted([path for path in expected if path not in actual])
    unexpected = sorted([path for path in actual if path not in expected])

    # Rust source closures can contain thousands of inputs. Preserve exact set
    # equality without printing the entire catalogue on a wiring regression.
    asserts.equals(env, 0, len(missing), "%s is missing inputs (first 8): %s" % (description, missing[:8]))
    asserts.equals(env, 0, len(unexpected), "%s has unexpected inputs (first 8): %s" % (description, unexpected[:8]))

def _assert_family_guard_input_flags(env, action, rounds, flags, description):
    for flag in flags:
        values = _flag_values(action.argv, flag)
        asserts.equals(env, [str(index) for index in range(len(rounds))], [value.split("=")[0] for value in values], description + " changed indexed input " + flag)
        for index, artifacts in enumerate(rounds):
            if index < len(values) and flag in artifacts:
                asserts.true(env, _named_action_path_names_artifact(values[index], str(index), artifacts[flag]), description + " changed round %s artifact for %s" % (index, flag))

def _assert_cpu_profile_not_in_provider(env, provider, profile, description):
    files = []
    for field in dir(provider):
        value = getattr(provider, field)
        values = value.values() if type(value) == "dict" else [value]
        for item in values:
            if type(item) == "File":
                files.append(item)
            elif type(item) == "depset":
                files.extend(item.to_list())
            elif type(item) == "FilesToRunProvider" and item.executable != None:
                files.append(item.executable)
    asserts.false(env, any([type(file) == "File" and file.path == profile.path for file in files]), description + " must not expose the diagnostic CPU profile")

def _assert_family_guard_profile(env, replay, guard_planners, owner, kind):
    actions = analysistest.target_actions(env)
    target = analysistest.target_under_test(env)
    mnemonic = "LinuxMappedCompilerGuardCPUProfile" if kind == "cpu" else "LinuxMappedCompilerGuardHeapProfile"
    group_name = "compiler_guard1_" + kind + "_profile"
    profiles = [action for action in actions if action.mnemonic == mnemonic]
    asserts.equals(env, 1, len(profiles), "the family must have exactly one opt-in diagnostic profile action")
    ordinary = [action for action in guard_planners if any([file.basename == owner + ".compiler-guards-1.json" for file in action.outputs.to_list()])]
    asserts.equals(env, 1, len(ordinary), "the profile must mirror ordinary guard round 1")
    if not profiles or not ordinary:
        return
    diagnostic = profiles[0]
    planner = ordinary[0]
    outputs = diagnostic.outputs.to_list()
    asserts.equals(env, [owner + ".compiler-guards-1." + kind + ".pprof"], [file.basename for file in outputs])
    if len(outputs) != 1:
        return
    profile = outputs[0]
    asserts.false(env, profile.is_directory, "the diagnostic publishes only its pprof file")
    prefix_size = 8 if kind == "cpu" else 10
    asserts.true(env, len(diagnostic.argv) >= prefix_size, "the supervisor must have its complete launcher prefix")
    if len(diagnostic.argv) < prefix_size:
        return
    asserts.equals(env, ["-planner", "-out", "-duration", "180s"], [diagnostic.argv[1], diagnostic.argv[3], diagnostic.argv[5], diagnostic.argv[6]])
    if kind == "cpu":
        asserts.equals(env, planner.argv[0], diagnostic.argv[2], "CPU capture retains the exact ordinary planner executable")
        asserts.equals(env, "--", diagnostic.argv[7])
    else:
        asserts.equals(env, ["-kind", "heap", "--"], diagnostic.argv[7:10])
        asserts.false(env, planner.argv[0] == diagnostic.argv[2], "heap capture must use the isolated heap-enabled planner")
    asserts.true(env, _action_path_names_artifact(diagnostic.argv[4], profile), "the supervisor must write only the declared profile")
    asserts.equals(env, planner.argv[1:], diagnostic.argv[prefix_size:], "the profiled invocation must exactly retain ordinary guard-round-1 argv")
    asserts.equals(env, planner.env, diagnostic.env, "profiling must not change the planner environment")

    # This ActionApi field is gated by experimental_google_legacy_api in
    # Bazel. Check it when exposed; do not require a new flag for this test.
    ordinary_requirements = getattr(planner, "execution_info", None)
    diagnostic_requirements = getattr(diagnostic, "execution_info", None)
    if ordinary_requirements != None and diagnostic_requirements != None:
        expected_requirements = dict(ordinary_requirements)
        expected_requirements["no-cache"] = "1"
        asserts.equals(env, expected_requirements, diagnostic_requirements)

    supervisor = [file for file in diagnostic.inputs.to_list() if _action_path_names_artifact(diagnostic.argv[0], file)]
    asserts.equals(env, 1, len(supervisor), "the diagnostic must declare its exact supervisor executable")
    expected_inputs = {file.path: True for file in planner.inputs.to_list()}
    if kind == "heap":
        ordinary_tools = [file for file in planner.inputs.to_list() if _action_path_names_artifact(planner.argv[0], file)]
        heap_tools = [file for file in diagnostic.inputs.to_list() if _action_path_names_artifact(diagnostic.argv[2], file)]
        asserts.equals(env, 1, len(ordinary_tools))
        asserts.equals(env, 1, len(heap_tools))
        for file in ordinary_tools:
            expected_inputs.pop(file.path, None)
            expected_inputs.pop(file.path + ".runfiles", None)
        for file in heap_tools:
            asserts.equals(env, "kconfig_parse_heap", file.basename)
            asserts.equals(env, "internal/cmd/kconfig_parse", file.owner.package)
            asserts.equals(env, "kconfig_parse_heap", file.owner.name)
            expected_inputs[file.path] = True
            for runfiles in diagnostic.inputs.to_list():
                if runfiles.path == file.path + ".runfiles":
                    asserts.equals(env, file.owner, runfiles.owner)
                    expected_inputs[runfiles.path] = True
    for file in supervisor:
        asserts.equals(env, "profilecapture", file.basename)
        expected_inputs[file.path] = True
        for runfiles in diagnostic.inputs.to_list():
            if runfiles.path == file.path + ".runfiles":
                # Bazel models this as RUNFILES, not TREE; File.is_directory
                # intentionally excludes it. Bind the exact path and owner.
                asserts.equals(env, file.owner, runfiles.owner, "the extra runfiles input must belong to the supervisor")
                expected_inputs[runfiles.path] = True
    _assert_exact_action_inputs(env, expected_inputs, diagnostic, "profile round 1 must retain exactly ordinary immutable/cut/round-0 inputs plus the declared diagnostic tool closures")
    diagnostic_inputs = {file.path: True for file in diagnostic.inputs.to_list()}
    forbidden = {file.path: True for file in replay.outputs.to_list()}
    for file in replay.inputs.to_list():
        if file.basename.startswith(owner + ".compiler-guards-1.") or file.basename.startswith(owner + ".compiler-guards-2."):
            forbidden[file.path] = True
    asserts.equals(env, [], sorted([path for path in forbidden if path in diagnostic_inputs]), "profiling must not demand normal round-1/round-2 outputs or final replay")
    for action in actions:
        asserts.false(env, profile.path in {file.path: True for file in action.inputs.to_list()}, action.mnemonic + " must not consume the optional diagnostic profile")

    asserts.true(env, LinuxMappedKernelFamilyInfo in target)
    if LinuxMappedKernelFamilyInfo in target:
        for variant, payload in target[LinuxMappedKernelFamilyInfo].variants.items():
            asserts.equals(env, [payload.kernel.image.path], [file.path for file in payload.default.files.to_list()], variant + " default outputs must remain only the kernel image")
            for runfiles in [payload.default.default_runfiles, payload.default.data_runfiles]:
                if runfiles != None:
                    asserts.false(env, profile.path in {file.path: True for file in runfiles.files.to_list()}, variant + " runfiles must not expose the diagnostic profile")
            for provider in [payload.kernel, payload.module_sdk, payload.module_tree]:
                _assert_cpu_profile_not_in_provider(env, provider, profile, variant + " " + type(provider))
            groups = payload.output_groups
            asserts.true(env, hasattr(groups, group_name), variant + " must expose the explicit diagnostic output group")
            for name in dir(groups):
                group = getattr(groups, name)
                if type(group) != "depset":
                    continue
                files = group.to_list()
                if name == group_name:
                    asserts.equals(env, [profile.path], [file.path for file in files])
                else:
                    asserts.false(env, profile.path in {file.path: True for file in files}, variant + " output group " + name + " must not demand the diagnostic profile")
    if OutputGroupInfo in target:
        asserts.true(env, hasattr(target[OutputGroupInfo], group_name))
        if hasattr(target[OutputGroupInfo], group_name):
            asserts.equals(env, [profile.path], [file.path for file in getattr(target[OutputGroupInfo], group_name).to_list()])
    asserts.equals(env, [target[LinuxKernelInfo].image.path], [file.path for file in target[DefaultInfo].files.to_list()], "the public default output must not demand profiling")

def _assert_family_observation_pipeline(env, initial, replay, guard_planners, owner, variants):
    initial_outputs = {file.basename: file for file in initial.outputs.to_list()}
    replay_outputs = {file.basename: file for file in replay.outputs.to_list()}
    initial_inputs = {file.path: True for file in initial.inputs.to_list()}
    segments = linux_test_family_execution_segments()
    trees = sorted([tree for segment in segments for tree in segment.output_trees])
    asserts.equals(env, 10, len(trees))
    asserts.equals(env, ["initial"], _flag_values(initial.argv, "-family_execution_mode"))
    asserts.equals(env, ["replay"], _flag_values(replay.argv, "-family_execution_mode"))
    for action in [initial, replay]:
        asserts.equals(env, 4, len(_flag_values(action.argv, "-family_execution_segment_out")))
        asserts.equals(env, [], _flag_values(action.argv, "-action_plan_family_segment_out"))
        asserts.equals(env, [], _flag_values(action.argv, "-action_plan_family_out"))
        asserts.equals(env, variants, _flag_values(action.argv, "-family_plan_variant"))
        _assert_manifest_derived_rust(env, action)
        _assert_selected_rust_sources_are_inputs(env, action)
    immutable_flags = [
        "-root",
        "-srctree",
        "-kbuild",
        "-resolve_config",
        "-config_mode",
        "-kernel_version",
        "-target_toolset_identity",
        "-host_toolset_identity",
        "-target_toolset_manifest",
        "-host_toolset_manifest",
        "-target_probe_results",
        "-host_probe_results",
        "-host_kconfig_probe_results",
        "-target_kconfig_probe_results",
        "-host_kbuild_probe_results",
        "-target_kbuild_probe_results",
        "-kbuild_target",
        "-kbuild_prepare_target",
        "-var",
        "-source_root_map",
        "-family_plan_overlay",
    ]
    for flag in immutable_flags:
        asserts.equals(env, _flag_values(initial.argv, flag), _flag_values(replay.argv, flag), "replay changed immutable invocation flag " + flag)

    expected_initial = _family_variant_output_names(owner, variants, True) + [owner + ".execution-cut.json", owner + ".cut-selection"]
    expected_replay = _family_variant_output_names(owner, variants, False) + [owner + ".pinned-selection", owner + ".observed-headers", owner + ".observed-artifacts", owner + ".reuse-report.json"]
    for segment in segments:
        expected_initial.append(owner + ".cut-plan-" + segment.name + "-v7")
        expected_replay.append(owner + ".family-plan-" + segment.name + "-v7")
    asserts.equals(env, sorted(expected_initial), sorted(initial_outputs.keys()))
    asserts.equals(env, sorted(expected_replay), sorted(replay_outputs.keys()))

    for action, flag, name, outputs in [
        (initial, "-family_execution_cut_out", owner + ".execution-cut.json", initial_outputs),
        (initial, "-family_execution_selection_out", owner + ".cut-selection", initial_outputs),
        (replay, "-family_execution_pinned_out", owner + ".pinned-selection", replay_outputs),
        (replay, "-family_execution_headers_out", owner + ".observed-headers", replay_outputs),
        (replay, "-family_execution_artifacts_out", owner + ".observed-artifacts", replay_outputs),
        (replay, "-family_execution_reuse_report_out", owner + ".reuse-report.json", replay_outputs),
    ]:
        values = _flag_values(action.argv, flag)
        asserts.equals(env, 1, len(values))
        asserts.true(env, name in outputs)
        if values and name in outputs:
            asserts.true(env, _action_path_names_artifact(values[0], outputs[name]))
    cut = initial_outputs.get(owner + ".execution-cut.json")
    cut_inputs = _flag_values(replay.argv, "-family_execution_cut_in")
    asserts.equals(env, 1, len(cut_inputs))
    if cut != None and cut_inputs:
        asserts.true(env, _action_path_names_artifact(cut_inputs[0], cut))

    snapshot_inputs = _flag_values(replay.argv, "-family_execution_initial_snapshot")
    asserts.equals(env, variants, sorted([value.split("=")[0] for value in snapshot_inputs]))
    additional = {cut.path: True} if cut != None else {}
    for value in snapshot_inputs:
        variant = value.split("=")[0]
        snapshot = initial_outputs.get(owner + "." + variant + ".action-plan.json.gz")
        asserts.true(env, snapshot != None)
        if snapshot != None:
            asserts.true(env, _named_action_path_names_artifact(value, variant, snapshot))
            additional[snapshot.path] = True
    store_values = _flag_values(replay.argv, "-family_execution_store")
    asserts.equals(env, trees, sorted([value.split("=")[0] for value in store_values]))
    store_inputs = {file.basename: file for file in replay.inputs.to_list() if file.basename.startswith(owner + ".cut.tree-store-")}
    asserts.equals(env, len(trees), len(store_inputs))
    for value in store_values:
        tree = value.split("=")[0]
        store = store_inputs.get(owner + ".cut.tree-store-" + tree)
        asserts.true(env, store != None)
        if store != None:
            asserts.true(env, store.is_directory, "observation input must be a completed TreeArtifact")
            asserts.true(env, _named_action_path_names_artifact(value, tree, store))
            additional[store.path] = True
    asserts.false(env, any([path in initial_inputs for path in additional]), "initial planning must not depend on execution outputs")

    guard_input_suffixes = {
        "-family_compiler_guard_manifest": ".json",
        "-family_compiler_guard_plan": ".plan",
        "-family_compiler_guard_host_results": ".results-host",
        "-family_compiler_guard_target_results": ".results-target",
    }
    guard_output_suffixes = {
        "-family_compiler_guard_manifest_out": ".json",
        "-family_compiler_guard_plan_out": ".plan",
    }
    for action in [initial, replay]:
        for flag in guard_output_suffixes:
            asserts.equals(env, [], _flag_values(action.argv, flag), "only guard discovery may publish " + flag)
    _assert_family_guard_input_flags(env, initial, [], guard_input_suffixes, "initial planning")
    asserts.equals(env, 3, len(guard_planners))
    replay_input_files = {file.basename: file for file in replay.inputs.to_list()}
    expected_inputs = dict(initial_inputs)
    expected_inputs.update(additional)
    guard_rounds = []
    for round_index in range(3):
        description = "guard round " + str(round_index)
        prefix = owner + ".compiler-guards-" + str(round_index)
        artifacts = {}
        for flag, suffix in guard_input_suffixes.items():
            artifact = replay_input_files.get(prefix + suffix)
            asserts.true(env, artifact != None, description + " is missing replay input " + suffix)
            if artifact != None:
                asserts.equals(env, suffix != ".json", artifact.is_directory, description + " input has wrong artifact kind: " + suffix)
                artifacts[flag] = artifact
        planners = [action for action in guard_planners if any([file.basename == prefix + ".json" for file in action.outputs.to_list()])]
        asserts.equals(env, 1, len(planners), description + " must have exactly one planner")
        if planners:
            planner = planners[0]
            outputs = {file.basename: file for file in planner.outputs.to_list()}
            asserts.equals(env, sorted([prefix + suffix for suffix in guard_output_suffixes.values()]), sorted(outputs.keys()), description + " must publish only a manifest and plan")
            asserts.equals(env, ["guards"], _flag_values(planner.argv, "-family_execution_mode"))
            asserts.equals(env, variants, _flag_values(planner.argv, "-family_plan_variant"))
            _assert_manifest_derived_rust(env, planner)
            _assert_selected_rust_sources_are_inputs(env, planner)
            for flag in immutable_flags:
                asserts.equals(env, _flag_values(initial.argv, flag), _flag_values(planner.argv, flag), description + " changed immutable invocation flag " + flag)
            for flag in ["-family_execution_cut_in", "-family_execution_initial_snapshot", "-family_execution_store"]:
                asserts.equals(env, _flag_values(replay.argv, flag), _flag_values(planner.argv, flag), description + " changed cut evidence flag " + flag)
            asserts.equals(env, sorted(guard_output_suffixes.keys()), sorted([arg for arg in planner.argv if arg.startswith("-") and arg.endswith("_out")]), description + " must not request initial or final planning outputs")
            for flag, suffix in guard_output_suffixes.items():
                values = _flag_values(planner.argv, flag)
                asserts.equals(env, 1, len(values), description + " must publish exactly one " + flag)
                artifact = outputs.get(prefix + suffix)
                if artifact != None and values:
                    asserts.true(env, _action_path_names_artifact(values[0], artifact), description + " changed output binding " + flag)
                    replay_artifact = artifacts.get(flag[:-4])
                    if replay_artifact != None:
                        asserts.equals(env, artifact.path, replay_artifact.path, description + " replay must consume this planner's output")
            _assert_family_guard_input_flags(env, planner, guard_rounds, guard_input_suffixes, description)
            _assert_exact_action_inputs(env, expected_inputs, planner, description + " must retain precisely immutable inputs, cut evidence, and preceding guard quartets")
        guard_rounds.append(artifacts)
        for artifact in artifacts.values():
            asserts.false(env, artifact.path in initial_inputs, "initial planning must not depend on compiler guard execution")
            expected_inputs[artifact.path] = True
    _assert_family_guard_input_flags(env, replay, guard_rounds, guard_input_suffixes, "final replay")
    _assert_exact_action_inputs(env, expected_inputs, replay, "replay must retain precisely immutable inputs, cut evidence, and all three guard quartets")
    _assert_family_guard_profile(env, replay, guard_planners, owner, "cpu")
    _assert_family_guard_profile(env, replay, guard_planners, owner, "heap")

    # Bazel excludes StarlarkMapActionTemplate from target.actions because it
    # is not an ActionApi value. The backend unit test exercises the real shared
    # registration helper with a recording factory; the actual fixture build
    # executes these templates. Planner/store coupling above uses real actions.

def _family_execution_registration_cases(env):
    calls = []
    declared = {}

    def declare_directory(name):
        if name in declared:
            fail("family registration repeats output directory " + name)
        artifact = struct(basename = name, is_directory = True, path = "bin/" + name, short_path = name)
        declared[name] = artifact
        return artifact

    def map_directory(**kwargs):
        calls.append(kwargs)

    owner = "registration"
    ctx = struct(label = struct(name = owner), actions = struct(declare_directory = declare_directory, map_directory = map_directory))
    segments = linux_test_family_execution_segments()
    variants = ["base", "irrelevant", "relevant"]
    views = {variant: {} for variant in variants}
    cut_stores = {}
    final_stores = {}
    base_inputs = {"prep": "empty-prep", "host_toolset_identity": "host-identity", "target_toolset_identity": "target-identity"}
    map_inputs = {"source_files": depset(["immutable-source"])}
    tools = {"runner": "recipe-runner"}
    params = {"source_prefix": "kernel"}
    requirements = {"supports-path-mapping": "1"}
    for initial in [True, False]:
        stores = cut_stores if initial else final_stores
        plans = {segment.name: ("cut-plan-" if initial else "final-plan-") + segment.name for segment in segments}
        selection = "cut-selection" if initial else "verified-pins"
        for segment in segments:
            expected_inputs = dict(base_inputs)
            expected_inputs.update({tree: stores[tree] for tree in segment.input_trees})
            expected_inputs.update({"plan": plans[segment.name], "execution": selection})
            if not initial:
                expected_inputs["observed-headers"] = "verified-headers"
                expected_inputs["observed-artifacts"] = "verified-artifacts"
                expected_inputs.update({"cut-" + tree: cut_stores[tree] for tree in segment.output_trees})
            linux_test_register_family_execution_segment(
                ctx,
                segment = segment,
                initial = initial,
                plans = plans,
                stores = stores,
                cut_stores = cut_stores,
                base_inputs = base_inputs,
                map_inputs = map_inputs,
                selection = selection,
                observed_headers = "verified-headers",
                observed_artifacts = "verified-artifacts",
                view_trees = views,
                tools = tools,
                params = params,
                requirements = requirements,
            )
            action = calls[-1]
            asserts.equals(env, expand_linux_family_plan, action["implementation"])
            asserts.equals(env, segment.mnemonic + ("Cut" if initial else ""), action["mnemonic"])
            asserts.equals(env, expected_inputs, action["input_directories"])
            asserts.equals(env, map_inputs, action["additional_inputs"])
            asserts.equals(env, tools, action["tools"])
            asserts.equals(env, requirements, action["execution_requirements"])
            asserts.equals(env, {}, action["env"])
            asserts.equals(env, segment.scope, action["additional_params"]["family_scope"])
            asserts.equals(env, ",".join(segment.stages), action["additional_params"]["family_stages"])
            asserts.equals(env, segment.emit_views and not initial, action["additional_params"]["family_emit_views"])
            asserts.equals(env, "kernel", action["additional_params"]["source_prefix"])
            if segment.scope == "host":
                asserts.equals(env, "host_cc", action.get("exec_group"))
                asserts.false(env, "toolchain" in action)
            else:
                asserts.equals(env, CC_TOOLCHAIN_TYPE, action.get("toolchain"))
                asserts.false(env, "exec_group" in action)
            prefix = owner + (".cut" if initial else "")
            expected_outputs = {"work": prefix + ".tree-work-" + segment.name}
            for tree in segment.output_trees:
                expected_outputs[tree] = prefix + ".tree-store-" + tree
                asserts.equals(env, stores[tree], action["output_directories"][tree])
            if segment.emit_views and not initial:
                for variant in variants:
                    for tree in ["objects", "sdk", "vmlinux", "image", "modules", "metadata"]:
                        expected_outputs["view@%s@%s" % (variant, tree)] = owner + "." + variant + ".tree-" + tree
                        asserts.equals(env, views[variant][tree], action["output_directories"]["view@%s@%s" % (variant, tree)])
            asserts.equals(env, expected_outputs, {key: artifact.basename for key, artifact in action["output_directories"].items()})
    asserts.equals(env, 8, len(calls))
    asserts.equals(env, 10, len(cut_stores))
    asserts.equals(env, sorted(cut_stores), sorted(final_stores))
    asserts.equals(env, {"source_prefix": "kernel"}, params, "registration must not mutate shared base params")

def _mapped_kernel_toolset_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = analysistest.target_actions(env)
    identity_actions = [action for action in actions if action.mnemonic == "LinuxToolsetIdentity"]
    probe_plan_actions = [action for action in actions if action.mnemonic == "LinuxProbePlan"]
    kconfig_probe_plan_actions = [action for action in actions if action.mnemonic == "LinuxKconfigProbePlan"]
    kbuild_probe_plan_actions = [action for action in actions if action.mnemonic == "LinuxKbuildProbePlan"]
    kbuild_probe_union_actions = [action for action in actions if action.mnemonic == "LinuxKbuildProbeUnion"]
    planner_actions = [action for action in actions if action.mnemonic == "LinuxMappedFamilySnapshotPlan"]
    family_plan_actions = [action for action in actions if action.mnemonic == "LinuxMappedFamilyPlan"]
    guard_plan_actions = [action for action in actions if action.mnemonic == "LinuxMappedCompilerGuardPlan"]

    prep_seed_actions = [action for action in actions if action.mnemonic == "LinuxMappedFamilyPrepSeed"]
    prep_base_actions = [action for action in actions if action.mnemonic == "LinuxMappedPrepBase"]
    asserts.equals(env, 2, len(identity_actions))
    asserts.equals(env, 1, len(probe_plan_actions))
    asserts.equals(env, 1, len(kconfig_probe_plan_actions))
    asserts.equals(env, 1, len(kbuild_probe_plan_actions))
    asserts.equals(env, 1, len(kbuild_probe_union_actions))
    asserts.equals(env, 1, len(planner_actions))
    asserts.equals(env, 1, len(family_plan_actions))
    asserts.equals(env, 1, len(prep_seed_actions))
    asserts.equals(env, 0, len(prep_base_actions))
    if prep_seed_actions:
        prep_seed_action = prep_seed_actions[0]
        asserts.equals(
            env,
            ["analysis_smoke.tree-prep-seed"],
            [output.basename for output in prep_seed_action.outputs.to_list()],
        )
        asserts.equals(env, 1, len(_flag_values(prep_seed_action.argv, "-tree_out")))
        asserts.equals(env, [], _flag_values(prep_seed_action.argv, "-copy"))
        asserts.false(
            env,
            any([input.basename == "analysis_smoke.prep-seed.marker" for input in prep_seed_action.inputs.to_list()]),
            "the empty prep seed must not be populated by a marker input",
        )
    asserts.true(env, OutputGroupInfo in target)
    asserts.true(env, LinuxKernelInfo in target)
    asserts.true(env, LinuxModuleSdkInfo in target)
    if OutputGroupInfo in target:
        asserts.equals(env, ["analysis_smoke.base.arch"], [file.basename for file in target[OutputGroupInfo].arch.to_list()])
        asserts.equals(env, 2, len(target[OutputGroupInfo].toolsets.to_list()))
        asserts.equals(env, [
            "analysis_smoke.family-plan-bootstrap-v7",
            "analysis_smoke.family-plan-host-v7",
            "analysis_smoke.family-plan-prehost-v7",
            "analysis_smoke.family-plan-target-v7",
        ], sorted([file.basename for file in target[OutputGroupInfo].plan.to_list()]))
        asserts.equals(env, [
            "analysis_smoke.kbuild-probe-plan",
            "analysis_smoke.kbuild-probe-results-host",
            "analysis_smoke.kbuild-probe-results-target",
            "analysis_smoke.kconfig-probe-plan",
            "analysis_smoke.kconfig-probe-results-host",
            "analysis_smoke.kconfig-probe-results-target",
            "analysis_smoke.probe-plan",
            "analysis_smoke.probe-results-host",
            "analysis_smoke.probe-results-target",
        ], sorted([file.basename for file in target[OutputGroupInfo].probes.to_list()]))
    if probe_plan_actions:
        probe_plan = probe_plan_actions[0]
        asserts.equals(env, 1, len([arg for arg in probe_plan.argv if arg == "-probe_plan_out"]))
        asserts.equals(env, 1, len([arg for arg in probe_plan.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in probe_plan.argv if arg == "-host_toolset_identity"]))
    if kconfig_probe_plan_actions:
        kconfig_probe_plan = kconfig_probe_plan_actions[0]
        _assert_manifest_derived_rust(env, kconfig_probe_plan)
        _assert_selected_rust_sources_are_inputs(env, kconfig_probe_plan)
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-kconfig_probe_plan_out"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-target_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-host_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-host_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-target_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in kconfig_probe_plan.argv if arg == "-host_toolset_manifest"]))
        asserts.equals(env, 0, len([value for value in _flag_values(kconfig_probe_plan.argv, "-var") if value.startswith("PYTHON3=")]))
        asserts.equals(env, {}, kconfig_probe_plan.env)
    if kbuild_probe_plan_actions:
        kbuild_probe_plan = kbuild_probe_plan_actions[0]
        _assert_manifest_derived_rust(env, kbuild_probe_plan)
        _assert_selected_rust_sources_are_inputs(env, kbuild_probe_plan)
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-kbuild_probe_plan_out"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-target_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in kbuild_probe_plan.argv if arg == "-host_toolset_manifest"]))
        asserts.equals(env, ["all"], _flag_values(kbuild_probe_plan.argv, "-kbuild_target"))
        asserts.equals(env, ["modules_prepare"], _flag_values(kbuild_probe_plan.argv, "-kbuild_prepare_target"))
        asserts.equals(env, 0, len([value for value in _flag_values(kbuild_probe_plan.argv, "-var") if value.startswith("PYTHON3=")]))
        asserts.equals(env, {}, kbuild_probe_plan.env)
    if kbuild_probe_union_actions:
        kbuild_probe_union = kbuild_probe_union_actions[0]
        asserts.equals(env, 1, len(_flag_values(kbuild_probe_union.argv, "-probe_plan_union_input")))
        asserts.equals(env, 1, len(_flag_values(kbuild_probe_union.argv, "-probe_plan_union_out")))
    if family_plan_actions:
        family_plan_action = family_plan_actions[0]
        asserts.equals(env, 4, len(_flag_values(family_plan_action.argv, "-family_execution_segment_out")))
        asserts.equals(env, 0, len(_flag_values(family_plan_action.argv, "-action_plan_family_out")))
        if planner_actions:
            _assert_family_observation_pipeline(env, planner_actions[0], family_plan_action, guard_plan_actions, "analysis_smoke", ["base"])
    if planner_actions:
        planner = planner_actions[0]
        _assert_manifest_derived_rust(env, planner)
        _assert_selected_rust_sources_are_inputs(env, planner)
        asserts.equals(env, ["base"], _flag_values(planner.argv, "-family_plan_variant"))
        asserts.equals(env, [], _flag_values(planner.argv, "-family_plan_overlay"))
        snapshot_outputs = _flag_values(planner.argv, "-family_plan_snapshot_out")
        asserts.equals(env, 1, len(snapshot_outputs))
        snapshots = [file for file in planner.outputs.to_list() if file.basename == "analysis_smoke.base.action-plan.json.gz"]
        asserts.equals(env, 1, len(snapshots))
        if snapshots:
            asserts.true(env, _named_action_path_names_artifact(snapshot_outputs[0], "base", snapshots[0]))
        asserts.equals(env, 1, len(_flag_values(planner.argv, "-family_plan_resolved_arch_out")))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_toolset_identity"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_toolset_manifest"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_kconfig_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-host_kbuild_probe_results"]))
        asserts.equals(env, 1, len([arg for arg in planner.argv if arg == "-target_kbuild_probe_results"]))
        asserts.equals(env, ["all"], _flag_values(planner.argv, "-kbuild_target"))
        asserts.equals(env, ["modules_prepare"], _flag_values(planner.argv, "-kbuild_prepare_target"))
        asserts.equals(env, 0, len([value for value in _flag_values(planner.argv, "-var") if value.startswith("PYTHON3=")]))
        asserts.equals(env, {}, planner.env)
        asserts.equals(env, 1, len([file for file in planner.outputs.to_list() if file.basename == "analysis_smoke.base.initial.arch"]))
        if OutputGroupInfo in target:
            asserts.equals(
                env,
                [
                    "analysis_smoke.family-plan-bootstrap-v7",
                    "analysis_smoke.family-plan-host-v7",
                    "analysis_smoke.family-plan-prehost-v7",
                    "analysis_smoke.family-plan-target-v7",
                ],
                sorted([file.basename for file in target[OutputGroupInfo].plan.to_list()]),
                "the plan output group must expose every exact family segment",
            )
        planner_inputs = planner.inputs.to_list()
        asserts.equals(
            env,
            2,
            len([file for file in planner_inputs if ".toolset-" in file.basename and not file.basename.endswith(".json")]),
        )
        asserts.equals(
            env,
            2,
            len([file for file in planner_inputs if ".toolset-" in file.basename and file.basename.endswith(".json")]),
        )
    if LinuxKernelInfo in target:
        kernel = target[LinuxKernelInfo]
        asserts.equals(env, "File", type(kernel.arch))
        asserts.equals(env, "analysis_smoke.base.arch", kernel.arch.basename)
    for action in identity_actions:
        asserts.equals(env, 1, len([arg for arg in action.argv if arg == "-manifest"]))
        asserts.equals(env, 1, len([arg for arg in action.argv if arg == "-out"]))
        asserts.true(env, len(action.inputs.to_list()) > 1, "identity action must depend on the complete toolchain closure")
    if LinuxModuleSdkInfo in target:
        sdk = target[LinuxModuleSdkInfo]
        for scope, kind, runner, closure in [
            ("host", "probe", sdk.host_probe_runner, sdk.host_toolchain_files),
            ("host", "recipe", sdk.host_recipe_runner, sdk.host_toolchain_files),
            ("target", "probe", sdk.target_probe_runner, sdk.target_toolchain_files),
            ("target", "recipe", sdk.target_recipe_runner, sdk.target_toolchain_files),
        ]:
            asserts.equals(
                env,
                "FilesToRunProvider",
                type(runner),
                "%s %s runner must retain the exact configured FilesToRunProvider" % (scope, kind),
            )
            asserts.true(env, runner.executable != None, "%s %s runner must be executable" % (scope, kind))
            if runner.executable != None:
                asserts.true(
                    env,
                    runner.executable.path in {file.path: True for file in closure.to_list()},
                    "%s %s runner must remain in the identity-bound toolset closure" % (scope, kind),
                )
        for scope, action_args, closure in [
            ("host", sdk.host_action_args, sdk.host_toolchain_files),
            ("target", sdk.target_action_args, sdk.target_toolchain_files),
        ]:
            for role in ["ar", "as", "cc", "cc-link", "cxx", "cxx-link", "ld", "nm", "objcopy", "objdump", "ranlib", "readelf", "strip"]:
                asserts.equals(
                    env,
                    1,
                    len([arg for arg in action_args[role] if arg == "__LINUX_BZL_KBUILD_ARGS_V1__"]),
                    "%s %s action must expose the insertion sentinel exactly once" % (scope, role),
                )
            for role in ["cc", "cxx"]:
                link_role = role + "-link"
                asserts.equals(
                    env,
                    sdk.host_tool_files[role] if scope == "host" else sdk.target_tool_files[role],
                    sdk.host_tool_files[link_role] if scope == "host" else sdk.target_tool_files[link_role],
                    "%s %s must retain the source-selected compiler executable" % (scope, link_role),
                )
                asserts.true(
                    env,
                    len(action_args[link_role]) > len(action_args[role]),
                    "%s %s must add the configured standard link envelope" % (scope, link_role),
                )
            closure_paths = {file.path: True for file in closure.to_list()}
            runtime_paths = {}
            for role in ["cc-link", "cxx-link"]:
                argv = action_args[role]
                markers = [index for index, arg in enumerate(argv) if arg == "__LINUX_BZL_KBUILD_ARGS_V1__"]
                runtime_paths[role] = sorted([
                    arg
                    for arg in argv[markers[0] + 1:]
                    if arg in closure_paths and arg.endswith(".a")
                ])
                asserts.true(
                    env,
                    len(runtime_paths[role]) > 0,
                    "%s %s must append configured runtime archives after source-selected inputs" % (scope, role),
                )
            asserts.equals(
                env,
                runtime_paths["cc-link"],
                runtime_paths["cxx-link"],
                "%s compiler-driver link roles must share configured runtime archives" % scope,
            )
            for path in runtime_paths["cc-link"]:
                asserts.false(
                    env,
                    path in action_args["cc"] or path in action_args["cxx"],
                    "%s runtime archive must remain link-only" % scope,
                )
        host_dependency_path = sdk.host_deps.path
        for role in ["cc", "cc-link", "cxx", "cxx-link"]:
            dependency_arguments = [
                arg
                for arg in sdk.host_action_args[role]
                if host_dependency_path in arg
            ]
            asserts.true(
                env,
                len(dependency_arguments) > 0,
                "host %s action must carry staged dependency compile defaults" % role,
            )
            asserts.true(
                env,
                any([arg.startswith("-isystem") for arg in dependency_arguments]),
                "host %s action must classify ordinary dependency roots as system headers" % role,
            )
            asserts.false(
                env,
                any([arg.startswith("-I") for arg in dependency_arguments]),
                "host %s action must not expose staged dependency headers to Linux warning policy" % role,
            )
            for argument in dependency_arguments:
                asserts.true(
                    env,
                    argument.startswith("-iquote") or argument.startswith("-isystem"),
                    "host %s dependency root must retain its CcInfo search-path class" % role,
                )
                rendered = linux_test_render_toolchain_action_value(argument, [sdk.host_deps])
                asserts.true(
                    env,
                    rendered.substituted and sdk.host_deps in rendered.fragments,
                    "host %s dependency flag must retain its typed TreeArtifact root" % role,
                )
        asserts.true(
            env,
            any([flag.startswith("-isystem") for flag in sdk.libelf_compile_flags]),
            "module SDK must classify ordinary dependency roots as system headers",
        )
        asserts.false(
            env,
            any([flag.startswith("-I") for flag in sdk.libelf_compile_flags]),
            "module SDK must not expose staged dependency headers to Linux warning policy",
        )
        for role in ["cc", "cc-link", "cxx", "cxx-link"]:
            asserts.false(
                env,
                any([host_dependency_path in arg for arg in sdk.target_action_args[role]]),
                "target %s action must not inherit host dependency compile defaults" % role,
            )
        for role in ["as", "cc", "cxx"]:
            host_args = sdk.host_action_args[role]
            asserts.true(
                env,
                len([arg for arg in host_args if arg == "-isystem"]) >= 2,
                "host %s action must carry toolchain-declared kernel and libc header roots" % role,
            )
            asserts.true(
                env,
                "-target" in sdk.target_action_args[role],
                "target %s action must retain the configured LLVM target" % role,
            )
        for scope, action_args, tool_files in [
            ("host", sdk.host_action_args, sdk.host_tool_files),
            ("target", sdk.target_action_args, sdk.target_tool_files),
        ]:
            for applet_role, basename in [
                ("script-applet-find", "toybox"),
                ("script-applet-perl", "perl"),
            ]:
                asserts.true(env, applet_role in tool_files)
                asserts.equals(env, basename, tool_files[applet_role].basename)
                asserts.equals(
                    env,
                    [],
                    action_args[applet_role],
                    "%s %s override must remain a runtime applet, not an action-role wrapper" % (scope, basename),
                )
        asserts.true(env, "actionfile" in sdk.host_tool_files)
        asserts.true(env, "awk" in sdk.host_tool_files)
        asserts.true(env, "bison" in sdk.host_tool_files)
        asserts.true(env, "flex" in sdk.host_tool_files)
        asserts.true(env, "m4" in sdk.host_tool_files)
        asserts.true(env, "pkg-config" in sdk.host_tool_files)
        asserts.equals(
            env,
            1,
            len([arg for arg in sdk.host_action_args["pkg-config"] if arg == "__LINUX_BZL_KBUILD_ARGS_V1__"]),
            "host pkg-config action must expose one source-argument insertion point",
        )
        pkg_config_contract = sdk.host_action_args["pkg-config"]
        asserts.equals(env, 4, len(pkg_config_contract))
        asserts.equals(env, "-manifest", pkg_config_contract[0])
        asserts.equals(env, "--", pkg_config_contract[2])
        manifest_path = pkg_config_contract[1]
        pkg_config_manifests = [
            file
            for file in sdk.host_toolchain_files.to_list()
            if file.path == manifest_path and file.basename == "analysis_smoke.pkg-config.json"
        ]
        asserts.equals(
            env,
            1,
            len(pkg_config_manifests),
            "host pkg-config manifest argument must name its identity-bound File",
        )
        asserts.false(env, "pkg-config" in sdk.target_tool_files)
        asserts.false(env, "m4-deny-shell" in sdk.host_tool_files)
        asserts.true(env, "script-runtime" in sdk.host_tool_files)
        asserts.true(env, "scriptrun" in sdk.host_tool_files)
        asserts.true(env, "actionfile" in sdk.target_tool_files)
        asserts.true(env, "awk" in sdk.target_tool_files)
        asserts.true(env, "lz4" in sdk.target_tool_files)
        asserts.true(env, "pahole" in sdk.target_tool_files)
        asserts.true(env, "python3" in sdk.target_tool_files)
        asserts.true(env, "script-runtime" in sdk.target_tool_files)
        asserts.true(env, "scriptrun" in sdk.target_tool_files)
        asserts.true(env, "rustc" in sdk.target_tool_files)
        asserts.true(env, "bindgen" in sdk.target_tool_files)
        asserts.true(env, "rustc" in sdk.host_tool_files)
        asserts.true(env, "python3" in sdk.host_tool_files)
        asserts.false(env, "lz4" in sdk.host_tool_files)
        asserts.false(env, "pahole" in sdk.host_tool_files)

        target_closure = {file.path: True for file in sdk.target_toolchain_files.to_list()}
        asserts.true(
            env,
            any([file.basename == "rustc" for file in sdk.target_toolchain_files.to_list()]),
            "target map toolchain closure must contain the selected rustc executable",
        )
        for role in ["awk", "lz4", "pahole"]:
            executable = getattr(sdk.target_tool_files.get(role), "executable", None)
            if executable == None:
                executable = sdk.target_tool_files.get(role)
            asserts.true(env, executable != None)
            if executable != None:
                asserts.true(
                    env,
                    executable.path in target_closure,
                    "%s must be part of the identity-bound target closure" % role,
                )

        host_closure = {file.path: True for file in sdk.host_toolchain_files.to_list()}
        target_closure = {file.path: True for file in sdk.target_toolchain_files.to_list()}
        asserts.true(env, len(sdk.host_toolset_anchors) > 0)
        asserts.true(env, len(sdk.target_toolset_anchors) > 0)
        for anchor in sdk.host_toolset_anchors.values():
            asserts.true(env, anchor.path in host_closure, "host root anchor must belong to its exact toolset closure")
        for anchor in sdk.target_toolset_anchors.values():
            asserts.true(env, anchor.path in target_closure, "target root anchor must belong to its exact toolset closure")
        m4 = sdk.host_tool_files.get("m4")
        m4_executable = getattr(m4, "executable", None)
        bison_companions = sdk.host_companion_tools.get("bison", [])
        flex_companions = sdk.host_companion_tools.get("flex", [])
        m4_companions = sdk.host_companion_tools.get("m4", [])
        asserts.true(env, m4_executable != None)
        asserts.equals(env, 1, len(bison_companions))
        asserts.equals(env, 2, len(flex_companions))
        asserts.equals(env, 1, len(m4_companions))
        for role, companions in [
            ("bison", bison_companions),
            ("flex", flex_companions),
            ("m4", m4_companions),
        ]:
            for companion in companions:
                asserts.equals(env, "FilesToRunProvider", type(companion), "%s companions must retain their typed runfiles provider" % role)
                asserts.true(env, companion.executable.path in host_closure, "%s companion executable must be identity-bound" % role)
        if m4_executable != None and len(bison_companions) == 1 and len(flex_companions) == 2 and len(m4_companions) == 1:
            bison_deny_shell = bison_companions[0]
            flex_deny_shell = flex_companions[0]
            m4_deny_shell = m4_companions[0]
            asserts.equals(env, m4, flex_companions[1])
            asserts.equals(env, bison_deny_shell.executable.path, sdk.host_action_environments["bison"].get("M4_SYSCMD_SHELL"))
            asserts.equals(env, m4_executable.path, sdk.host_action_environments["flex"].get("M4"))
            asserts.equals(env, flex_deny_shell.executable.path, sdk.host_action_environments["flex"].get("M4_SYSCMD_SHELL"))
            asserts.equals(env, m4_deny_shell.executable.path, sdk.host_action_environments["m4"].get("M4_SYSCMD_SHELL"))
            asserts.true(env, m4_executable.path in host_closure)
            mapped_host_tools = linux_map_directory_tools(
                "runner",
                "host",
                sdk.target_tool_files,
                sdk.target_toolchain_files,
                sdk.target_toolset_manifest,
                sdk.target_toolset_anchors,
                sdk.host_tool_files,
                sdk.host_toolchain_files,
                sdk.host_toolset_manifest,
                sdk.host_toolset_anchors,
                sdk.target_companion_tools,
                sdk.host_companion_tools,
            )
            asserts.equals(env, bison_companions, linux_test_companion_tool_bindings(mapped_host_tools, "bison", "host"))
            asserts.equals(env, flex_companions, linux_test_companion_tool_bindings(mapped_host_tools, "flex", "host"))
            asserts.equals(env, m4_companions, linux_test_companion_tool_bindings(mapped_host_tools, "m4", "host"))
        planner_input_paths = {
            file.path: True
            for file in planner_actions[0].inputs.to_list()
        } if planner_actions else {}
        kconfig_probe_plan_input_paths = {
            file.path: True
            for file in kconfig_probe_plan_actions[0].inputs.to_list()
        } if kconfig_probe_plan_actions else {}
        kbuild_probe_plan_input_paths = {
            file.path: True
            for file in kbuild_probe_plan_actions[0].inputs.to_list()
        } if kbuild_probe_plan_actions else {}
        for scope, tool_files, closure in [
            ("target", sdk.target_tool_files, target_closure),
            ("host", sdk.host_tool_files, host_closure),
        ]:
            for role in ["ar", "as", "awk", "cc", "cxx", "ld", "nm", "objcopy", "objdump", "ranlib", "readelf", "strip"]:
                executable = getattr(tool_files.get(role), "executable", None)
                if executable == None:
                    executable = tool_files.get(role)
                asserts.true(env, executable != None, "%s %s executable is selected" % (scope, role))
                if executable != None:
                    asserts.true(env, executable.path in closure, "%s %s is identity-bound" % (scope, role))
                    asserts.false(
                        env,
                        executable.path in planner_input_paths,
                        "final planner must not receive selected %s %s executable" % (scope, role),
                    )
                    asserts.false(
                        env,
                        executable.path in kconfig_probe_plan_input_paths,
                        "Kconfig probe discovery must not receive %s %s executable" % (scope, role),
                    )
                    asserts.false(
                        env,
                        executable.path in kbuild_probe_plan_input_paths,
                        "Kbuild probe discovery must not receive %s %s executable" % (scope, role),
                    )
            for role, tool in tool_files.items():
                executable = getattr(tool, "executable", None)
                if executable == None:
                    executable = tool
                asserts.false(
                    env,
                    executable.path in kconfig_probe_plan_input_paths,
                    "Kconfig probe discovery must not receive selected %s %s tool input" % (scope, role),
                )
                asserts.false(
                    env,
                    executable.path in kbuild_probe_plan_input_paths,
                    "Kbuild probe discovery must not receive selected %s %s tool input" % (scope, role),
                )
                asserts.false(
                    env,
                    executable.path in planner_input_paths,
                    "final planner must not receive selected %s %s tool input" % (scope, role),
                )
        for role in ["bindgen", "python3", "rustc"]:
            tool = sdk.target_tool_files.get(role)
            executable = getattr(tool, "executable", None)
            if executable == None:
                executable = tool
            asserts.true(env, executable != None, "target %s probe tool is selected" % role)
            if executable != None:
                asserts.false(
                    env,
                    executable.path in kconfig_probe_plan_input_paths,
                    "Kconfig discovery must not receive selected %s binary" % role,
                )
                asserts.false(
                    env,
                    executable.path in kbuild_probe_plan_input_paths,
                    "Kbuild discovery must not receive selected %s binary" % role,
                )
                asserts.false(
                    env,
                    executable.path in planner_input_paths,
                    "final planner must not receive selected %s binary" % role,
                )
                asserts.true(
                    env,
                    executable.path in target_closure,
                    "Kconfig map toolset closure must receive selected %s binary" % role,
                )
        for name in ["BISON_BAZEL_RUNFILES_M4", "BISON_PKGDATADIR", "M4"]:
            asserts.true(env, name in sdk.host_action_environments["bison"])
        asserts.false(env, "RUSTC_BOOTSTRAP" in sdk.target_action_environments["rustc"])
        asserts.false(env, "RUSTC_BOOTSTRAP" in sdk.host_action_environments["rustc"])
        for scope, rustc_args in [
            ("host", sdk.host_action_args["rustc"]),
            ("target", sdk.target_action_args["rustc"]),
        ]:
            asserts.equals(env, 4, len(rustc_args), "%s rustc action must preserve one configured default and the source-selected invocation boundary" % scope)
            asserts.true(env, rustc_args[0].startswith("__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot="))
            asserts.equals(env, "__LINUX_BZL_KBUILD_ARGS_V1__", rustc_args[1])
            asserts.equals(env, ["-Zunstable-options", "-Clink-self-contained=-linker"], rustc_args[2:])
        asserts.equals(
            env,
            sdk.target_action_args["rustc"],
            sdk.target_action_args["clippy"],
            "clippy must preserve the same external-linker contract as rustc",
        )
        asserts.true(env, "clippy" in sdk.target_tool_files)
        asserts.true(env, "rustdoc" in sdk.target_tool_files)
        asserts.equals(env, sdk.target_action_environments["rustc"], sdk.target_action_environments["clippy"])
        asserts.equals(env, {}, sdk.target_action_environments["rustdoc"])
        asserts.equals(env, {}, sdk.target_action_environments["bindgen"])
    return analysistest.end(env)

_mapped_kernel_toolset_test = analysistest.make(
    _mapped_kernel_toolset_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
    },
)

def mapped_kernel_toolset_test(name):
    _mapped_kernel_toolset_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:analysis_smoke",
    )

def _mapped_kernel_family_wiring_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    actions = analysistest.target_actions(env)
    kbuild_probe_plan_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxKbuildProbePlan"
    ]
    kbuild_probe_union_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxKbuildProbeUnion"
    ]
    family_snapshot_plan_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxMappedFamilySnapshotPlan"
    ]
    family_reduce_actions = [
        action
        for action in actions
        if action.mnemonic == "LinuxMappedFamilyPlan"
    ]
    guard_plan_actions = [action for action in actions if action.mnemonic == "LinuxMappedCompilerGuardPlan"]

    config_actions = [action for action in actions if action.mnemonic == "LinuxKconfigResolve"]
    asserts.equals(env, 3, len(config_actions), "each public configuration resolves before Kbuild planning")
    for variant in ["base", "irrelevant", "relevant"]:
        expected_name = "family_smoke." + variant + ".kconfig.config"
        matches = [action for action in config_actions if expected_name in [file.basename for file in action.outputs.to_list()]]
        asserts.equals(env, 1, len(matches), "missing early configuration for " + variant)
        if not matches:
            continue
        action = matches[0]
        output = [file for file in action.outputs.to_list() if file.basename == expected_name][0]
        values = _flag_values(action.argv, "-resolved_config_out")
        asserts.equals(env, 1, len(values))
        if values:
            asserts.true(env, _action_path_names_artifact(values[0], output))
        asserts.equals(env, 6, len(action.outputs.to_list()))
        for flag in ["-kbuild", "-kbuild_probe_plan_out", "-target_kbuild_probe_results", "-family_plan_variant", "-family_execution_mode"]:
            asserts.equals(env, [], _flag_values(action.argv, flag), "configuration must not request " + flag)
        overlays = _flag_values(action.argv, "-resolve_config_overlay")
        asserts.equals(env, 0 if variant == "base" else 1, len(overlays))
        if variant != "base" and overlays:
            overlay = [file for file in action.inputs.to_list() if file.basename == variant + ".config"]
            asserts.equals(env, 1, len(overlay))
            if overlay:
                asserts.true(env, _action_path_names_artifact(overlays[0], overlay[0]))
        for file in action.inputs.to_list():
            asserts.false(env, any([name in file.basename for name in ["compiler-guards", "execution-cut", "tree-store", "kbuild-probe", "action-plan"]]), "early config has a late planning dependency: " + file.basename)

    # The family fixture has base plus two overlays. Discovery remains
    # variant-specific, while initial cut execution and verified replay share
    # one complete family and the same immutable probe/source/tool inputs.
    asserts.equals(env, 3, len(kbuild_probe_plan_actions))
    asserts.equals(env, 1, len(kbuild_probe_union_actions))
    asserts.equals(env, 1, len(family_snapshot_plan_actions))
    asserts.equals(env, 1, len(family_reduce_actions))
    if kbuild_probe_union_actions:
        union_inputs = _flag_values(
            kbuild_probe_union_actions[0].argv,
            "-probe_plan_union_input",
        )
        asserts.equals(env, 3, len(union_inputs))
        asserts.equals(
            env,
            ["base", "irrelevant", "relevant"],
            sorted([value.split("=")[0] for value in union_inputs]),
        )
    if family_snapshot_plan_actions:
        family_plan = family_snapshot_plan_actions[0]
        asserts.equals(
            env,
            ["base", "irrelevant", "relevant"],
            _flag_values(family_plan.argv, "-family_plan_variant"),
        )
        overlay_values = _flag_values(family_plan.argv, "-family_plan_overlay")
        asserts.equals(env, ["irrelevant", "relevant"], sorted([value.split("=")[0] for value in overlay_values]))
        for flag in [
            "-family_plan_resolved_arch_out",
            "-family_plan_resolved_config_out",
            "-family_plan_resolved_auto_conf_out",
            "-family_plan_resolved_auto_conf_cmd_out",
            "-family_plan_resolved_autoconf_out",
            "-family_plan_resolved_rustc_cfg_out",
            "-family_plan_resolved_kernel_release_out",
            "-family_plan_snapshot_out",
        ]:
            asserts.equals(env, 3, len(_flag_values(family_plan.argv, flag)))
        family_plan_inputs = {file.basename: True for file in family_plan.inputs.to_list()}
        asserts.true(env, "irrelevant.config" in family_plan_inputs)
        asserts.true(env, "relevant.config" in family_plan_inputs)
    if family_reduce_actions and family_snapshot_plan_actions:
        _assert_family_observation_pipeline(
            env,
            family_snapshot_plan_actions[0],
            family_reduce_actions[0],
            guard_plan_actions,
            "family_smoke",
            ["base", "irrelevant", "relevant"],
        )
    asserts.true(env, OutputGroupInfo in target)
    if OutputGroupInfo in target:
        asserts.equals(env, ["family_smoke.base.kconfig.config"], [file.basename for file in target[OutputGroupInfo].config.to_list()])
        asserts.equals(env, [
            "family_smoke.base.action-plan.json.gz",
            "family_smoke.irrelevant.action-plan.json.gz",
            "family_smoke.relevant.action-plan.json.gz",
        ], sorted([file.basename for file in target[OutputGroupInfo].plan_snapshots.to_list()]), "snapshot diagnostics must remain initial artifacts with no cut-execution dependency")
    return analysistest.end(env)

_mapped_kernel_family_wiring_test = analysistest.make(
    _mapped_kernel_family_wiring_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
    },
)

def mapped_kernel_family_wiring_test(name):
    _mapped_kernel_family_wiring_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:family_smoke",
    )

def _mapped_kernel_public_config_projection_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    asserts.equals(
        env,
        ["family_smoke.relevant.kconfig.config"],
        [file.basename for file in target[DefaultInfo].files.to_list()],
        "the generated public config projection must select early Kconfig, not the execution provider file",
    )
    asserts.equals(env, [], analysistest.target_actions(env), "config projection must not create another action")
    return analysistest.end(env)

_mapped_kernel_public_config_projection_test = analysistest.make(
    _mapped_kernel_public_config_projection_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
    },
)

def mapped_kernel_public_config_projection_test(name):
    _mapped_kernel_public_config_projection_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:family_smoke_relevant_config",
    )

def _mapped_kernel_variant_provider_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    for provider in [LinuxKernelInfo, LinuxModuleSdkInfo, LinuxModuleTreeInfo, OutputGroupInfo]:
        asserts.true(env, provider in target)
    asserts.equals(env, ["family_smoke.relevant.image"], [file.basename for file in target[DefaultInfo].files.to_list()])
    if LinuxKernelInfo in target:
        kernel = target[LinuxKernelInfo]
        asserts.equals(env, "family_smoke.relevant.config", kernel.config.basename)
        asserts.equals(env, "family_smoke.relevant.image", kernel.image.basename)
    if LinuxModuleSdkInfo in target:
        sdk = target[LinuxModuleSdkInfo]
        asserts.true(
            env,
            sdk.kernel_key.startswith(str(Label("//internal/tests/mapped_kernel:family_smoke")) + "#relevant#"),
            "family SDK key must include configured artifact/toolset identity",
        )
        asserts.equals(env, "family_smoke.relevant.config", sdk.config.basename)
    if OutputGroupInfo in target:
        groups = target[OutputGroupInfo]
        asserts.equals(env, [
            "family_smoke.family-plan-bootstrap-v7",
            "family_smoke.family-plan-host-v7",
            "family_smoke.family-plan-prehost-v7",
            "family_smoke.family-plan-target-v7",
        ], sorted([file.basename for file in groups.plan.to_list()]))
        asserts.equals(env, ["family_smoke.reuse-report.json"], [file.basename for file in groups.reuse_report.to_list()])
        asserts.equals(env, [
            "family_smoke.base.action-plan.json.gz",
            "family_smoke.irrelevant.action-plan.json.gz",
            "family_smoke.relevant.action-plan.json.gz",
        ], sorted([file.basename for file in groups.plan_snapshots.to_list()]))
    return analysistest.end(env)

_mapped_kernel_variant_provider_test = analysistest.make(
    _mapped_kernel_variant_provider_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
    },
)

def mapped_kernel_variant_provider_test(name):
    _mapped_kernel_variant_provider_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:family_smoke_relevant",
    )

def _mapped_kernel_variant_module_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    asserts.true(env, LinuxModuleInfo in target)
    if LinuxModuleInfo in target:
        asserts.true(
            env,
            target[LinuxModuleInfo].kernel_key.startswith(str(Label("//internal/tests/mapped_kernel:family_smoke")) + "#relevant#"),
            "external module must retain the configured family SDK key",
        )
    planner_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxExternalModulePlan"
    ]
    asserts.equals(env, 1, len(planner_actions))
    return analysistest.end(env)

_mapped_kernel_variant_module_test = analysistest.make(
    _mapped_kernel_variant_module_test_impl,
    config_settings = {
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_x86_64")),
    },
)

def mapped_kernel_variant_module_test(name):
    _mapped_kernel_variant_module_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:family_smoke_overlay_module",
    )

def _mapped_kernel_exec_python_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    sdk = target[LinuxModuleSdkInfo]
    rustc = sdk.target_tool_files.get("rustc")
    host_rustc = sdk.host_tool_files.get("rustc")
    asserts.true(env, rustc != None and host_rustc != None)
    for scope, selected_rustc, closure in [
        ("target", rustc, sdk.target_toolchain_files),
        ("host", host_rustc, sdk.host_toolchain_files),
    ]:
        files = closure.to_list()
        if selected_rustc != None:
            asserts.true(
                env,
                selected_rustc in files,
                "%s Rust closure must retain the exact compiler artifact selected for that execution group" % scope,
            )
        anchors = [file for file in files if file.basename == "rust.sysroot"]
        asserts.equals(env, 1, len(anchors), "%s Rust closure must retain its typed sysroot anchor" % scope)
        if anchors:
            default_argument = sdk.target_action_args["rustc"][0] if scope == "target" else sdk.host_action_args["rustc"][0]
            asserts.equals(
                env,
                "__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot=" + anchors[0].path,
                default_argument,
                "%s rustc contract must bind the execution sysroot through its exact anchor" % scope,
            )
            rendered = linux_test_render_toolchain_action_value(default_argument, [anchors[0]])
            asserts.true(env, rendered.substituted and anchors[0] in rendered.fragments)
        rustlib_paths = [file.path for file in files if "/lib/rustlib/" in file.path]
        asserts.true(
            env,
            any(["/lib/rustlib/x86_64-unknown-linux-gnu/lib/libstd-" in path for path in rustlib_paths]),
            "%s Rust closure must contain execution-platform libstd" % scope,
        )
        asserts.true(
            env,
            any(["/lib/rustlib/x86_64-unknown-linux-gnu/lib/libproc_macro-" in path for path in rustlib_paths]),
            "%s Rust closure must contain execution-platform proc_macro" % scope,
        )
        asserts.false(
            env,
            any(["/lib/rustlib/aarch64-unknown-linux-gnu/lib/libstd-" in path for path in rustlib_paths]),
            "%s Rust closure must not select target-platform prebuilt std" % scope,
        )
    for scope, tools, closure in [
        ("target", sdk.target_tool_files, sdk.target_toolchain_files),
        ("host", sdk.host_tool_files, sdk.host_toolchain_files),
    ]:
        interpreter = tools.get("python3")
        asserts.true(env, interpreter != None, "%s toolset must select Python" % scope)
        if interpreter != None:
            asserts.equals(env, _EXEC_PYTHON_INTERPRETER, interpreter.basename)
            asserts.true(
                env,
                interpreter.path in {file.path: True for file in closure.to_list()},
                "%s execution Python must be identity-bound" % scope,
            )

    for mnemonic in ["LinuxKconfigProbePlan", "LinuxKbuildProbePlan", "LinuxMappedFamilySnapshotPlan", "LinuxMappedFamilyPlan"]:
        actions = [action for action in analysistest.target_actions(env) if action.mnemonic == mnemonic]
        asserts.equals(env, 1, len(actions))
        if actions:
            python_vars = [value for value in _flag_values(actions[0].argv, "-var") if value.startswith("PYTHON3=")]
            asserts.equals(
                env,
                0,
                len(python_vars),
                "%s must derive Python only from the identity-bound toolset manifest" % mnemonic,
            )
    identity_actions = [
        action
        for action in analysistest.target_actions(env)
        if action.mnemonic == "LinuxToolsetIdentity"
    ]
    asserts.equals(env, 2, len(identity_actions))
    asserts.equals(
        env,
        2,
        len([
            action
            for action in identity_actions
            if _EXEC_PYTHON_INTERPRETER in [file.basename for file in action.inputs.to_list()]
        ]),
        "both execution scopes must identity-bind the execution-platform Python closure",
    )
    return analysistest.end(env)

_mapped_kernel_exec_python_test = analysistest.make(
    _mapped_kernel_exec_python_test_impl,
    config_settings = {
        "//command_line_option:extra_toolchains": [
            str(Label("//internal/tests:mapped_kernel_exec_python_test_toolchain")),
        ],
        "//command_line_option:platforms": str(Label("@llvm//platforms:linux_arm64")),
    },
)

def mapped_kernel_exec_python_test(name):
    interpreter = name + "_interpreter"
    implementation = name + "_implementation"
    toolchain = name + "_toolchain"
    _exec_python_interpreter(
        name = interpreter,
        tags = ["manual"],
        target_compatible_with = [
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
    )
    _exec_python_toolchain(
        name = implementation,
        interpreter = ":" + interpreter,
        tags = ["manual"],
    )
    native.toolchain(
        name = toolchain,
        exec_compatible_with = [
            "@platforms//cpu:x86_64",
            "@platforms//os:linux",
        ],
        toolchain = ":" + implementation,
        toolchain_type = _PYTHON_EXEC_TOOLS_TOOLCHAIN_TYPE,
    )
    _mapped_kernel_exec_python_test(
        name = name,
        target_under_test = "//internal/tests/mapped_kernel:analysis_smoke",
    )

def _mapped_kernel_backend_test_impl(ctx):
    env = unittest.begin(ctx)
    _family_source_aggregate_cases(env)
    _family_execution_success_cases(env)
    _family_execution_registration_cases(env)
    family_store_path = linux_test_family_store_path("a" * 64, "00000000")
    asserts.true(env, family_store_path.startswith("nodes/"))
    family_segments = linux_test_family_execution_segments()
    asserts.equals(env, ["prehost", "bootstrap", "host", "target"], [segment.name for segment in family_segments])
    asserts.equals(env, ["host", "target", "host", "target"], [segment.scope for segment in family_segments])
    asserts.equals(env, [
        ["prehost"],
        ["bootstrap"],
        ["host"],
        ["prep", "target"],
    ], [segment.stages for segment in family_segments])
    asserts.equals(env, [False, False, False, True], [segment.emit_views for segment in family_segments])
    asserts.equals(env, [
        "LinuxMappedFamilyPrehost",
        "LinuxMappedFamilyBootstrap",
        "LinuxMappedFamilyHost",
        "LinuxMappedFamilyTarget",
    ], [segment.mnemonic for segment in family_segments])

    # A facade copy is one expanded action per non-empty (variant, tree), not
    # one action per projected leaf. This fixture has two variants, three
    # non-empty trees each, and ten leaves in total.
    fixture = _family_view_batch_fixture()
    fake_context = _fake_map_directory_context()
    expand_linux_family_plan(
        fake_context.template_ctx,
        fixture.input_directories,
        fixture.output_directories,
        fixture.additional_inputs,
        fixture.tools,
        fixture.additional_params,
    )
    producer_actions = [
        action
        for action in fake_context.actions
        if action["progress_message"].startswith("Building Linux ")
    ]
    asserts.equals(env, 1, len(producer_actions))
    if producer_actions:
        asserts.equals(env, 1, len(producer_actions[0]["arguments"]))
        asserts.equals(env, {
            "argument": None,
            "format": None,
            "use_always": False,
        }, producer_actions[0]["arguments"][0].param_file)
        producer_args = producer_actions[0]["arguments"][0].values
        asserts.equals(env, [fixture.input_set], _flag_values(producer_args, "-input_set_root"))
        asserts.equals(env, 1, len(_flag_values(producer_args, "-input_set_manifest_root")))
        asserts.equals(env, 0, len(_flag_values(producer_args, "-input_set_manifest")))
        asserts.equals(env, 1, len(_flag_values(producer_args, "-input_set_source")))
        asserts.equals(env, 0, len(_flag_values(producer_args, "-input_set_input")))
        asserts.true(
            env,
            fixture.input_set_source_artifact in producer_actions[0]["inputs"].to_list(),
            "family actions must declare exact persistent-set sources through the shared depset",
        )
    shared_image_outputs = []
    if producer_actions:
        shared_image_outputs = [
            output
            for output in producer_actions[0]["outputs"]
            if output.parent == fixture.output_directories["image"] and
               output.name == linux_test_family_store_path(fixture.node_id, "00000002")
        ]
    asserts.equals(env, 1, len(shared_image_outputs))

    facade_actions = [
        action
        for action in fake_context.actions
        if action["progress_message"].startswith("Projecting Linux ")
    ]
    facade_outputs = []
    for action in facade_actions:
        facade_outputs.extend(action["outputs"])
    asserts.equals(env, 6, len(facade_actions))
    asserts.equals(env, 10, len(facade_outputs))
    asserts.equals(env, {
        "Projecting Linux base image view %{label}": 1,
        "Projecting Linux base metadata view %{label}": 2,
        "Projecting Linux base objects view %{label}": 2,
        "Projecting Linux debug image view %{label}": 1,
        "Projecting Linux debug metadata view %{label}": 2,
        "Projecting Linux debug objects view %{label}": 2,
    }, {
        action["progress_message"]: len(action["outputs"])
        for action in facade_actions
    })
    for action in facade_actions:
        output_count = len(action["outputs"])
        asserts.equals(env, 2 * output_count, len(action["inputs"]))
        asserts.equals(
            env,
            [output_count],
            _flag_values(action["arguments"][0].values, "-family_view_expected_count"),
        )
    if shared_image_outputs:
        shared_image_output = shared_image_outputs[0]
        for variant in ["base", "debug"]:
            image_actions = [
                action
                for action in facade_actions
                if action["progress_message"] == "Projecting Linux %s image view %%{label}" % variant
            ]
            asserts.equals(env, 1, len(image_actions))
            if image_actions:
                asserts.equals(env, [shared_image_output], [
                    input
                    for input in image_actions[0]["inputs"]
                    if getattr(input, "kind", None) == "file"
                ])
    prior_host_output = struct(tree_relative_path = "nodes/%s/00000000" % ("a" * 64))
    prior_bootstrap_output = struct(tree_relative_path = "nodes/%s/00000001" % ("b" * 64))
    asserts.equals(env, {
        "bootstrap:" + prior_bootstrap_output.tree_relative_path: prior_bootstrap_output,
        "host:" + prior_host_output.tree_relative_path: prior_host_output,
    }, linux_test_family_segment_prior_outputs({
        "bootstrap": struct(children = [prior_bootstrap_output]),
        "host": struct(children = [prior_host_output]),
        "plan": struct(children = []),
    }))
    family_node_index = struct(node_ids = ["a" * 64, "b" * 64], ordinals = {"a" * 64: 0, "b" * 64: 1})
    family_current_node = {
        "input_packs": {"helper": ["00000000.0.1.1"]},
    }
    asserts.equals(env, {
        "bootstrap:" + prior_bootstrap_output.tree_relative_path: prior_bootstrap_output,
    }, linux_test_selected_family_prior_outputs(
        {
            "bootstrap": struct(children = [prior_bootstrap_output]),
            "host": struct(children = [prior_host_output]),
        },
        {"a" * 64: family_current_node},
        family_node_index,
        {"b" * 64 + ":00000001": struct(artifact_path = "ignored-logical-path", tree = "bootstrap")},
    ))
    prior_tree = struct(directory = "prior-prehost-tree")
    prior_tree_input = linux_test_family_tree_input(
        "prehost",
        {"prehost": prior_tree},
        {},
    )
    asserts.equals(env, "prior-prehost-tree", prior_tree_input.path)
    asserts.equals(env, "-private_input_tree", prior_tree_input.projection_argument)
    current_tree_input = linux_test_family_tree_input(
        "prep",
        {"prep": struct(directory = "prior-prep-seed")},
        {"prep": "current-prep-store"},
    )
    asserts.equals(env, "current-prep-store", current_tree_input.path)
    asserts.equals(env, "-private_input_tree", current_tree_input.projection_argument)
    empty_toolset = struct(
        arguments = {},
        environments = {},
        make_variables = {},
        requirements_by_role = {},
        tools = {},
    )
    rust_environment = {"RUST_TOOLCHAIN_ENV": "selected"}
    rust_tools = linux_test_with_rust_toolchain(
        "target",
        empty_toolset,
        struct(
            clippy_driver = "selected-clippy",
            env = rust_environment,
            rust_doc = "selected-rustdoc",
            rustc = "selected-rustc",
            rustfmt = "selected-rustfmt",
            sysroot_anchor = struct(path = "bazel-out/exec/bin/rust-toolchain/rust.sysroot"),
        ),
        struct(bindgen = "selected-bindgen"),
    )
    asserts.equals(env, {
        "BINDGEN": "bindgen",
        "CLIPPY_DRIVER": "clippy",
        "RUSTC": "rustc",
        "RUSTDOC": "rustdoc",
        "RUSTFMT": "rustfmt",
    }, rust_tools.make_variables)
    asserts.false(env, "RUSTC_OR_CLIPPY" in rust_tools.make_variables)
    asserts.equals(env, rust_environment, rust_tools.environments["rustc"])
    asserts.equals(env, rust_environment, rust_tools.environments["clippy"])
    asserts.equals(env, {}, rust_tools.environments["rustdoc"])
    asserts.equals(env, {}, rust_tools.environments["rustfmt"])
    asserts.equals(env, {}, rust_tools.environments["bindgen"])
    asserts.equals(env, [
        "__LINUX_BZL_DEFAULT_DIRECTORY_ARGUMENT_V1__--sysroot=bazel-out/exec/bin/rust-toolchain/rust.sysroot",
        "__LINUX_BZL_KBUILD_ARGS_V1__",
        "-Zunstable-options",
        "-Clink-self-contained=-linker",
    ], rust_tools.arguments["rustc"])
    asserts.equals(env, rust_tools.arguments["rustc"], rust_tools.arguments["clippy"])
    for role in ["bindgen", "rustdoc", "rustfmt"]:
        asserts.equals(env, [], rust_tools.arguments[role])
    ambient_sysroot_tools = linux_test_with_rust_toolchain(
        "host",
        empty_toolset,
        struct(
            _toolchain_generated_sysroot = False,
            env = {},
            rustc = "selected-rustc",
            sysroot_anchor = struct(path = "bazel-out/exec/bin/rust-toolchain/rust.sysroot"),
        ),
    )
    asserts.equals(env, [
        "__LINUX_BZL_KBUILD_ARGS_V1__",
        "-Zunstable-options",
        "-Clink-self-contained=-linker",
    ], ambient_sysroot_tools.arguments["rustc"])
    bindgen_only = linux_test_with_rust_toolchain(
        "target",
        empty_toolset,
        None,
        struct(bindgen = "selected-bindgen"),
    )
    asserts.equals(env, {"BINDGEN": "bindgen"}, bindgen_only.make_variables)
    asserts.equals(env, {"bindgen": {}}, bindgen_only.environments)
    asserts.equals(
        env,
        "external/rust-src/lib/rustlib/src/library",
        linux_test_canonical_rust_source_root(
            "../rust-src",
            "lib/rustlib/src",
            "library",
        ),
    )
    asserts.equals(env, "linux-kbuild-host-cc", linux_test_kbuild_action_name("host", "cc"))
    asserts.equals(env, "linux-kbuild-target-cc", linux_test_kbuild_action_name("target", "cc"))
    indexed_cc = struct(name = "cc", path = "toolchain/bin/cc")
    asserts.equals(env, indexed_cc, linux_test_tool_file(
        [
            struct(name = "ld", path = "toolchain/bin/ld"),
            indexed_cc,
        ],
        indexed_cc.path,
        "cc",
    ))
    asserts.equals(env, [
        "link-prefix",
        "compile-prefix",
        "__LINUX_BZL_KBUILD_ARGS_V1__",
        "compile-suffix",
        "link-suffix",
    ], linux_test_merge_action_arguments(
        ["link-prefix", "__LINUX_BZL_KBUILD_ARGS_V1__", "link-suffix"],
        ["compile-prefix", "__LINUX_BZL_KBUILD_ARGS_V1__", "compile-suffix"],
    ))
    asserts.equals(env, "cc-link", linux_test_node_action_contract_role("link-driver", "cc"))
    asserts.equals(env, "cc", linux_test_node_action_contract_role("compile", "cc"))
    asserts.equals(env, "ld", linux_test_node_action_contract_role("link-driver", "ld"))
    asserts.equals(env, {
        "cc": "configured-cc",
        "ld": "configured-ld",
    }, linux_test_runtime_tool_bindings({
        "cc": struct(executable = "configured-cc"),
        "ld": "configured-ld",
        "runner": "recipe-runner",
        "toolchain_files": depset(),
    }))
    execution_root_marker = linux_test_execution_root_marker()
    sysroot = struct(
        is_directory = True,
        is_source = False,
        path = "bazel-out/k8-opt-exec/bin/external/toolchain/sysroot",
        short_path = "../toolchain/sysroot",
    )
    header = struct(
        is_directory = False,
        is_source = False,
        path = "bazel-out/k8-opt-exec/bin/external/toolchain/tool.h",
        short_path = "../toolchain/tool.h",
    )
    rendered_action_value = linux_test_render_toolchain_action_value(
        "-Iexternal/toolchain/sysroot/include:external/toolchain/tool.h;external/unrelated/tool.h-not-an-artifact",
        [sysroot, header],
    )
    asserts.true(env, rendered_action_value.substituted)
    asserts.equals(env, [
        "-I",
        execution_root_marker + "/",
        sysroot,
        "/include:",
        execution_root_marker + "/",
        header,
        ";external/unrelated/tool.h-not-an-artifact",
    ], rendered_action_value.fragments)
    asserts.equals(
        env,
        "-Iexternal/toolchain/sysroot/include:external/toolchain/tool.h;external/unrelated/tool.h-not-an-artifact",
        linux_test_canonicalize_toolchain_action_value(
            "-Ibazel-out/k8-opt-exec/bin/external/toolchain/sysroot/include:bazel-out/k8-opt-exec/bin/external/toolchain/tool.h;external/unrelated/tool.h-not-an-artifact",
            [sysroot, header],
        ),
    )

    # Prebuilt repositories can expose an opaque source directory as a source
    # File even though Starlark does not mark it as a directory. Its exact
    # artifact still anchors descendants such as Clang's builtin includes.
    opaque_resource_root = struct(
        is_directory = False,
        is_source = True,
        path = "external/toolchain/lib/clang/22",
        short_path = "../toolchain/lib/clang/22",
    )
    rendered_opaque_resource = linux_test_render_toolchain_action_value(
        opaque_resource_root.path + "/include",
        [opaque_resource_root],
    )
    asserts.equals(env, [
        execution_root_marker + "/",
        opaque_resource_root,
        "/include",
    ], rendered_opaque_resource.fragments)

    # Runfiles trees are mapped from their exact executable. Keep the File
    # typed through Args and leave only the Bazel-defined suffix textual.
    bison = struct(
        is_directory = False,
        is_source = False,
        path = "bazel-out/k8-opt-exec/bin/external/bison_repo/bin/bison",
        short_path = "../bison_repo/bin/bison",
    )
    rendered_runfiles_directory = linux_test_render_toolchain_action_value(
        bison.path + ".runfiles/bison_repo/data",
        [bison],
    )
    asserts.equals(env, [
        execution_root_marker + "/",
        bison,
        ".runfiles/bison_repo/data",
    ], rendered_runfiles_directory.fragments)

    # Clang's cc-link action passes its resource directory after a pair of
    # -Xclang options.  The directory is not itself a File, but the selected
    # toolchain closure proves it through the resource headers below it.
    resource_directory = "external/toolchain/lib/clang/22/include"
    resource_header = struct(
        is_directory = False,
        is_source = True,
        path = resource_directory + "/stddef.h",
        short_path = "../toolchain/lib/clang/22/include/stddef.h",
    )
    rendered_cc_link_resource = linux_test_render_toolchain_action_value(
        "-Xclang -internal-isystem -Xclang " + resource_directory,
        [resource_header],
    )
    asserts.equals(env, [
        "-Xclang -internal-isystem -Xclang ",
        execution_root_marker + "/",
        resource_directory,
    ], rendered_cc_link_resource.fragments)

    # Directory discovery is syntax-independent: attached options and
    # delimiter-separated path values use the same closure-derived index.
    rendered_source_directories = linux_test_render_toolchain_action_value(
        "-I%s:--resource-dir=%s;%s-not-an-artifact" % (
            resource_directory,
            resource_directory,
            resource_directory,
        ),
        [resource_header],
    )
    asserts.equals(env, [
        "-I",
        execution_root_marker + "/",
        resource_directory,
        ":--resource-dir=",
        execution_root_marker + "/",
        resource_directory,
        ";" + resource_directory + "-not-an-artifact",
    ], rendered_source_directories.fragments)

    # A rejected substring is not exhaustion: the next occurrence can have
    # valid boundaries. Repeated matches must retain their intervening text.
    rejected_prefix = "x" + header.path + "-suffix;"
    repeated_value = rejected_prefix + header.path + ":@" + header.path + ";tail"
    repeated_rendered = linux_test_render_toolchain_action_value(repeated_value, [sysroot, header])
    asserts.equals(env, [
        rejected_prefix,
        execution_root_marker + "/",
        header,
        ":@",
        execution_root_marker + "/",
        header,
        ";tail",
    ], repeated_rendered.fragments)
    asserts.equals(env, rejected_prefix + "external/toolchain/tool.h:@external/toolchain/tool.h;tail", linux_test_canonicalize_toolchain_action_value(
        repeated_value,
        [sysroot, header],
    ))

    # Earliest match wins across candidates, longest match at one position
    # wins across ancestors, and an exact artifact beats equal directory
    # candidates. Reversing File insertion order must not change the result.
    exact_resource = struct(
        is_directory = True,
        is_source = False,
        path = "bazel-out/k8-opt-exec/bin/" + resource_directory,
        short_path = "../toolchain/lib/clang/22/include",
    )
    generated_sibling = struct(
        is_directory = False,
        is_source = False,
        path = exact_resource.path + "/other.h",
        short_path = exact_resource.short_path + "/other.h",
    )
    competing_files = [generated_sibling, resource_header, exact_resource, header]
    competing_value = resource_directory + ";" + resource_header.path + ";" + header.path
    expected_competing = [
        execution_root_marker + "/",
        exact_resource,
        ";",
        execution_root_marker + "/",
        resource_header,
        ";",
        execution_root_marker + "/",
        header,
    ]
    for files in [competing_files, list(reversed(competing_files))]:
        asserts.equals(env, expected_competing, linux_test_render_toolchain_action_value(competing_value, files).fragments)
        asserts.equals(env, resource_directory + ";" + resource_header.path + ";external/toolchain/tool.h", linux_test_canonicalize_toolchain_action_value(competing_value, files))

    # Exhausted searches do not alter the empty/no-path contracts or reserve
    # execution-root markers. This long scalar used to run no-op loops once
    # per character for every absent indexed alias and directory.
    for scalar in ["", "-DUNRELATED=" + "x" * 1024]:
        unchanged = linux_test_render_toolchain_action_value(scalar, competing_files)
        asserts.false(env, unchanged.substituted)
        asserts.equals(env, [scalar], unchanged.fragments)
        asserts.equals(env, scalar, linux_test_canonicalize_toolchain_action_value(scalar, competing_files))

    # Repeated argument and environment text may share successful renders, but
    # not across scopes or callbacks with different typed artifact bindings.
    host_sysroot = struct(
        is_directory = True,
        is_source = False,
        path = "bazel-out/host-opt-exec/bin/external/toolchain/sysroot",
        short_path = sysroot.short_path,
    )
    contract_values = [
        "",
        "-DUNRELATED=1",
        "-Iexternal/toolchain/sysroot/include",
        repeated_value,
        bison.path + ".runfiles/bison_repo/data",
        opaque_resource_root.path + "/include",
        resource_directory,
    ] * 2
    contract_params = {}
    for role in ["target@ld", "host@cc", "target@cc", "host@ld"]:
        contract_params["action_arg_count_" + role] = str(len(contract_values))
        contract_params["action_env_count_" + role] = str(len(contract_values))
        for index, value in enumerate(contract_values):
            contract_params["action_arg_%s_%d" % (role, index)] = value
            contract_params["action_env_%s_%d" % (role, index)] = value
    common_files = [header, bison, opaque_resource_root, resource_header]
    for target_root in [sysroot, host_sysroot]:
        scoped_files = {
            "host": [host_sysroot] + common_files,
            "target": [target_root] + common_files,
        }
        contracts = linux_test_render_toolchain_action_contracts(contract_params, {
            "toolchain_files@" + scope: depset(files)
            for scope, files in scoped_files.items()
        })
        asserts.equals(env, ["host@cc", "host@ld", "target@cc", "target@ld"], contracts.keys())
        for role, contract in contracts.items():
            expected = [
                linux_test_render_toolchain_action_value(value, scoped_files[role.split("@")[0]])
                for value in contract_values
            ]
            asserts.equals(env, expected, contract.arguments)
            asserts.equals(env, expected, contract.environment)
    asserts.equals(
        env,
        {"no-remote": "1", "supports-path-mapping": "1"},
        linux_test_toolset_execution_requirements({
            "cc": {"supports-path-mapping": "1"},
            "ld": {"no-remote": "1", "supports-path-mapping": "1"},
        }, "test target"),
    )
    asserts.equals(
        env,
        "external/elfutils+/libcpu/i386.mnemonics",
        linux_test_canonical_file_path(
            "bazel-out/k8-opt-exec/bin/external/elfutils+/libcpu/i386.mnemonics",
            "../elfutils+/libcpu/i386.mnemonics",
        ),
    )
    asserts.equals(
        env,
        struct(path = "external/toolchain/bin/cc", root = "bazel-out/k8-opt-exec/bin"),
        linux_test_artifact_root_relative(
            "bazel-out/k8-opt-exec/bin/external/toolchain/bin/cc",
            "external/toolchain/bin/cc",
            "bazel-out/k8-opt-exec/bin",
        ),
    )
    asserts.equals(
        env,
        struct(path = "include/stddef.h", root = "../toolchain"),
        linux_test_artifact_root_relative(
            "../toolchain/include/stddef.h",
            "../toolchain/include/stddef.h",
            "../toolchain",
        ),
    )
    closure_file = struct(
        identity = "same-artifact",
        path = "bazel-out/k8-opt-exec/bin/toolchain/runtime",
        short_path = "toolchain/runtime",
    )
    asserts.equals(
        env,
        [closure_file],
        linux_test_filtered_toolchain_closure_files([], [closure_file, closure_file]),
        "the same additional artifact may be deduplicated",
    )
    asserts.equals(
        env,
        [],
        linux_test_filtered_toolchain_closure_files([closure_file], [closure_file]),
        "the same base artifact must win over its identical additional entry",
    )
    disjoint_file = struct(
        identity = "disjoint-artifact",
        path = "bazel-out/rbe_linux_x86_64-opt-exec/bin/toolchain/other-runtime",
        short_path = "toolchain/other-runtime",
    )
    asserts.equals(
        env,
        [disjoint_file],
        linux_test_filtered_toolchain_closure_files([closure_file], [closure_file, disjoint_file]),
        "a disjoint additional artifact must remain in the selected closure",
    )
    producer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    other_producer = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    node = {
        "inputs": {
            "payload:00000003": struct(producer = other_producer, slot = "00000001"),
            "subtool:00000000": struct(producer = producer, slot = "00000000"),
        },
    }
    bindings = linux_test_resolve_node_input_bindings(
        "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
        node,
        {
            other_producer + ":00000001": "exact-payload-tree-file",
            producer + ":00000000": "exact-generated-executable-tree-file",
        },
        {},
        {},
    )
    asserts.equals(env, {
        "payload:00000003": "exact-payload-tree-file",
        "subtool:00000000": "exact-generated-executable-tree-file",
    }, bindings)

    prior_tree_producer = "3" * 64
    artifact_tree_roots = linux_test_node_input_artifact_tree_roots(
        "c" * 64,
        {
            "host-tool:00000002": struct(producer = prior_tree_producer, slot = "00000002"),
            "payload:00000003": struct(producer = other_producer, slot = "00000001"),
            "subtool:00000000": struct(producer = producer, slot = "00000000"),
        },
        {
            other_producer + ":00000001": "exact-payload-tree-file",
            producer + ":00000000": "exact-generated-executable-tree-file",
        },
        {"host": struct(directory = "prior-host-tree-root")},
        {"objects": "current-objects-tree-root"},
        {
            other_producer + ":00000001": struct(artifact_path = "payload.o", tree = "objects"),
            prior_tree_producer + ":00000002": struct(artifact_path = "bin/host-tool", tree = "host"),
            producer + ":00000000": struct(artifact_path = "generated-tool", tree = "objects"),
        },
    )
    asserts.equals(env, {
        "host": "prior-host-tree-root",
        "objects": "current-objects-tree-root",
    }, artifact_tree_roots, "producer roots must be typed and deduplicated by output tree")

    cross_stage = linux_test_resolve_node_input_bindings(
        "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
        {"inputs": {"subtool:00000007": struct(producer = producer, slot = "00000002")}},
        {},
        {"host:bin/generated-helper": "exact-prior-stage-tree-file"},
        {
            producer + ":00000002": struct(tree = "host", artifact_path = "bin/generated-helper"),
        },
    )
    asserts.equals(env, {"subtool:00000007": "exact-prior-stage-tree-file"}, cross_stage)

    prehost_to_bootstrap = linux_test_resolve_node_input_bindings(
        "9" * 64,
        {"inputs": {"host-tool:00000000": struct(producer = producer, slot = "00000004")}},
        {},
        {"prehost:bin/early-helper": "exact-prehost-tree-file"},
        {
            producer + ":00000004": struct(tree = "prehost", artifact_path = "bin/early-helper"),
        },
    )
    asserts.equals(env, {"host-tool:00000000": "exact-prehost-tree-file"}, prehost_to_bootstrap)

    bootstrap_to_host = linux_test_resolve_node_input_bindings(
        "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
        {"inputs": {"target:00000000": struct(producer = producer, slot = "00000003")}},
        {},
        {"bootstrap:target-input.o": "exact-bootstrap-tree-file"},
        {
            producer + ":00000003": struct(tree = "bootstrap", artifact_path = "target-input.o"),
        },
    )
    asserts.equals(env, {"target:00000000": "exact-bootstrap-tree-file"}, bootstrap_to_host)

    asserts.equals(
        env,
        "prep_base",
        linux_test_tree_input_directory_name(
            "prep",
            {"prep_base": "immutable-config-or-sdk"},
            {"bootstrap": "bootstrap-output"},
            {"input_tree_alias_prep": "prep_base"},
        ),
        "bootstrap/host recipes must bind logical prep to the immutable pre-host view",
    )
    asserts.equals(
        env,
        ["prep", "prep_base"],
        linux_test_source_input_namespace_names(
            "prep_base",
            {"input_tree_alias_prep": "prep_base"},
        ),
        "aliased input trees must index source leaves under both physical and logical namespaces",
    )
    asserts.equals(
        env,
        ["rust_source_files"],
        linux_test_node_source_closure_keys(["kernel", "rust", "rust"]),
        "a Rust source edge must carry the selected recursive module closure exactly once",
    )
    asserts.equals(
        env,
        [],
        linux_test_node_source_closure_keys(["kernel", "config"]),
        "non-Rust source edges must retain their fine-grained inputs",
    )
    asserts.equals(
        env,
        ["include/config/auto.conf", "sdk-only/generated-tool"],
        linux_test_composed_tree_base_paths(
            ["include/config/auto.conf", "generated/same", "sdk-only/generated-tool"],
            ["generated/same", "module-owned/prep.h"],
        ),
        "plan-owned prep leaves must replace same-path SDK leaves",
    )
    composed_exact = linux_test_resolve_node_input_bindings(
        "f" * 64,
        {"inputs": {"generated:00000000": struct(producer = producer, slot = "00000004")}},
        {},
        {"prep:generated/same": "planned-child-in-composed-prep"},
        {producer + ":00000004": struct(tree = "prep", artifact_path = "generated/same")},
    )
    asserts.equals(env, {"generated:00000000": "planned-child-in-composed-prep"}, composed_exact)

    recipe = "d" * 64
    bindings_digest = "9" * 64
    parsed_outputs = linux_test_parse_plan_marker_paths([
        "schema/linux-kernel-plan-v5",
        "index/00000000/" + producer,
        "index/00000001/" + other_producer,
        "toolsets/host/sha256-" + ("6" * 64),
        "toolsets/target/sha256-" + ("0" * 64),
        "recipes/" + recipe + ".json",
        "nodes/target/" + producer + "/kind/generate",
        "nodes/target/" + producer + "/product/vmlinux",
        "nodes/target/" + producer + "/recipe/" + recipe,
        "nodes/target/" + producer + "/tool/cc",
        "nodes/target/" + producer + "/in/bindings/" + bindings_digest + ".json",
        "nodes/target/" + producer + "/in/tool/target/objcopy/unscoped",
        "nodes/target/" + producer + "/in/tool/host/cc/scoped",
        "nodes/target/" + producer + "/in/toolset/host",
        "nodes/target/" + producer + "/out/objects/00000000/.linux-bzl-versions/first/generated/preserved",
        "nodes/target/" + other_producer + "/kind/generate",
        "nodes/target/" + other_producer + "/product/vmlinux",
        "nodes/target/" + other_producer + "/recipe/" + recipe,
        "nodes/target/" + other_producer + "/tool/cc",
        "nodes/target/" + other_producer + "/in/bindings/" + bindings_digest + ".json",
        "nodes/target/" + other_producer + "/out/objects/00000000/generated/ephemeral",
    ], "target")
    asserts.equals(
        env,
        ".linux-bzl-versions/first/generated/preserved",
        parsed_outputs.declared_outputs[producer + ":00000000"].artifact_path,
        "plan output markers must retain their physical TreeFile path",
    )
    asserts.equals(
        env,
        ["host@cc", "objcopy"],
        sorted(parsed_outputs.nodes[producer]["tools"].keys()),
        "plan tool markers must retain scoped and source-owned binding spellings",
    )
    asserts.equals(
        env,
        ["host"],
        sorted(parsed_outputs.nodes[producer]["toolsets"].keys()),
        "plan toolset markers must retain exact recipe provenance scopes",
    )
    asserts.equals(env, {}, parsed_outputs.nodes[other_producer]["toolsets"])
    asserts.equals(env, bindings_digest, parsed_outputs.nodes[producer]["input_bindings"].id)
    asserts.equals(
        env,
        "nodes/target/" + producer + "/in/bindings/" + bindings_digest + ".json",
        parsed_outputs.nodes[producer]["input_bindings"].file.tree_relative_path,
    )
    versioned_cross_stage = linux_test_resolve_node_input_bindings(
        "e" * 64,
        {"inputs": {"versioned:00000000": struct(producer = producer, slot = "00000000")}},
        {},
        {"objects:.linux-bzl-versions/first/generated/preserved": "exact-versioned-tree-file"},
        parsed_outputs.declared_outputs,
    )
    asserts.equals(
        env,
        {"versioned:00000000": "exact-versioned-tree-file"},
        versioned_cross_stage,
        "cross-stage edges must bind the producer's physical TreeFile path",
    )

    # Persistent input sets retain their manifest DAG while binding leaf
    # provenance to exact source and producer TreeFiles.
    set_producer = "a" * 64
    set_consumer = "b" * 64
    set_root = "6" * 64
    set_leaf = "7" * 64
    set_source = "src-00000003"
    set_recipe = "8" * 64
    parsed_sets = linux_test_parse_plan_marker_paths([
        "schema/linux-kernel-plan-v5",
        "index/00000000/" + set_producer,
        "index/00000001/" + set_consumer,
        "toolsets/target/sha256-" + ("0" * 64),
        "recipes/" + set_recipe + ".json",
        "sources/" + set_source + "/kernel/include/closure.h",
        "input-sets/" + set_root + "/manifest/" + set_root + ".json",
        "input-sets/" + set_root + "/child/a/" + set_leaf,
        "input-sets/" + set_leaf + "/manifest/" + set_leaf + ".json",
        "input-sets/" + set_leaf + "/in/source/00000000/" + set_source,
        "input-sets/" + set_leaf + "/in/node/00000001/" + set_producer + "/00000000",
        "nodes/host/" + set_producer + "/out/host/00000000/bin/generated-helper",
        "nodes/target/" + set_consumer + "/kind/compile",
        "nodes/target/" + set_consumer + "/product/vmlinux",
        "nodes/target/" + set_consumer + "/recipe/" + set_recipe,
        "nodes/target/" + set_consumer + "/tool/cc",
        "nodes/target/" + set_consumer + "/in/bindings/" + bindings_digest + ".json",
        "nodes/target/" + set_consumer + "/in/input-set/" + set_root,
        "nodes/target/" + set_consumer + "/out/objects/00000000/result.o",
    ], "target", source_paths = ["include/closure.h"])
    set_dependencies = linux_test_input_set_dependencies(parsed_sets.input_sets, set_root)
    asserts.equals(env, {set_source: True}, set_dependencies.sources)
    asserts.equals(env, {
        set_producer + ":00000000": struct(producer = set_producer, slot = "00000000"),
    }, set_dependencies.inputs)
    set_producer_artifact = _fake_tree_child("host/bin/generated-helper")
    bound_sets = linux_test_bind_input_sets(
        parsed_sets.input_sets,
        parsed_sets.sources,
        {set_producer + ":00000000": set_producer_artifact},
    )
    set_args = _fake_args([])
    set_inputs = linux_test_add_input_set_args(
        set_args,
        set_root,
        parsed_sets.input_sets,
        bound_sets,
    )
    asserts.equals(env, [set_root], _flag_values(set_args.values, "-input_set_root"))
    asserts.equals(env, 2, len(_flag_values(set_args.values, "-input_set_manifest")))
    asserts.equals(env, 1, len(_flag_values(set_args.values, "-input_set_source")))
    asserts.equals(env, 1, len(_flag_values(set_args.values, "-input_set_input")))
    asserts.equals(env, 4, len(set_inputs.to_list()), "the root depset must reuse its child closure without flattening it")

    # Family transport keeps those exact leaf dependencies, including every
    # manifest, but sends only typed root anchors and bounded ordinal packs.
    family_producer_path = "nodes/%s/00000000" % set_producer
    family_producer = struct(
        path = "bazel-out/target/bin/family.objects/" + family_producer_path,
        short_path = "family.objects/" + family_producer_path,
        tree_relative_path = family_producer_path,
    )
    family_set_nodes = {}
    for set_id, input_set in parsed_sets.input_sets.nodes.items():
        family_set_nodes[set_id] = dict(input_set)
        relative = input_set["manifest"].tree_relative_path
        family_set_nodes[set_id]["manifest"] = struct(
            path = "bazel-out/target/bin/family.plan/" + relative,
            short_path = "family.plan/" + relative,
            tree_relative_path = relative,
        )
    family_sets = struct(nodes = family_set_nodes, postorder = parsed_sets.input_sets.postorder)
    family_bound = linux_test_bind_input_sets(
        family_sets,
        parsed_sets.sources,
        {set_producer + ":00000000": family_producer},
    )
    family_args = _fake_args([])
    family_inputs = linux_test_add_input_set_args(family_args, set_root, family_sets, family_bound, family = True)
    asserts.equals(env, family_bound[set_root].inputs, family_inputs)
    asserts.equals(env, 4, len(family_inputs.to_list()))
    asserts.equals(env, [family_sets.nodes[set_root]["manifest"]], _flag_values(family_args.values, "-input_set_manifest_root"))
    asserts.equals(env, [], _flag_values(family_args.values, "-input_set_manifest"))
    asserts.equals(env, [], _flag_values(family_args.values, "-input_set_input"))
    asserts.equals(env, ["00000000:0"], _flag_values(family_args.values, "-input_set_store_pack"))
    asserts.equals(env, 1, len(_flag_values(family_args.values, "-input_set_store_anchor")))
    asserts.true(env, family_producer in family_args.typed_values, "the root anchor must remain a typed artifact")
    asserts.false(env, family_sets.nodes[set_leaf]["manifest"] in family_args.typed_values, "child paths come from the authenticated manifest closure")

    # Distinct configured stores with equal canonical names must not collapse.
    # The same test covers current/prior stores, unmapped and mapped paths,
    # deterministic producer ordering, and the 256-record pack boundary.
    store_prefixes = [
        "bazel-out/config-a/bin/family.objects",
        "bazel-out/config-b/bin/family.objects",
        "bazel-out/cfg/bin/prior.objects",
    ]
    packed_artifacts = {}
    expected_indices = []
    for index in range(257):
        producer = ("0" * 64 + str(index))[-64:]
        relative = "nodes/%s/00000000" % producer
        packed_artifacts[producer + ":00000000"] = struct(
            path = store_prefixes[index % 3] + "/" + relative,
            short_path = "family.objects/" + relative,
            tree_relative_path = relative,
        )
        expected_indices.append(str(index % 3))
    packed_args = _fake_args([])
    linux_test_add_family_input_set_bindings(packed_args, packed_artifacts)
    asserts.equals(env, 3, len(_flag_values(packed_args.values, "-input_set_store_anchor")))
    asserts.equals(env, 3, len(packed_args.typed_values))
    asserts.equals(env, [
        "00000000:" + ".".join(expected_indices[:256]),
        "00000256:" + expected_indices[256],
    ], _flag_values(packed_args.values, "-input_set_store_pack"))
    reversed_args = _fake_args([])
    linux_test_add_family_input_set_bindings(reversed_args, {key: packed_artifacts[key] for key in sorted(packed_artifacts, reverse = True)})
    asserts.equals(env, packed_args.values, reversed_args.values)
    asserts.equals(env, packed_args.typed_values, reversed_args.typed_values)

    # Segment expansion must not bind persistent sets used only by a future
    # segment. Their producer TreeFiles do not exist in this segment yet.
    selected_sets = linux_test_selected_input_sets(
        parsed_sets.input_sets,
        {set_consumer: parsed_sets.nodes[set_consumer]},
    )
    asserts.equals(env, [set_root, set_leaf], sorted(selected_sets.nodes))
    prehost_recipe = "7" * 64
    prehost_node = "8" * 64
    parsed_prehost = linux_test_parse_plan_marker_paths([
        "schema/linux-kernel-plan-v5",
        "index/00000000/" + prehost_node,
        "toolsets/host/sha256-" + ("6" * 64),
        "recipes/" + prehost_recipe + ".json",
        "nodes/prehost/" + prehost_node + "/kind/generate",
        "nodes/prehost/" + prehost_node + "/product/sdk",
        "nodes/prehost/" + prehost_node + "/recipe/" + prehost_recipe,
        "nodes/prehost/" + prehost_node + "/tool/cc",
        "nodes/prehost/" + prehost_node + "/in/bindings/" + bindings_digest + ".json",
        "nodes/prehost/" + prehost_node + "/out/prehost/00000000/bin/early-helper",
    ], "prehost")
    asserts.equals(env, "bin/early-helper", parsed_prehost.declared_outputs[prehost_node + ":00000000"].artifact_path)

    # Stage parsing must retain only action metadata used by this callback,
    # independent of the order in which ExpandedDirectory exposes markers.
    current_node = "1" * 64
    prior_node = "2" * 64
    unused_node = "3" * 64
    current_recipe = "4" * 64
    unused_recipe = "5" * 64
    prior_artifact_path = ".linux-bzl-versions/prior/bin/selected-helper"
    parsed_stage = linux_test_parse_plan_marker_paths([
        "recipes/" + current_recipe + ".json",
        "recipes/" + unused_recipe + ".json",
        "sources/src-00000001/kernel/selected.c",
        "sources/src-00000002/kernel/unused.c",
        "nodes/host/" + prior_node + "/out/host/00000000/" + prior_artifact_path,
        "schema/linux-kernel-plan-v5",
        "index/00000000/" + current_node,
        "index/00000001/" + prior_node,
        "index/00000002/" + unused_node,
        "toolsets/target/sha256-" + ("0" * 64),
        "nodes/target/" + current_node + "/kind/compile",
        "nodes/target/" + current_node + "/product/vmlinux",
        "nodes/target/" + current_node + "/recipe/" + current_recipe,
        "nodes/target/" + current_node + "/tool/cc",
        "nodes/target/" + current_node + "/in/bindings/" + bindings_digest + ".json",
        "nodes/target/" + current_node + "/in/source/src/00000000/src-00000001",
        "nodes/target/" + current_node + "/in/node-pack/helper/00000000.0.1.0",
        "nodes/target/" + current_node + "/out/objects/00000000/selected.o",
    ], "target", source_paths = ["selected.c", "unused.c"])
    asserts.false(env, hasattr(parsed_stage, "children"))
    asserts.false(env, hasattr(parsed_stage, "source_children"))
    asserts.equals(env, [current_recipe], sorted(parsed_stage.recipes))
    asserts.equals(env, ["src-00000001"], sorted(parsed_stage.sources))
    asserts.equals(env, "selected.c", parsed_stage.sources["src-00000001"].file.tree_relative_path)
    asserts.equals(env, [
        current_node + ":00000000",
        prior_node + ":00000000",
    ], sorted(parsed_stage.declared_outputs))
    asserts.equals(env, "selected.o", parsed_stage.nodes[current_node]["outputs"]["00000000"].artifact_path)
    asserts.equals(env, prior_artifact_path, parsed_stage.declared_outputs[prior_node + ":00000000"].artifact_path)
    decoded_stage_inputs = linux_test_decode_packed_node_inputs(parsed_stage, current_node)
    asserts.equals(env, {
        "helper:00000000": struct(producer = prior_node, slot = "00000000"),
    }, decoded_stage_inputs)
    source_projection_manifest = struct(file = "projection-manifest", id = "7" * 64)
    asserts.equals(env, {
        "kernel": {
            "kernel-source:00000000": True,
            "kernel-source:00000002": True,
        },
    }, linux_test_decode_packed_source_projections(current_node, {
        "source_projection_packs": {"kernel": ["00000000.0,2"]},
        "source_projections": source_projection_manifest,
        "sources": {
            "kernel-source:00000000": "src-first",
            "kernel-source:00000001": "src-unprojected",
            "kernel-source:00000002": "src-second",
        },
        "trees": {"kernel": True},
    }))
    first_exact_source = struct(path = "drivers/first.c")
    second_exact_source = struct(path = "include/second.h")
    root_kconfig = struct(path = "Kconfig")
    exact_projection = {
        "kernel": {
            "kernel-source:00000002": True,
            "kernel-source:00000000": True,
        },
    }
    exact_sources = {
        "kernel-source:00000002": second_exact_source,
        "kernel-source:00000000": first_exact_source,
        "unprojected-root": root_kconfig,
    }
    source_bundle_anchor = struct(path = "bazel-out/exec/bin/source.anchor")
    source_bundle = struct(executable = source_bundle_anchor)
    exact_kernel_tree = linux_test_family_kernel_tree_binding(
        current_node,
        exact_projection,
        exact_sources,
        source_bundle,
    )
    asserts.equals(env, first_exact_source, exact_kernel_tree.path)
    asserts.true(env, exact_kernel_tree.private)
    asserts.equals(env, "", exact_kernel_tree.path_suffix)
    asserts.equals(
        env,
        None,
        exact_kernel_tree.source_runfiles,
        "a precise action must not declare the full source aggregate to spell its private tree path",
    )
    opaque_kernel_tree = linux_test_family_kernel_tree_binding(
        current_node,
        {},
        {"unprojected-root": root_kconfig},
        source_bundle,
    )
    asserts.equals(env, source_bundle_anchor, opaque_kernel_tree.path)
    asserts.false(env, opaque_kernel_tree.private)
    asserts.equals(env, ".runfiles/kernel", opaque_kernel_tree.path_suffix)
    asserts.equals(
        env,
        source_bundle,
        opaque_kernel_tree.source_runfiles,
        "an opaque action must retain the exact source-plus-marker aggregate as a tool",
    )
    source_descriptor = struct(file = first_exact_source, namespace = "kernel", path = "drivers/percent%s input.c")
    source_args = _fake_args([])
    linux_test_add_family_source_binding(source_args, "-source", "source:00000000", source_descriptor, source_bundle)
    asserts.equals(env, ["source:00000000=" + str(source_bundle_anchor) + ".runfiles/kernel/drivers/percent%s input.c"], _flag_values(source_args.values, "-source"))
    asserts.equals(env, [source_bundle_anchor], source_args.typed_values)
    private_args = _fake_args([])
    linux_test_add_family_source_binding(private_args, "-source", "source:00000000", source_descriptor)
    asserts.equals(env, [first_exact_source], private_args.typed_values)
    config_args = _fake_args([])
    config_descriptor = struct(file = root_kconfig, namespace = "capsule", path = "autoconf.h")
    linux_test_add_family_source_binding(config_args, "-source", "config:00000000", config_descriptor, source_bundle)
    asserts.equals(env, [root_kconfig], config_args.typed_values)

    # Every noncurrent descriptor in a trusted stage shard is exact and must be
    # indexed; same-stage output descriptors still remain action-local.
    same_stage_node = "6" * 64
    prior_file = struct(tree_relative_path = prior_artifact_path)
    unused_prior_file = struct(tree_relative_path = "bin/unused-helper")
    same_stage_file = struct(tree_relative_path = "same-stage.o")
    selected_prior = linux_test_selected_prior_outputs(
        {
            "host": struct(children = [prior_file, unused_prior_file]),
            "objects": struct(children = [same_stage_file]),
        },
        {
            current_node: {},
            same_stage_node: {},
        },
        {
            prior_node + ":00000000": struct(artifact_path = prior_artifact_path, tree = "host"),
            unused_node + ":00000000": struct(artifact_path = "bin/unused-helper", tree = "host"),
            same_stage_node + ":00000000": struct(artifact_path = "same-stage.o", tree = "objects"),
        },
    )
    asserts.equals(env, {
        "host:" + prior_artifact_path: prior_file,
        "host:bin/unused-helper": unused_prior_file,
    }, selected_prior)
    template_ctx = struct(
        declare_file = _fake_declare_file,
    )
    ephemeral = linux_test_declare_working_output(template_ctx, "work-tree", producer)
    asserts.equals(env, "file", ephemeral.artifact.kind)
    asserts.equals(env, producer + "/.linux-bzl-work-root", ephemeral.artifact.name)
    asserts.equals(env, "work-tree", ephemeral.artifact.parent)
    asserts.equals(env, "-working_directory_marker", ephemeral.argument)

    tools = linux_map_directory_tools(
        "exact-runner",
        "target",
        {
            "lz4": "exact-lz4-tool",
            "pahole": "exact-pahole-tool",
        },
        "exact-target-toolchain-closure",
        "exact-target-toolset-manifest",
        {"root-00000000": "exact-target-toolset-anchor"},
        {"cc": "exact-host-cc-tool"},
        "exact-host-toolchain-closure",
        "exact-host-toolset-manifest",
        {"root-00000000": "exact-host-toolset-anchor"},
        {"lz4": ["exact-lz4-helper", "exact-lz4-runtime"]},
    )
    asserts.equals(env, "exact-lz4-tool", tools.get("lz4"))
    asserts.equals(env, "exact-pahole-tool", tools.get("pahole"))
    asserts.equals(env, "exact-runner", tools.get("runner"))
    asserts.equals(env, "exact-target-toolchain-closure", tools.get("toolchain_files@target"))
    asserts.equals(env, "exact-host-toolchain-closure", tools.get("toolchain_files@host"))
    asserts.equals(env, "exact-target-toolset-manifest", tools.get("toolset_manifest@target"))
    asserts.equals(env, "exact-host-toolset-manifest", tools.get("toolset_manifest@host"))
    asserts.equals(env, "exact-target-toolset-anchor", tools.get("toolset_anchor@target@root-00000000"))
    asserts.equals(env, "exact-host-toolset-anchor", tools.get("toolset_anchor@host@root-00000000"))
    asserts.equals(env, "exact-host-cc-tool", tools.get("host@cc"))
    asserts.equals(env, ["exact-lz4-helper", "exact-lz4-runtime"], linux_test_companion_tool_bindings(tools, "lz4"))
    asserts.equals(env, {
        "lz4": "exact-lz4-tool",
        "pahole": "exact-pahole-tool",
    }, linux_test_runtime_tool_bindings(tools))
    asserts.equals(env, {
        "host@cc": "exact-host-cc-tool",
        "lz4": "exact-lz4-tool",
        "pahole": "exact-pahole-tool",
    }, linux_test_runtime_tool_bindings(tools, "target", {"host": True, "target": True}))
    asserts.false(env, linux_test_node_uses_runtime_toolset("copy", "actionfile"))
    asserts.false(env, linux_test_node_uses_runtime_toolset("generate", "actionfile"))
    asserts.true(env, linux_test_node_uses_runtime_toolset("copy", "objcopy"))
    asserts.true(env, linux_test_node_uses_runtime_toolset("copy", "actionfile", {"host@cc": True}))

    probe_tools = linux_probe_map_directory_tools(
        "exact-probe-runner",
        {
            "bindgen": "exact-bindgen-tool",
            "python3": "exact-python-tool",
            "rustc": "exact-rustc-tool",
        },
        "exact-rust-toolchain-closure",
        "exact-rust-toolset-manifest",
        {"root-00000000": "exact-rust-toolset-anchor"},
        {"bindgen": ["exact-bindgen-helper"]},
    )
    asserts.equals(env, "exact-bindgen-tool", probe_tools.get("probe_role_bindgen"))
    asserts.equals(env, "exact-python-tool", probe_tools.get("probe_role_python3"))
    asserts.equals(env, "exact-rustc-tool", probe_tools.get("probe_role_rustc"))
    asserts.equals(env, "exact-probe-runner", probe_tools.get("probe_runner"))
    asserts.equals(env, "exact-rust-toolchain-closure", probe_tools.get("toolchain_files"))
    asserts.equals(env, "exact-rust-toolset-manifest", probe_tools.get("toolset_manifest"))
    asserts.equals(env, "exact-rust-toolset-anchor", probe_tools.get("toolset_anchor_root-00000000"))
    asserts.equals(env, "exact-bindgen-helper", probe_tools.get("companion_tool_bindgen_00000000"))

    host_compile_flags = linux_test_host_dependency_compile_flags(
        struct(
            defines = ["LIBELF_STATIC=1", "LIBELF_STATIC=1"],
            external_includes = ["external/libelf/include"],
            framework_includes = [],
            includes = ["external/libelf/public"],
            local_defines = ["PRIVATE_TO_LIBELF=1"],
            local_includes = ["external/libelf/private"],
            quote_includes = ["external/libelf/quoted"],
            system_includes = [
                "external/libelf/system",
                "external/libelf/include",
            ],
        ),
        "libelf",
    )
    asserts.equals(env, [
        "-DLIBELF_STATIC=1",
        "-iquote__LINUX_BZL_HOST_DEPS__/external/libelf/quoted",
        "-isystem__LINUX_BZL_HOST_DEPS__/external/libelf/public",
        "-isystem__LINUX_BZL_HOST_DEPS__/external/libelf/system",
        "-isystem__LINUX_BZL_HOST_DEPS__/external/libelf/include",
    ], host_compile_flags)

    asserts.equals(
        env,
        [
            "--configured-before",
            "__LINUX_BZL_KBUILD_ARGS_V1__",
            "-Ityped/host-dependency/include",
            "--configured-after",
        ],
        linux_test_with_compile_action_arguments(
            [
                "--configured-before",
                "__LINUX_BZL_KBUILD_ARGS_V1__",
                "--configured-after",
            ],
            ["-Ityped/host-dependency/include"],
        ),
    )
    asserts.equals(
        env,
        [
            "--configured-before",
            "__LINUX_BZL_KBUILD_ARGS_V1__",
            "toolchain/lib/libc++.a",
            "toolchain/lib/libunwind.a",
            "--configured-after",
        ],
        linux_test_with_link_runtime_arguments(
            [
                "--configured-before",
                "__LINUX_BZL_KBUILD_ARGS_V1__",
                "--configured-after",
            ],
            [
                struct(path = "toolchain/lib/libc++.a"),
                struct(path = "toolchain/lib/libunwind.a"),
            ],
        ),
    )

    static_archive = "external/libelf/libelf.a"
    pic_static_archive = "external/libelf/libelf.pic.a"
    asserts.equals(
        env,
        static_archive,
        linux_test_host_library_artifact(
            struct(
                alwayslink = False,
                pic_static_library = pic_static_archive,
                static_library = static_archive,
            ),
            "static test library",
        ),
    )
    asserts.equals(
        env,
        pic_static_archive,
        linux_test_host_library_artifact(
            struct(
                alwayslink = False,
                pic_static_library = pic_static_archive,
                static_library = None,
            ),
            "PIC static test library",
        ),
    )

    asserts.equals(
        env,
        [
            "-L__LINUX_BZL_HOST_DEPS__/external/zlib+",
            "-L__LINUX_BZL_HOST_DEPS__/external/elfutils+",
        ],
        linux_test_host_dependency_library_search_flags([
            "__LINUX_BZL_HOST_DEPS__/external/zlib+/libz.a",
            "__LINUX_BZL_HOST_DEPS__/external/elfutils+/libelf.a",
            "__LINUX_BZL_HOST_DEPS__/external/elfutils+/libeu.a",
            "__LINUX_BZL_HOST_DEPS__/external/zlib+/libz.a",
        ]),
    )

    staged_script = "__LINUX_BZL_HOST_DEPS__/external/libelf/version.lds"
    link_paths = {
        "bazel-out/k8-opt-exec/bin/external/libelf/version.lds": staged_script,
        "external/libelf/version.lds": staged_script,
    }
    asserts.equals(
        env,
        "-Wl,--version-script=" + staged_script,
        linux_test_rewrite_host_dependency_link_flag(
            "-Wl,--version-script=external/libelf/version.lds",
            link_paths,
        ),
    )
    asserts.equals(
        env,
        "@" + staged_script,
        linux_test_rewrite_host_dependency_link_flag(
            "@bazel-out/k8-opt-exec/bin/external/libelf/version.lds",
            link_paths,
        ),
    )
    return unittest.end(env)

_mapped_kernel_backend_test = unittest.make(_mapped_kernel_backend_test_impl)

def mapped_kernel_backend_test(name):
    _mapped_kernel_backend_test(name = name)

def _requirement_conflict_probe_impl(_ctx):
    linux_test_toolset_execution_requirements({
        "cc": {"requires-network": "0"},
        "ld": {"requires-network": "1"},
    }, "conflicting target")
    return []

_requirement_conflict_probe = rule(implementation = _requirement_conflict_probe_impl)

def _requirement_conflict_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "tool execution requirements disagree on requires-network")
    return analysistest.end(env)

_requirement_conflict_test = analysistest.make(
    _requirement_conflict_test_impl,
    expect_failure = True,
)

def mapped_kernel_requirement_validation_test(name):
    subject = name + "_subject"
    _requirement_conflict_probe(name = subject, tags = ["manual"])
    _requirement_conflict_test(name = name, target_under_test = ":" + subject)

def _family_execution_platform_mismatch_probe_impl(ctx):
    linux_test_validate_family_execution_platforms(
        ctx.label,
        linux_execution_platform_label(ctx.attr._target_execution_platform),
        linux_execution_platform_label(ctx.attr._host_execution_platform),
    )
    return []

_family_execution_platform_mismatch_probe = rule(
    implementation = _family_execution_platform_mismatch_probe_impl,
    attrs = {
        "_host_execution_platform": linux_execution_platform_attr(exec_group = "host_cc"),
        "_target_execution_platform": linux_execution_platform_attr(),
    },
    exec_groups = {
        "host_cc": exec_group(toolchains = [_SECOND_PIN_TOOLCHAIN_TYPE]),
    },
    toolchains = [_FIRST_PIN_TOOLCHAIN_TYPE],
)

def _family_execution_platform_mismatch_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "requires target and host tools on one execution platform for source-selected mixed-tool actions")
    return analysistest.end(env)

_family_execution_platform_mismatch_test = analysistest.make(
    _family_execution_platform_mismatch_test_impl,
    config_settings = {
        "//command_line_option:extra_execution_platforms": [
            str(_FIRST_EXECUTION_PLATFORM),
            str(_SECOND_EXECUTION_PLATFORM),
        ],
        "//command_line_option:extra_toolchains": [
            str(Label("//internal/tests:rust_guard_pin_toolchain")),
            str(Label("//internal/tests:rust_guard_second_pin_toolchain")),
        ],
        "//command_line_option:platforms": str(_FIRST_EXECUTION_PLATFORM),
    },
    expect_failure = True,
)

def mapped_kernel_execution_platform_validation_test(name):
    subject = name + "_subject"
    _family_execution_platform_mismatch_probe(name = subject, tags = ["manual"])
    _family_execution_platform_mismatch_test(name = name, target_under_test = ":" + subject)

def _canonical_path_probe_impl(ctx):
    linux_test_canonical_file_path(ctx.attr.path, ctx.attr.short_path)
    return []

_canonical_path_probe = rule(
    implementation = _canonical_path_probe_impl,
    attrs = {
        "path": attr.string(mandatory = True),
        "short_path": attr.string(mandatory = True),
    },
)

def _toolchain_closure_collision_probe_impl(_ctx):
    first = struct(
        identity = "first",
        path = "bazel-out/first-exec/bin/toolchain/runtime",
        short_path = "toolchain/runtime",
    )
    second = struct(
        identity = "second",
        path = first.path,
        short_path = "toolchain/runtime",
    )
    linux_test_filtered_toolchain_closure_files([], [first, second])
    return []

_toolchain_closure_collision_probe = rule(implementation = _toolchain_closure_collision_probe_impl)

def _toolchain_closure_cross_configuration_collision_probe_impl(_ctx):
    base = struct(
        identity = "target-config-artifact",
        path = "bazel-out/k8-opt-exec/bin/toolchain/runtime",
        short_path = "toolchain/runtime",
    )
    additional = struct(
        identity = "exec-config-artifact",
        path = "bazel-out/rbe_linux_x86_64-opt-exec/bin/toolchain/runtime",
        short_path = "toolchain/runtime",
    )
    linux_test_filtered_toolchain_closure_files([base], [additional])
    return []

_toolchain_closure_cross_configuration_collision_probe = rule(
    implementation = _toolchain_closure_cross_configuration_collision_probe_impl,
)

def _canonical_path_failure_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, ctx.attr.expected_error)
    return analysistest.end(env)

_canonical_path_failure_test = analysistest.make(
    _canonical_path_failure_test_impl,
    attrs = {"expected_error": attr.string(mandatory = True)},
    expect_failure = True,
)

def _packed_plan_paths(
        index_markers,
        input_markers = [],
        binding_markers = None,
        toolset_markers = [],
        input_set_markers = [],
        input_set_root = None):
    node = "b" * 64
    recipe = "c" * 64
    root = "nodes/target/" + node
    if binding_markers == None:
        binding_markers = [("9" * 64) + ".json"]
    return [
        "schema/linux-kernel-plan-v5",
        "toolsets/target/sha256-" + ("0" * 64),
        "recipes/" + recipe + ".json",
        root + "/kind/compile",
        root + "/product/vmlinux",
        root + "/recipe/" + recipe,
        root + "/tool/cc",
        root + "/out/objects/00000000/result.o",
    ] + index_markers + input_set_markers + ([
        root + "/in/input-set/" + input_set_root,
    ] if input_set_root != None else []) + [
        root + "/in/bindings/" + marker
        for marker in binding_markers
    ] + [
        root + "/in/node-pack/" + marker
        for marker in input_markers
    ] + [
        root + "/in/toolset/" + marker
        for marker in toolset_markers
    ]

def _packed_plan_probe_impl(ctx):
    parsed = linux_test_parse_plan_marker_paths(ctx.attr.paths, "target")
    linux_test_decode_packed_node_inputs(parsed, ctx.attr.node_id)
    return []

_packed_plan_probe = rule(
    implementation = _packed_plan_probe_impl,
    attrs = {
        "node_id": attr.string(mandatory = True),
        "paths": attr.string_list(mandatory = True),
    },
)

def _artifact_tree_root_probe_impl(ctx):
    producer = "a" * 64
    slot = "00000000"
    output_key = producer + ":" + slot
    dependency = struct(producer = producer, slot = slot)
    outputs = {output_key: "current-output"}
    if ctx.attr.mode == "invalid_producer_id":
        dependency = struct(producer = "not-a-digest", slot = slot)
        outputs = {}
    linux_test_node_input_artifact_tree_roots(
        "b" * 64,
        {"payload:00000000": dependency},
        outputs,
        {},
        {},
        {output_key: struct(artifact_path = "result.o", tree = "objects")},
    )
    return []

_artifact_tree_root_probe = rule(
    implementation = _artifact_tree_root_probe_impl,
    attrs = {"mode": attr.string(mandatory = True)},
)

def _stage_output_tree_probe_impl(_ctx):
    node = "a" * 64
    recipe = "b" * 64
    linux_test_parse_plan_marker_paths([
        "schema/linux-kernel-plan-v5",
        "index/00000000/" + node,
        "toolsets/target/sha256-" + ("0" * 64),
        "recipes/" + recipe + ".json",
        "nodes/prep/" + node + "/kind/generate",
        "nodes/prep/" + node + "/product/vmlinux",
        "nodes/prep/" + node + "/recipe/" + recipe,
        "nodes/prep/" + node + "/tool/actionfile",
        "nodes/prep/" + node + "/in/bindings/" + ("9" * 64) + ".json",
        "nodes/prep/" + node + "/out/objects/00000000/generated/object",
    ], "prep")
    return []

_stage_output_tree_probe = rule(implementation = _stage_output_tree_probe_impl)

def _composed_tree_prefix_collision_probe_impl(_ctx):
    linux_test_composed_tree_base_paths(
        ["include/generated"],
        ["include/generated/header.h"],
    )
    return []

_composed_tree_prefix_collision_probe = rule(implementation = _composed_tree_prefix_collision_probe_impl)

def _planned_tree_prefix_collision_probe_impl(_ctx):
    linux_test_composed_tree_base_paths(
        [],
        [
            "a",
            "a-0",
            "a/x",
        ],
        tree = "host",
    )
    return []

_planned_tree_prefix_collision_probe = rule(implementation = _planned_tree_prefix_collision_probe_impl)

def _family_source_collision_probe_impl(_ctx):
    children = {}
    linux_test_record_family_source_child(children, "kernel/include/config.h", "first")
    linux_test_record_family_source_child(children, "kernel/include/config.h", "second")
    return []

_family_source_collision_probe = rule(implementation = _family_source_collision_probe_impl)

def _generated_toolchain_directory_path_probe_impl(_ctx):
    linux_test_render_toolchain_action_value(
        "-Ibazel-out/k8-opt-exec/bin/toolchain/lib/clang/22/include",
        [struct(
            is_directory = False,
            is_source = False,
            path = "bazel-out/k8-opt-exec/bin/toolchain/lib/clang/22/include/stddef.h",
            short_path = "toolchain/lib/clang/22/include/stddef.h",
        )],
    )
    return []

_generated_toolchain_directory_path_probe = rule(implementation = _generated_toolchain_directory_path_probe_impl)

def _toolchain_contract_cache_failure_probe_impl(ctx):
    source = struct(
        is_directory = False,
        is_source = True,
        path = "external/toolchain/include/stddef.h",
        short_path = "../toolchain/include/stddef.h",
    )
    generated = struct(
        is_directory = False,
        is_source = False,
        path = "bazel-out/target-opt/bin/external/toolchain/include/stddef.h",
        short_path = source.short_path,
    )
    value = "-Iexternal/toolchain/include"
    params = {
        "action_arg_count_host@cc": "2",
        "action_arg_host@cc_0": value,
        "action_arg_host@cc_1": value,
        "action_arg_count_target@cc": "1",
        "action_arg_target@cc_0": value,
    }
    tools = {"toolchain_files@host": depset([source])}
    if ctx.attr.mode == "generated_directory":
        tools["toolchain_files@target"] = depset([generated])
    elif ctx.attr.mode == "invalid_closure":
        tools["toolchain_files@target"] = depset([struct(
            is_directory = False,
            is_source = True,
            path = source.path,
            short_path = "../different/header.h",
        )])
        params["action_arg_count_target@cc"] = "0"
    elif ctx.attr.mode == "reserved_marker":
        tools["toolchain_files@target"] = depset([source])
        params["action_env_count_target@cc"] = "2"
        params["action_env_target@cc_0"] = value
        params["action_env_target@cc_1"] = linux_test_execution_root_marker()
    linux_test_render_toolchain_action_contracts(params, tools)
    return []

_toolchain_contract_cache_failure_probe = rule(
    implementation = _toolchain_contract_cache_failure_probe_impl,
    attrs = {"mode": attr.string(mandatory = True)},
)

def _input_set_transport_failure_probe_impl(ctx):
    producer = "a" * 64
    relative = "nodes/%s/00000000" % producer
    args = _fake_args([])
    if ctx.attr.mode in ["wrong_relative", "wrong_suffix"]:
        artifact = struct(
            path = "store/" + (relative if ctx.attr.mode == "wrong_relative" else "wrong/path"),
            tree_relative_path = "wrong/path" if ctx.attr.mode == "wrong_relative" else relative,
        )
        linux_test_add_family_input_set_bindings(args, {producer + ":00000000": artifact})
    else:
        root = "6" * 64
        leaf = "7" * 64
        nodes = {}
        bound = {}
        for set_id, store in [(root, "first-plan"), (leaf, "second-plan")]:
            relative = "input-sets/%s/manifest/%s.json" % (set_id, set_id)
            artifact = struct(path = store + "/" + relative, tree_relative_path = relative)
            nodes[set_id] = {"manifest": artifact, "children": {"0": leaf} if set_id == root else {}, "entries": {}}
            bound[set_id] = struct(inputs = depset([artifact]), source_artifacts = {}, producer_artifacts = {})
        linux_test_add_input_set_args(args, root, struct(nodes = nodes), bound, family = True)
    return []

_input_set_transport_failure_probe = rule(
    implementation = _input_set_transport_failure_probe_impl,
    attrs = {"mode": attr.string(mandatory = True)},
)

def mapped_kernel_path_validation_test(name):
    tests = []
    for mode, expected in [
        ("wrong_relative", "does not have its declared relative path"),
        ("wrong_suffix", "is not below its exact store root"),
        ("manifest_stores", "manifests resolve to distinct physical plan roots"),
    ]:
        subject = name + "_input_set_transport_" + mode + "_subject"
        test = name + "_input_set_transport_" + mode
        _input_set_transport_failure_probe(name = subject, mode = mode, tags = ["manual"])
        _canonical_path_failure_test(name = test, expected_error = expected, target_under_test = ":" + subject)
        tests.append(":" + test)
    for mode, expected in [
        ("generated_directory", "references generated directory"),
        ("invalid_closure", "map to different canonical paths"),
        ("reserved_marker", "contains reserved execution-root marker"),
        ("missing_closure", "has no target toolchain closure"),
    ]:
        subject = name + "_contract_cache_" + mode + "_subject"
        test = name + "_contract_cache_" + mode
        _toolchain_contract_cache_failure_probe(name = subject, mode = mode, tags = ["manual"])
        _canonical_path_failure_test(name = test, expected_error = expected, target_under_test = ":" + subject)
        tests.append(":" + test)
    for mode, expected in [
        ("mixed_mode", "mixes pinned mode with cut node"),
        ("unknown_node", "unknown or repeated node"),
        ("invalid_seal", "invalid or repeated marker"),
        ("duplicate_mode", "invalid or repeated marker"),
        ("duplicate_schema", "invalid or repeated marker"),
        ("duplicate_node", "unknown or repeated node"),
        ("duplicate_seal", "invalid or repeated marker"),
        ("missing_seal", "requires schema, mode, and seal"),
        ("missing_schema", "requires schema, mode, and seal"),
        ("missing_mode", "requires schema, mode, and seal"),
        ("unknown_marker", "invalid or repeated marker"),
        ("node_bound", "exceeds node marker bound"),
        ("direct_not_closed", "depends on unselected producer"),
        ("set_not_closed", "depends on unselected producer"),
        ("missing_slot", "missing outputs"),
        ("extra_slot", "extra or misplaced output"),
        ("wrong_tree", "extra or misplaced output"),
        ("duplicate_slot", "unknown or repeated pinned output"),
        ("unknown_slot_node", "unknown or repeated pinned output"),
        ("invalid_slot_path", "invalid output"),
        ("slot_bound", "exceeds copied slot bound"),
        ("cut_views", "must not emit final views"),
        ("execution_source", "has unstaged source"),
        ("cut_store_source", "has unstaged source"),
    ]:
        subject = name + "_execution_" + mode + "_subject"
        test = name + "_execution_" + mode
        _family_execution_failure_probe(name = subject, mode = mode, tags = ["manual"])
        _canonical_path_failure_test(name = test, expected_error = expected, target_under_test = ":" + subject)
        tests.append(":" + test)
    for case in [
        struct(
            expected_error = "has invalid relative path",
            name = "parent",
            path = "external/repo/../escape",
            short_path = "external/repo/../escape",
        ),
        struct(
            expected_error = "has invalid relative path",
            name = "absolute",
            path = "/tmp/tool",
            short_path = "/tmp/tool",
        ),
        struct(
            expected_error = "map to different canonical paths",
            name = "mismatch",
            path = "bazel-out/k8-opt-exec/bin/external/repo/one",
            short_path = "../repo/two",
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        _canonical_path_probe(
            name = subject,
            path = case.path,
            short_path = case.short_path,
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    consumer = "b" * 64
    for case in [
        struct(
            expected_error = "repeats node index ordinal or ID",
            indexes = ["index/00000000/" + ("a" * 64), "index/00000000/" + consumer],
            inputs = [],
            name = "packed_duplicate_index",
        ),
        struct(
            indexes = ["index/00000000/" + consumer],
            expected_error = "non-canonical base36 ordinal",
            inputs = ["payload/00000000.00.0.0"],
            name = "packed_noncanonical_base36",
        ),
        struct(
            bindings = [],
            expected_error = "has no input binding manifest",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "missing_input_bindings",
        ),
        struct(
            bindings = ["sha256-" + ("9" * 64) + ".json"],
            expected_error = "has invalid input binding manifest",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "invalid_input_bindings_id",
        ),
        struct(
            bindings = [("8" * 64) + ".json", ("9" * 64) + ".json"],
            expected_error = "repeats input binding manifest",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "duplicate_input_bindings",
        ),
        struct(
            expected_error = "has invalid or repeated toolset scope",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "invalid_toolset_scope",
            toolsets = ["exec"],
        ),
        struct(
            expected_error = "has invalid or repeated toolset scope",
            indexes = ["index/00000000/" + consumer],
            inputs = [],
            name = "duplicate_toolset_scope",
            toolsets = ["host", "host"],
        ),
        struct(
            expected_error = "references unknown input-set root",
            indexes = ["index/00000000/" + consumer],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_unknown_root",
        ),
        struct(
            expected_error = "invalid or repeated child marker",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
                "input-sets/%s/child/AA/%s" % ("d" * 64, "e" * 64),
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_malformed_child",
        ),
        struct(
            expected_error = "references unknown child",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
                "input-sets/%s/child/a/%s" % ("d" * 64, "e" * 64),
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_unknown_child",
        ),
        struct(
            expected_error = "input-set graph contains a cycle",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
                "input-sets/%s/child/a/%s" % ("d" * 64, "e" * 64),
                "input-sets/%s/manifest/%s.json" % ("e" * 64, "e" * 64),
                "input-sets/%s/child/b/%s" % ("e" * 64, "d" * 64),
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_cycle",
        ),
        struct(
            expected_error = "entry ordinals are not contiguous",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
                "input-sets/%s/in/node/00000001/%s/00000000" % ("d" * 64, consumer),
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_noncontiguous_entry",
        ),
        struct(
            expected_error = "maximum is 16",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
            ] + [
                "input-sets/%s/in/node/%s/%s/00000000" % (
                    "d" * 64,
                    "000000" + ("0" if ordinal < 10 else "") + str(ordinal),
                    consumer,
                )
                for ordinal in range(17)
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_leaf_bound",
        ),
        struct(
            expected_error = "references unknown source",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
                "input-sets/%s/in/source/00000000/src-00000099" % ("d" * 64),
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_unknown_source",
        ),
        struct(
            expected_error = "references unknown producer",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
                "input-sets/%s/in/node/00000000/%s/00000000" % ("d" * 64, "e" * 64),
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_unknown_producer",
        ),
        struct(
            expected_error = "references unavailable output slot",
            indexes = ["index/00000000/" + consumer],
            input_set_markers = [
                "input-sets/%s/manifest/%s.json" % ("d" * 64, "d" * 64),
                "input-sets/%s/in/node/00000000/%s/00000001" % ("d" * 64, consumer),
            ],
            input_set_root = "d" * 64,
            inputs = [],
            name = "input_set_output_bound",
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        _packed_plan_probe(
            name = subject,
            node_id = consumer,
            paths = _packed_plan_paths(
                case.indexes,
                input_markers = case.inputs,
                binding_markers = getattr(case, "bindings", None),
                toolset_markers = getattr(case, "toolsets", []),
                input_set_markers = getattr(case, "input_set_markers", []),
                input_set_root = getattr(case, "input_set_root", None),
            ),
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    for case in [
        struct(
            expected_error = "has invalid producer binding",
            mode = "invalid_producer_id",
            name = "artifact_tree_invalid_producer_id",
        ),
        struct(
            expected_error = "requires unavailable current-stage output artifact tree objects",
            mode = "missing_current_root",
            name = "artifact_tree_missing_current_root",
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        _artifact_tree_root_probe(
            name = subject,
            mode = case.mode,
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    for case in [
        struct(
            expected_error = "cannot write objects tree",
            name = "stage_output_tree",
            rule = _stage_output_tree_probe,
        ),
        struct(
            expected_error = "file/subtree collision",
            name = "composed_tree_prefix",
            rule = _composed_tree_prefix_collision_probe,
        ),
        struct(
            expected_error = "file/subtree collision",
            name = "planned_tree_prefix",
            rule = _planned_tree_prefix_collision_probe,
        ),
        struct(
            expected_error = "references generated directory",
            name = "generated_toolchain_directory",
            rule = _generated_toolchain_directory_path_probe,
        ),
        struct(
            expected_error = "provided by distinct artifacts",
            name = "family_source_collision",
            rule = _family_source_collision_probe,
        ),
    ]:
        subject = name + "_" + case.name + "_subject"
        test = name + "_" + case.name
        case.rule(name = subject, tags = ["manual"])
        _canonical_path_failure_test(
            name = test,
            expected_error = case.expected_error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    subject = name + "_toolchain_closure_additional_collision_subject"
    test = name + "_toolchain_closure_additional_collision"
    _toolchain_closure_collision_probe(name = subject, tags = ["manual"])
    _canonical_path_failure_test(
        name = test,
        expected_error = "collides with distinct artifact",
        target_under_test = ":" + subject,
    )
    tests.append(":" + test)
    subject = name + "_toolchain_closure_cross_configuration_collision_subject"
    test = name + "_toolchain_closure_cross_configuration_collision"
    _toolchain_closure_cross_configuration_collision_probe(name = subject, tags = ["manual"])
    _canonical_path_failure_test(
        name = test,
        expected_error = "test merged toolchain closure",
        target_under_test = ":" + subject,
    )
    tests.append(":" + test)
    native.test_suite(name = name, tests = tests)

def _host_dependency_tree_artifact_probe_impl(ctx):
    tree = ctx.actions.declare_directory(ctx.label.name + ".headers")
    linux_test_validate_host_dependency_artifact(tree, "test header")
    return []

_host_dependency_tree_artifact_probe = rule(implementation = _host_dependency_tree_artifact_probe_impl)

def _host_library_validation_probe_impl(ctx):
    fields = {
        "alwayslink": False,
        "dynamic_library": None,
        "interface_library": None,
        "objects": [],
        "pic_objects": [],
        "pic_static_library": None,
        "static_library": None,
    }
    if ctx.attr.kind == "alwayslink":
        fields["alwayslink"] = True
        fields["static_library"] = "libalways.a"
    elif ctx.attr.kind == "dynamic":
        fields["dynamic_library"] = "libdynamic.so"
    elif ctx.attr.kind == "objects":
        fields["objects"] = ["loose.o"]
    linux_test_host_library_artifact(struct(**fields), "test library")
    return []

_host_library_validation_probe = rule(
    implementation = _host_library_validation_probe_impl,
    attrs = {"kind": attr.string(mandatory = True)},
)

def mapped_kernel_host_dependency_validation_test(name):
    tests = []
    tree_subject = name + "_tree_subject"
    tree_test = name + "_tree"
    _host_dependency_tree_artifact_probe(name = tree_subject, tags = ["manual"])
    _canonical_path_failure_test(
        name = tree_test,
        expected_error = "test header is a TreeArtifact",
        target_under_test = ":" + tree_subject,
    )
    tests.append(":" + tree_test)
    for case in [
        struct(error = "requires alwayslink/whole-archive semantics", kind = "alwayslink"),
        struct(error = "has only dynamic/interface library inputs", kind = "dynamic"),
        struct(error = "has only loose object inputs", kind = "objects"),
    ]:
        subject = name + "_" + case.kind + "_subject"
        test = name + "_" + case.kind
        _host_library_validation_probe(
            name = subject,
            kind = case.kind,
            tags = ["manual"],
        )
        _canonical_path_failure_test(
            name = test,
            expected_error = case.error,
            target_under_test = ":" + subject,
        )
        tests.append(":" + test)
    native.test_suite(name = name, tests = tests)
