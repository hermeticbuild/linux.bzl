"""Bounded, path-mapped argv transport for Bazel 9 action templates."""

visibility("//internal/...")

# SpawnAction.Builder.buildCommandLines drops ParamFileInfo for action templates
# in Bazel 9.1/9.2. Keep small actions unchanged and explicitly materialize large
# argument vectors, without flattening typed Files into un-mappable strings.
_COMMAND_BYTES = 64 * 1024
_CHUNK_BYTES = 60 * 1024
_MAX_ARGUMENTS = 65536

def _sum(values):
    result = 0
    for value in values:
        result += value
    return result

def template_arguments(template_ctx):
    """Collects Args operations without freezing path-mappable Files.

    Args:
        template_ctx: The active map_directory callback's TemplateContext.

    Returns:
        A narrow Args-compatible object with a finish method. Call finish with
        the consumer's private work tree and complete original input/tool sets.
    """
    chunks = [template_ctx.args()]
    size = [0]
    count = [0]

    def value_size(value):
        # Include argv pointers and generous room for the terminating NUL.
        if type(value) != "string":
            value = getattr(value, "executable", value)
            value = getattr(value, "path", str(value))
        return len(value) + 16

    def reserve(byte_count, argument_count):
        if byte_count > _CHUNK_BYTES:
            fail("mapped Linux argument exceeds the 60 KiB transport chunk limit")
        count[0] += argument_count
        if count[0] > _MAX_ARGUMENTS:
            fail("mapped Linux action exceeds the 65536 argument transport limit")
        if size[-1] + byte_count > _CHUNK_BYTES:
            chunks.append(template_ctx.args())
            size.append(0)
        size[-1] += byte_count
        return chunks[-1]

    def add(*values):
        reserve(_sum([value_size(value) for value in values]), len(values)).add(*values)

    def add_all(values, expand_directories = False, format_each = None):
        if expand_directories:
            fail("mapped Linux argument transport requires explicit directory bindings")
        for value in values:
            reserve(value_size(value) + len(format_each or ""), 1).add_all(
                [value],
                expand_directories = False,
                format_each = format_each,
            )

    def add_joined(flag, values, expand_directories = False, join_with = ""):
        if expand_directories:
            fail("mapped Linux argument transport requires explicit directory bindings")
        byte_count = value_size(flag) + _sum([value_size(value) for value in values]) + len(join_with) * len(values)
        reserve(byte_count, 2).add_joined(flag, values, expand_directories = False, join_with = join_with)

    def finish(runner, directory, key, inputs, tools):
        if len(chunks) == 1:
            if size[0] + value_size(runner) > _COMMAND_BYTES:
                fail("mapped Linux command exceeds 64 KiB including its executable")
            return struct(arguments = chunks, inputs = [])
        private_prefix = directory.short_path + "/argument-chunks/"
        effective_inputs = inputs.to_list()
        for tool in tools + [runner]:
            if type(tool) == "depset":
                effective_inputs.extend(tool.to_list())
            elif type(tool) == "File":
                effective_inputs.append(tool)
            else:
                # Configured runtime runfiles are also carried in the toolset
                # closure depsets. FilesToRunProvider exposes only these three
                # artifacts; do not invent an accessor for its hidden closure.
                for field in ["executable", "runfiles_manifest", "repo_mapping_manifest"]:
                    artifact = getattr(tool, field, None)
                    if artifact != None:
                        effective_inputs.append(artifact)
        for artifact in effective_inputs:
            if artifact.short_path.startswith(private_prefix):
                fail("mapped Linux argument chunks overlap a consumer input: %s" % artifact)
        files = []
        for ordinal, chunk in enumerate(chunks):
            output = template_ctx.declare_file("argument-chunks/%s/%s.json" % (key, ("00000000" + str(ordinal))[-8:]), directory = directory)
            if size[ordinal] + value_size(output) + value_size(runner) + 128 > _COMMAND_BYTES:
                fail("mapped Linux argument chunk command exceeds 64 KiB including its header")
            header = template_ctx.args()
            header.add("-write_argument_chunk", output)
            header.add("--")
            template_ctx.run(
                executable = runner,
                # Path mapping is action-local: a cross-configuration input
                # collision can disable it. Writers must see the same complete
                # input/tool closure as their consumer, even though they only
                # serialize names. The added chunks live in this callback's
                # private work tree and cannot alias an original input.
                inputs = inputs,
                tools = tools,
                outputs = [output],
                arguments = [header, chunk],
                progress_message = "Writing Linux argument chunk %s/%d" % (key[:12], ordinal),
            )
            files.append(output)
        if _sum([value_size(file) for file in files]) + value_size(runner) + 64 > _COMMAND_BYTES:
            fail("mapped Linux argument chunk index exceeds 64 KiB")
        args = template_ctx.args()
        args.add("-argument_chunks")
        args.add_all(files, expand_directories = False)
        return struct(arguments = [args], inputs = files)

    return struct(add = add, add_all = add_all, add_joined = add_joined, finish = finish)
