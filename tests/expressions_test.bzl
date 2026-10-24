"""Native assignment timing and complete probe context regressions."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//rules:commands.bzl", "command_scope", "compiler_flags", "native_tool_environment", "postprocess_commands", "shell_words", "value")
load("//rules:expressions.bzl", "evaluate", "program")
load("//rules:host_tools.bzl", "resolve_subcmd_archive", "target_exports")
load("//rules:image.bzl", "architecture_context", "directory_context", "rule_prerequisites")
load("//rules:toolchain.bzl", "make_setup")

def _literal(value):
    return [dict(op = "literal", value = value)]

def _reference(name):
    return [dict(op = "reference", name = _literal(name))]

def _assignment(name, value, flavor = "simple"):
    return dict(kind = "assignment", name_expression = _literal(name), value_expression = value, value = "", flavor = flavor)

def _probe():
    return [dict(op = "function", name = "kbuild_probe", arguments = [_literal("cc-option"), _literal("-fno-common")])]

def _context_test(ctx):
    env = unittest.begin(ctx)
    asserts.equals(env, ["-fdebug-compilation-dir=.", "-g", "-m16"], compiler_flags("clang -g -m16 -c source.c -o target.o", "clang", "source.c", "target.o", True))
    asserts.equals(env, ["-fdebug-compilation-dir=.", "-g", "-fdebug-compilation-dir=native"], compiler_flags("clang -g -fdebug-compilation-dir=native -c source.c -o target.o", "clang", "source.c", "target.o", True))
    asserts.equals(env, ["-fdebug-compilation-dir=.", "-ffile-compilation-dir=native", "-fdebug-prefix-map=native=recorded"], compiler_flags("clang -ffile-compilation-dir=native -fdebug-prefix-map=native=recorded -c source.c -o target.o", "clang", "source.c", "target.o", True))
    asserts.equals(env, ["-g"], compiler_flags("gcc -g -c source.c -o target.o", "gcc", "source.c", "target.o", False))

    # The native module writer and its modpost child inherit MODORDER from
    # setup, including its source-selected prefix rather than a fixed path.
    module_variables = [
        dict(_assignment("sub_make_done", _literal("1")), export = 1),
        _assignment("extmod_prefix", [dict(op = "function", name = "if", arguments = [[dict(op = "reference", name = _literal("KBUILD_EXTMOD"))], [dict(op = "reference", name = _literal("KBUILD_EXTMOD"))] + _literal("/")])], flavor = "recursive"),
        dict(_assignment("MODORDER", [dict(op = "reference", name = _literal("extmod_prefix"))] + _literal("modules.order")), export = 1),
    ]
    architecture = [
        _assignment("M16_CFLAGS", [dict(op = "function", name = "kbuild_probe", arguments = [_literal("cc-option"), _literal("-m16"), _literal("-m32 -Wa,code16gcc.h")])]),
        dict(_assignment("REALMODE_CFLAGS", _reference("M16_CFLAGS")), export = 1),
        _assignment("KBUILD_CFLAGS", _literal("-mcmodel=kernel"), flavor = "append"),
        dict(kind = "rule", targets_expression = _reference("objtree") + _literal("/arch/x86/boot/bzImage"), tail_expression = _reference("srctree") + _literal("/arch/x86/boot/header.h ") + _reference("KCONFIG_CONFIG"), separator = ":", secondary_inputs = {"early_source": _reference("srctree") + _literal("/arch/x86/boot/header.h ") + _reference("KCONFIG_CONFIG")}, secondary_expression = _reference("early_source") + _literal(" ") + _reference("@")),
        dict(kind = "recipe", value_expression = _reference("MAKE") + _literal(" -f ") + _reference("srctree") + _literal("/scripts/Makefile.build obj=arch/x86/boot")),
    ]
    target_flags = dict(kind = "target_assignment", targets_expression = _literal("arch/x86/boot/bzImage"), name_expression = _literal("asflags-y"), value_expression = _literal("-DSETUP_TARGET"), value = "-DSETUP_TARGET", flavor = "append")
    setup_requests = {}
    setup = make_setup(
        ctx,
        struct(arch = "x86_64", architecture = struct(srcarch = "x86", subarch = "x86", header_arch = "x86"), values = {}, make_values = {}, identity = "fixture"),
        struct(root = struct(path = "source"), logical = []),
        struct(commands = {"lz4": struct(path = "declared-lz4")}, environment = {}),
        {"CC": "selected compiler", "HOSTCC": "host-compiler"},
        "compiler/include",
        ["-target", "aarch64-linux-musl"],
        ["-target", "x86_64-linux-gnu"],
        ["-host-link"],
        {},
        setup_requests,
        [],
        {
            "capabilities": {},
            "global": [
                _assignment("CC", _literal("ignored-makefile-compiler")),
                _assignment("SELECTED_PROBE", _probe()),
                dict(kind = "directive", directive = "include", arguments_expression = _reference("srctree") + _literal("/arch/x86/Makefile")),
                _assignment("KBUILD_CPPFLAGS", _literal("--target=aarch64-linux-gnu")),
                _assignment("KBUILD_EXTMOD", _literal("external-module")),
                _assignment("KBUILD_CHECKSRC", _literal("1")),
                target_flags,
            ],
            "image_cpp": [],
            "image_exports": [dict(kind = "conditional", branches = [dict(
                condition = dict(directive = "ifneq", operands = [[dict(op = "reference", name = _literal("sub_make_done"))], _literal("1")]),
                body = [dict(kind = "directive", directive = "export", arguments = "KBUILD_CHECKSRC", arguments_expression = _literal("KBUILD_CHECKSRC"))],
            )])],
            "image_flag_programs": {},
            "image_programs": {"arch/x86/Makefile": architecture},
            "image_link_global": [],
            "image_link_programs": {},
            "image_module_variables": module_variables,
            "image_setup_programs": {"arch/x86/Makefile": architecture[:3]},
            "image_utilities": [_assignment("LZ4", _literal("lz4"))],
            "image_version": [],
        },
    )
    asserts.equals(env, "selected compiler", setup.tools["CC"])
    cc = evaluate([dict(op = "reference", name = _literal("CC"))], setup.variables, setup.sources)
    asserts.equals(env, ["selected compiler", "-target", "aarch64-linux-musl"], shell_words(cc))
    asserts.equals(env, cc, setup.make_tools["CC"])
    asserts.equals(env, ["selected compiler", "-target", "aarch64-linux-musl"], setup.cc_arguments)
    asserts.equals(env, "declared-lz4", setup.make_tools["LZ4"])

    # Native ${CC} word-splits arguments without parsing Make's quote bytes.
    script_tools = native_tool_environment(struct(
        tools = {"CC": "selected-compiler", "HOSTCC": "host-compiler"},
        cc_arguments = ["selected-compiler"] + setup.cc_arguments[1:],
    ))
    asserts.equals(env, {"CC": "selected-compiler -target aarch64-linux-musl", "HOSTCC": "host-compiler"}, script_tools)
    asserts.equals(env, cc, setup.variables["__command_line__"]["CC"])
    asserts.equals(env, cc, setup.sources["__probe_environment__"]["CC"])
    evaluate([dict(op = "reference", name = _literal("SELECTED_PROBE"))], setup.variables, setup.sources)
    asserts.equals(env, 1, len(setup_requests))
    request = setup_requests.values()[0]
    asserts.equals(env, cc, request["compiler"])
    asserts.equals(env, "host-compiler", request["environment"]["HOSTCC"])
    asserts.equals(env, ["-target", "x86_64-linux-gnu"], setup.host_compile_flags)
    asserts.equals(env, ["-host-link"], setup.host_link_flags)
    command = [dict(op = "reference", name = _literal("CC"))] + _literal(" ") + [dict(op = "reference", name = _literal("KBUILD_CPPFLAGS"))]
    asserts.equals(env, ["selected compiler", "-target", "aarch64-linux-musl", "--target=aarch64-linux-gnu"], shell_words(evaluate(command, setup.variables, setup.sources)))
    destination = [dict(op = "reference", name = _literal("MODORDER"))]
    writer = _literal("{ echo net/sched/sch_ingress.ko; } > ") + destination
    asserts.equals(env, "{ echo net/sched/sch_ingress.ko; } > external-module/modules.order", evaluate(writer, setup.variables, setup.sources))
    asserts.equals(env, "external-module/modules.order", evaluate(destination, target_exports(setup), {}))
    asserts.equals(env, "1", evaluate([dict(op = "reference", name = _literal("KBUILD_CHECKSRC"))], target_exports(setup), {}))
    in_tree = program(module_variables, {}, {}).variables
    asserts.equals(env, "{ echo net/sched/sch_ingress.ko; } > modules.order", evaluate(writer, in_tree, {}))

    # Architecture simple variables and rules are retained from their native
    # include, before later kernel flags. Boot dispatch must not probe again.
    dispatch = architecture_context(setup)
    asserts.equals(env, "-m32 -Wa,code16gcc.h", value("REALMODE_CFLAGS", dispatch.variables, setup.sources))
    requests = [request for request in setup_requests.values() if request.get("argument") == "-m16"]
    asserts.equals(env, 1, len(requests))
    asserts.equals(env, "", requests[0]["flags"])
    asserts.equals(env, "-mcmodel=kernel", value("KBUILD_CFLAGS", dispatch.variables, setup.sources))
    asserts.equals(env, ["arch/x86/boot/bzImage"], dispatch.rules[0]["targets"])
    asserts.equals(env, "__source__/arch/x86/boot/header.h .config", dispatch.rules[0]["prerequisites"])
    asserts.equals(env, "__source__/arch/x86/boot/header.h .config arch/x86/boot/bzImage", rule_prerequisites(dispatch.rules[0], "arch/x86/boot/bzImage", "", dispatch.variables, setup.sources))
    asserts.equals(env, [target_flags], dispatch.target_assignments)
    asserts.equals(env, "__make_dispatch__ -f __source__/scripts/Makefile.build obj=arch/x86/boot", evaluate(dispatch.rules[0]["recipes"][0], dispatch.variables, setup.sources))

    # A recursive boot invocation materializes exports, then the graph and
    # command scopes evaluate different native slices. Imported graph cells
    # must not alias command cells in either ordinary or shell-capture caches.
    directory = "arch/x86/boot/compressed"
    local = [
        _assignment("KBUILD_CFLAGS", _literal("-m64 -fPIE")),
        _assignment("KBUILD_AFLAGS", _reference("KBUILD_CFLAGS") + _literal(" -D__ASSEMBLY__")),
    ]
    graph = _assignment("lib-y", [dict(op = "function", name = "filter-out", arguments = [_reference("obj-y"), [dict(op = "function", name = "sort", arguments = [_reference("lib-y") + _literal(" ") + _reference("lib-m")])]])])
    boot_program = dict(
        image_programs = {directory + "/Makefile": local},
        image_flag_programs = {directory + "/Makefile": local},
        image_build_init = [],
        image_common = [],
        graph_init = [_assignment("lib-y", _literal(""))],
        graph_helpers = [graph],
        graph_variables = ["lib-y"],
        image_helpers = [_assignment("KBUILD_AFLAGS", _reference("subdir-asflags-y"), flavor = "append")],
        image_object_helpers = [dict(kind = "conditional", branches = [dict(
            condition = dict(directive = "ifneq", operands = [[dict(op = "function", name = "strip", arguments = [_reference("lib-y") + _literal(" ") + _reference("lib-m") + _literal(" ") + _reference("lib-")])], _literal("")]),
            body = [_assignment("targets-for-builtin", _reference("obj") + _literal("/lib.a"), flavor = "append")],
        )])],
        image_implicit_rules = [],
    )
    boot_sources = {}
    boot_variables = program([], {"CC": "clang", "__config__": {}}, boot_sources).variables
    boot_setup = struct(variables = boot_variables, sources = boot_sources, program = boot_program, tools = {name: name for name in ["KGZIP", "LEX", "YACC", "AWK"]}, architecture = struct(arch = "x86_64", header_arch = "x86"))
    environment = {"CC": {"value": "clang"}, "__exports__": {"CC": True}}
    state = dict(arch = "x86", setup = boot_setup, program = boot_program, contexts = {}, invocations = {directory: struct(environment = environment)}, environment = environment, directory_modes = {}, modules = [], config = {}, dtbs_lists = {}, sources = boot_sources, children = {}, parents = {})
    graph_context = directory_context(directory, state)
    scope = command_scope(boot_setup, directory, directory + "/kernel_info.o", "__object__", "__source__", graph_variables = graph_context.graph_variables, invocation_environment = environment)
    command = program([
        _assignment("cmd_as_o_S", _reference("CC") + _literal(" ") + _reference("KBUILD_AFLAGS") + _literal(" -c ") + _reference("<") + _literal(" -o ") + _reference("@"), flavor = "recursive"),
    ], scope, boot_sources, cells = True).variables
    command["<"] = {"value": directory + "/kernel_info.S"}
    expected = ["clang", "-m64", "-fPIE", "-D__ASSEMBLY__", "-c", directory + "/kernel_info.S", "-o", directory + "/kernel_info.o"]
    asserts.equals(env, expected, shell_words(value("cmd_as_o_S", command, boot_sources)))
    asserts.equals(env, expected, shell_words(value("cmd_as_o_S", command, {"__shell_commands__": []})))

    # GNU Make's first prerequisite expansion freezes ordinary operands;
    # escaped references are evaluated later in the selected target scope.
    first = [dict(op = "reference", name = _literal("FIRST"))]
    secondary = program([
        _assignment("FIRST", _literal("original.c")),
        dict(
            kind = "rule",
            targets_expression = _literal("%.o"),
            tail_expression = first + _literal(" $(objtool_dep) $@"),
            separator = ":",
            secondary_inputs = {"__first": first},
            secondary_expression = [dict(op = "reference", name = _literal("__first"))] + _literal(" ") + [dict(op = "reference", name = _literal("objtool_dep"))] + _literal(" ") + [dict(op = "reference", name = _literal("@"))],
        ),
        _assignment("FIRST", _literal("changed.c")),
        _assignment("objtool_dep", _literal("tools/objtool/objtool")),
    ], {}, {}, cells = True)
    asserts.equals(env, "original.c tools/objtool/objtool lib/ctype.o", rule_prerequisites(secondary.rules[0], "lib/ctype.o", "lib/ctype", secondary.variables, {}))
    asserts.equals(env, "plain.h", rule_prerequisites({"prerequisites": "plain.h"}, "lib/ctype.o", "lib/ctype", {}, {}))
    requests = {}
    sources = {"__requests__": requests, "__probe_environment__": {"SELECTED": "yes"}}
    initial = {"__command_line__": {"CC": "compiler"}, "CC": "compiler", "KBUILD_CPPFLAGS": "-D__KERNEL__", "KBUILD_CFLAGS": "", "objtree": ".", "srctree": "source"}
    result = program([
        _assignment("CC", _literal("ignored-makefile-compiler")),
        _assignment("KBUILD_CFLAGS", _literal("-m32")),
        _assignment("FIRST", _probe()),
        _assignment("KBUILD_CFLAGS", _literal("-m64"), flavor = "append"),
        _assignment("SECOND", _probe()),
    ], initial, sources)
    evaluate([dict(op = "reference", name = _literal("FIRST"))], result.variables, sources)
    evaluate([dict(op = "reference", name = _literal("SECOND"))], result.variables, sources)
    asserts.equals(env, 2, len(requests))
    asserts.equals(env, ["-D__KERNEL__ -m32", "-D__KERNEL__ -m32 -m64"], [r["flags"] for r in requests.values()])
    asserts.equals(env, "-m32 -m64", evaluate([dict(op = "reference", name = _literal("KBUILD_CFLAGS"))], result.variables, sources))
    origin = [dict(op = "function", name = "origin", arguments = [_literal("CC")])]
    asserts.equals(env, "command line", evaluate(origin, result.variables, sources))
    for request in requests.values():
        asserts.equals(env, "compiler", request["compiler"])
        asserts.equals(env, {"SELECTED": "yes"}, request["environment"])
        asserts.equals(env, ".", request["directory"])
        asserts.equals(env, "source", request["source"])
    explicit = _probe()[0]
    explicit["probe_context"] = dict(compiler = _literal("explicit-compiler"), flags = _literal("-m16"))
    evaluate([explicit], result.variables, sources)
    asserts.equals(env, "explicit-compiler", requests.values()[-1]["compiler"])
    asserts.equals(env, "-m16", requests.values()[-1]["flags"])

    # Native subcmd's lazy OUTPUT must request its directory measurement
    # before the owner decides that the current probe round has converged.
    directory = "bazel-out/config/bin/kernel.generated/libsubcmd"
    command = "cd __object__; cd " + directory + "; pwd"
    subcmd_program = [
        _assignment("ABSOLUTE_O", [dict(op = "function", name = "kbuild_host_shell", arguments = [_literal(command)])]),
        _assignment("OUTPUT", [dict(op = "reference", name = _literal("ABSOLUTE_O"))] + _literal("/")),
        _assignment("LIBFILE", [dict(op = "reference", name = _literal("OUTPUT"))] + _literal("libsubcmd.a")),
    ]
    request = dict(kind = "host-shell", command = command, environment = {}, directory = ".", source = "source", directories = [directory])
    for capabilities in [{}, {json.encode(request): directory}]:
        pending = {}
        source_context = {"__requests__": pending, "__capabilities__": capabilities, "__probe_environment__": {}, "__source_root__": "source", "__probe_directories__": [directory]}
        subcmd = program(subcmd_program, {}, source_context, cells = True).variables
        asserts.equals(env, {}, pending)
        archive = resolve_subcmd_archive(subcmd, source_context, directory + "/libsubcmd.a")
        if capabilities:
            asserts.equals(env, directory + "/libsubcmd.a", archive)
            asserts.equals(env, {}, pending)
        else:
            asserts.equals(env, None, archive)
            asserts.equals(env, {json.encode(request): request}, pending)

    # ARM64 exports this flag lazily. Its compiler include belongs to the
    # target context, even when a later host Make invocation inherits it.
    target_sources = {"__compiler_include__": "target/compiler/include"}
    target_variables = program([
        _assignment("CC_FLAGS_FPU", _literal("-ffreestanding -isystem ") + [dict(op = "function", name = "kbuild_compiler_include", arguments = [])]),
    ], {}, target_sources, cells = True).variables
    target_variables["__exports__"] = {"CC_FLAGS_FPU": True}
    inherited = target_exports(struct(variables = target_variables, sources = target_sources))
    host_sources = {"__compiler_include__": "host/compiler/include"}
    asserts.equals(env, "-ffreestanding -isystem target/compiler/include", evaluate([dict(op = "reference", name = _literal("CC_FLAGS_FPU"))], inherited, host_sources))

    # Both native include spellings refer to the already loaded auto.conf
    # values. Keep the native Make string representation unchanged.
    native_values = {"CONFIG_NATIVE": "y", "CONFIG_NATIVE_STRING": '\"quoted\\\\value\"'}
    for prefix in ["", "__object__/"]:
        for suffix in ["", ".cmd"]:
            configured = program([
                dict(kind = "directive", directive = "include", arguments_expression = _literal(prefix + "include/./config/auto.conf" + suffix)),
                _assignment("SELECTED", [dict(op = "reference", name = _literal("CONFIG_NATIVE_STRING"))]),
            ], {"__config__": native_values}, {})
            asserts.equals(env, native_values, configured.variables["__config__"])
            asserts.equals(env, native_values["CONFIG_NATIVE_STRING"], evaluate([dict(op = "reference", name = _literal("SELECTED"))], configured.variables))
    other_include = "__object__/include/config/native.mk"
    included = program([
        dict(kind = "directive", directive = "include", arguments_expression = _literal(other_include)),
    ], {}, {"__includes__": {other_include: [_assignment("NATIVE_INCLUDE", _literal("selected"))]}})
    asserts.equals(env, "selected", evaluate([dict(op = "reference", name = _literal("NATIVE_INCLUDE"))], included.variables))

    # A simple assignment captures once in each recipe expansion context.
    # Reusing the cell for another action must not reuse the previous slot.
    captures = {"__shell_commands__": []}
    command = [dict(op = "function", name = "shell", arguments = [_literal("printf captured")])]
    result = program([
        _assignment("SIMPLE", command),
        _assignment("RECURSIVE", command, flavor = "recursive"),
    ], {}, captures)
    simple = [dict(op = "reference", name = _literal("SIMPLE"))]
    recursive = [dict(op = "reference", name = _literal("RECURSIVE"))]
    asserts.equals(env, "__KBUILD_SHELL_RESULT_0__", evaluate(simple, result.variables, captures))
    asserts.equals(env, "__KBUILD_SHELL_RESULT_0__", evaluate(simple, result.variables, captures))
    asserts.equals(env, ["printf captured"], captures["__shell_commands__"])
    other = {"__shell_commands__": []}
    asserts.equals(env, "__KBUILD_SHELL_RESULT_0__", evaluate(simple, result.variables, other))
    asserts.equals(env, "__KBUILD_SHELL_RESULT_0__", evaluate(simple, result.variables, other))
    asserts.equals(env, "__KBUILD_SHELL_RESULT_1__", evaluate(recursive, result.variables, other))
    asserts.equals(env, "__KBUILD_SHELL_RESULT_2__", evaluate(recursive, result.variables, other))
    asserts.equals(env, ["printf captured"] * 3, other["__shell_commands__"])
    asserts.equals(env, ["printf captured"], captures["__shell_commands__"])

    # An empty native tail needs no source/header staging, but an empty-valued shell
    # expansion still belongs to the recipe and must execute its capture.
    post_setup = struct(sources = {}, program = {"image_objtool_variables": []}, tools = {})
    asserts.equals(env, None, postprocess_commands(post_setup, "target.o", {"cmd_empty": {"value": " ;  "}}, ["cmd_empty"], {}))
    post = postprocess_commands(post_setup, "target.o", {"cmd_nonempty": {"value": " ; selected-tool target.o"}}, ["cmd_nonempty"], {})
    asserts.equals(env, ["selected-tool target.o"], post.commands)
    asserts.false(env, post.object_only)
    tool = struct(path = "generated/objtool")
    dependencies = {"tools/objtool/objtool": tool}
    object_command = tool.path + " --orc --hacks=noinstr --static-call --uaccess --noabs target.o"
    dependency_command = "{ echo ; echo 'target.o: $(wildcard generated/objtool)' ; } >> .target.o.cmd"
    for text, object_only in [
        (object_command, True),
        (object_command.replace(" ", " \t "), True),
        (dependency_command, True),
        ("other/objtool --orc target.o", False),
        ("generated/objtool --orc extra.o target.o", False),
        ("generated/objtool --unknown target.o", False),
        ("generated/objtool --output=other.o target.o", False),
        ("generated/objtool orc generate target.o", False),
        ("generated/objtool --orc $(cat path) target.o", False),
        ("generated/objtool --orc\n target.o", False),
        (dependency_command.replace("echo ;", "cat source ;"), False),
    ]:
        post = postprocess_commands(post_setup, "target.o", {"native_tail": {"value": text}}, ["native_tail"], dependencies)
        asserts.equals(env, [text], post.commands)
        asserts.equals(env, object_only, post.object_only)
    for name in ["OBJTOOL_ARGS", "OBJTOOL_VERBOSE"]:
        post = postprocess_commands(post_setup, "target.o", {"native_tail": {"value": object_command}}, ["native_tail"], dependencies, {name: "--unknown"})
        asserts.false(env, post.object_only)
    exported_setup = struct(sources = {}, program = post_setup.program, tools = {"OBJTOOL_ARGS": "--unknown"})
    post = postprocess_commands(exported_setup, "target.o", {"native_tail": {"value": object_command}}, ["native_tail"], dependencies)
    asserts.false(env, post.object_only)
    captures = {"__shell_commands__": []}
    variables = program([
        _assignment("discard", _literal("")),
        _assignment("cmd_capture", [dict(op = "function", name = "call", arguments = [_literal("discard"), command])]),
    ], {}, captures).variables
    post = postprocess_commands(post_setup, "target.o", variables, ["cmd_capture"], {})
    asserts.equals(env, [], post.commands)
    asserts.equals(env, ["printf captured"], post.sources["__shell_commands__"])
    variables["native_tail"] = {"value": object_command}
    post = postprocess_commands(post_setup, "target.o", variables, ["cmd_capture", "native_tail"], dependencies)
    asserts.equals(env, [object_command], post.commands)
    asserts.false(env, post.object_only)
    return unittest.end(env)

probe_context_test = unittest.make(_context_test)
