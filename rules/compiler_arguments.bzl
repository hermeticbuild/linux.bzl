"""Retain artifact identity while rendering native compiler arguments.

The command owner supplies literal arguments or a parts list containing literals
and {file: logical path, parent: ancestor count} records. The scheduler resolves
those records to Files. Path access happens in Args.map_each, where Bazel applies
the action's PathMapper, including to directory and prefix-map operands.
"""

def _literal(value, forbidden):
    if any([prefix and prefix in value for prefix in forbidden]):
        fail("Compiler argument contains an unresolved artifact path: " + value)
    return value

def _render_argument(parts):
    result = []
    for part in parts:
        if type(part) == "string":
            result.append(part)
            continue
        path = part.file.path
        for _ in range(part.parent):
            path = path.rpartition("/")[0]
            if not path:
                fail("Compiler path operand escapes the execution root: " + part.file.path)
        result.append(path)
    return "".join(result)

def compiler_arguments(tctx, arguments, files, outputs, forbidden_prefixes):
    """Resolve operands from shared available Files and this action's outputs."""
    forbidden = ["bazel-out/", "__object__", "__source__"] + forbidden_prefixes
    args = tctx.args()
    for argument in arguments:
        if type(argument) == "string":
            args.add(_literal(argument, forbidden))
            continue
        if type(argument) != "dict" or argument.keys() != ["parts"] or type(argument["parts"]) != "list":
            fail("Expected a literal compiler argument or a parts record: " + str(argument))
        parts = []
        for part in argument["parts"]:
            if type(part) == "string":
                parts.append(_literal(part, forbidden))
            elif type(part) == "dict" and sorted(part.keys()) == ["file", "parent"] and type(part["file"]) == "string" and type(part["parent"]) == "int" and part["parent"] >= 0:
                path = part["file"]
                file = outputs[path] if path in outputs else files.get(path)
                if file == None:
                    fail("Compiler path operand has no declared artifact: " + path)
                parts.append(struct(file = file, parent = part["parent"]))
            else:
                fail("Invalid compiler path operand: " + str(part))
        args.add_all([parts], map_each = _render_argument, expand_directories = False)
    return args
