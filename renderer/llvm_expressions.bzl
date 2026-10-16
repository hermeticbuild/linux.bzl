"""Bounded interpreter for native Make expression nodes during analysis.

There is no Make lexer here. GNU Make exports every operation/argument boundary.
External processes and generated-file reads require a declared action, and are
rejected rather than run during analysis.
"""

def _words(value):
    return [w for w in value.replace("\t", " ").replace("\n", " ").replace("\r", " ").split(" ") if w]

def _match(pattern, word):
    if "%" not in pattern:
        return "" if pattern == word else None
    before, after = pattern.split("%", 1)
    if word.startswith(before) and word.endswith(after) and len(word) >= len(before) + len(after):
        return word[len(before):len(word) - len(after) if after else len(word)]
    return None

def _glob(pattern, word):
    # Shell wildcard semantics within a pathname component; '/' is never '*'.
    if len(pattern.split("/")) != len(word.split("/")) or any([w.startswith(".") and not p.startswith(".") for p, w in zip(pattern.split("/"), word.split("/"))]):
        return False
    previous = [True] + [False] * len(word)
    for char in pattern.elems():
        row = [previous[0] if char == "*" else False]
        for i in range(len(word)):
            row.append((previous[i + 1] or (row[i] and word[i] != "/")) if char == "*" else previous[i] and (char == word[i] or (char == "?" and word[i] != "/")))
        previous = row
    return previous[-1]

def _builtin(name, a, sources):
    if name == "kbuild_min_version":
        return "y" if int(a[0] or "0") >= int(a[1].strip()) else ""
    if name == "error":
        fail("Kbuild: " + " ".join(a))
    if name in ("warning", "info"):
        return ""

    if name == "strip":
        return " ".join(_words(a[0]))
    if name == "subst":
        return a[2].replace(a[0], a[1])
    if name == "patsubst":
        result = []
        for word in _words(a[2]):
            stem = _match(a[0], word)
            result.append(a[1].replace("%", stem, 1) if stem != None else word)
        return " ".join(result)
    if name in ("filter", "filter-out"):
        return " ".join([w for w in _words(a[1]) if any([_match(p, w) != None for p in _words(a[0])]) == (name == "filter")])
    if name == "findstring":
        return a[0] if a[0] in a[1] else ""
    if name == "sort":
        return " ".join(sorted({w: True for w in _words(a[0])}))
    if name in ("addprefix", "addsuffix"):
        return " ".join([a[0] + w if name == "addprefix" else w + a[0] for w in _words(a[1])])
    if name in ("basename", "suffix", "dir", "notdir"):
        words = []
        for word in _words(a[0]):
            head, sep, tail = word.rpartition("/")
            base, dot, extension = tail.rpartition(".")
            words.append((head + "/" if sep else "./") if name == "dir" else tail if name == "notdir" else ("." + extension if dot else "") if name == "suffix" else ((head + "/" if sep else "") + base if dot else word))
        return " ".join([w for w in words if w])
    if name == "words":
        return str(len(_words(a[0])))
    if name in ("word", "wordlist", "firstword", "lastword"):
        words = _words(a[-1])
        if name == "firstword":
            return words[0] if words else ""
        if name == "lastword":
            return words[-1] if words else ""
        start = int(a[0].strip())
        if start < 1:
            fail("Make word index must be positive")
        return " ".join(words[start - 1:int(a[1].strip())]) if name == "wordlist" else (words[start - 1] if start <= len(words) else "")
    if name == "join":
        left, right = _words(a[0]), _words(a[1])
        return " ".join([(left[i] if i < len(left) else "") + (right[i] if i < len(right) else "") for i in range(max(len(left), len(right)))])
    if name in ("abspath", "realpath"):
        fail("%s needs an explicit path namespace; use source-relative prerequisites" % name)
    if name == "wildcard":
        result = []
        for pattern in _words(a[0]):
            if "[" in pattern:
                fail("Bracket wildcard patterns are not supported: " + pattern)
            prefix = pattern.split("*", 1)[0].split("?", 1)[0]
            if "*" not in pattern and "?" not in pattern:
                result.extend([pattern] if pattern in sources else [])
            else:
                # Prefix index reduces repeated full-tree scans for common globs.
                directory = prefix.rpartition("/")[0]
                candidates = sources.get(directory + "/", [])
                result.extend(sorted([p for p in candidates if p.startswith(prefix) and _glob(pattern, p)]))
        return " ".join(result)
    fail("Make operation %r cannot be evaluated during analysis; it needs explicit lowering: %r" % (name, a))

def evaluate(terms, variables, sources = {}):
    """Evaluate with an explicit stack: no Starlark recursion or Python fallback."""
    tasks = [("expr", terms, variables, [])]
    values = []
    for _ in range(100000):
        if not tasks:
            return values[0]
        task = tasks.pop()
        op, data, scope, chain = task
        if op == "expr":
            tasks.append(("concat", len(data), scope, chain))
            for term in reversed(data):
                tasks.append(("term", term, scope, chain))
        elif op in ("concat", "join", "function"):
            count = data if op != "function" else data[1]
            args = values[len(values) - count:] if count else []
            if count:
                values = values[:len(values) - count]
            values.append(("" if op == "concat" else " ").join(args) if op != "function" else _builtin(data[0], args, sources))
        elif op == "term":
            kind = data["op"]
            if kind == "literal":
                values.append(data["value"])
            elif kind == "config":
                tasks.append(("lookup", data["name"], scope, chain))
            elif kind == "reference":
                tasks.extend([("lookup_name", None, scope, chain), ("expr", data["name"], scope, chain)])
            elif kind == "function":
                name, arguments = data["name"], data["arguments"]
                if name == "kbuild_probe":
                    probe = arguments[0][0]["value"]

                    # try-run contains an arbitrary shell program, not a flag.
                    # Keep it opaque rather than expanding subprocesses in it.
                    if probe == "try-run":
                        values.append(json.encode(arguments[1]))
                        tasks.append(("probe", (probe, arguments), scope, chain))
                    else:
                        tasks.extend([("probe", (probe, arguments), scope, chain), ("expr", arguments[1], scope, chain)])
                elif name == "intcmp":
                    tasks.extend([("intcmp", arguments, scope, chain), ("expr", arguments[1], scope, chain), ("expr", arguments[0], scope, chain)])
                elif name == "origin":
                    tasks.extend([("origin", None, scope, chain), ("expr", arguments[0], scope, chain)])
                elif name == "if":
                    tasks.extend([("if", arguments, scope, chain), ("expr", arguments[0], scope, chain)])
                elif name in ("or", "and"):
                    tasks.extend([("logic", (name, arguments, 0), scope, chain), ("expr", arguments[0], scope, chain)])
                elif name == "foreach":
                    tasks.extend([("foreach", arguments[2], scope, chain), ("expr", arguments[1], scope, chain), ("expr", arguments[0], scope, chain)])
                elif name == "call":
                    tasks.append(("call", len(arguments), scope, chain))
                    for arg in reversed(arguments):
                        tasks.append(("expr", arg, scope, chain))
                else:
                    tasks.append(("function", (name, len(arguments)), scope, chain))
                    for arg in reversed(arguments):
                        tasks.append(("expr", arg, scope, chain))
            else:
                fail("Unknown native expression node " + kind)
        elif op == "probe":
            probe, arguments = data
            option = values.pop().strip()
            key = probe + ":" + option
            supported = sources.get("__capabilities__", {}).get(key)
            if supported == None:
                # buildifier: disable=print
                print("WARNING: unresolved Kbuild compiler capability %r; using the fallback branch" % key)
                supported = False
            if probe in ("cc-option-yn", "rustc-option-yn"):
                values.append("y" if supported else "n")
            elif probe == "cc-disable-warning":
                values.append("-Wno-" + option if supported else "")
            elif probe in ("as-instr", "try-run"):
                index = 2 if supported else 3
                tasks.append(("expr", arguments[index] if len(arguments) > index else [], scope, chain))
            elif supported:
                values.append(option)
            else:
                tasks.append(("expr", arguments[2] if len(arguments) > 2 else [], scope, chain))
        elif op == "origin":
            name = values.pop()
            values.append("file" if name in scope else "undefined")
        elif op == "intcmp":
            left, right = int(values.pop(-2).strip()), int(values.pop().strip())
            index = 2 if left < right else 3 if left == right else 4
            tasks.append(("expr", data[index] if index < len(data) else (data[3] if len(data) > 3 and left > right else []), scope, chain))
        elif op == "lookup_name":
            tasks.append(("lookup", values.pop(), scope, chain))
        elif op == "lookup":
            if ":" in data and "=" in data:
                name, substitution = data.split(":", 1)
                before, after = substitution.split("=", 1)
                tasks.extend([("substitute", (before, after), scope, chain), ("lookup", name, scope, chain)])
            elif data in chain:
                fail("Recursive Make variable: " + " -> ".join(chain + [data]))
            else:
                cell = scope.get(data, {"value": scope.get("__config__", {}).get(data, "")})
                if "value" in cell:
                    values.append(cell["value"])
                elif "append" in cell:
                    tasks.append(("cell", cell, scope, []))
                elif "parts" in cell:
                    tasks.append(("concat", len(cell["parts"]), scope, chain))
                    for part in reversed(cell["parts"]):
                        tasks.append(("cell", part, scope, chain + [data]))
                else:
                    tasks.append(("expr", cell["expr"], cell.get("scope", scope), [] if "scope" in cell else chain + [data]))
        elif op == "cell":
            if "value" in data:
                values.append(data["value"])
            elif "append" in data:
                tasks.extend([
                    ("append_result", None, scope, chain),
                    ("cell", data["append"][1], scope, chain),
                    ("cell", data["append"][0], scope, chain),
                ])
            elif "parts" in data:
                tasks.append(("concat", len(data["parts"]), scope, chain))
                for part in reversed(data["parts"]):
                    tasks.append(("cell", part, scope, chain))
            else:
                tasks.append(("expr", data["expr"], data.get("scope", scope), [] if "scope" in data else chain))
        elif op == "append_result":
            new, old = values.pop(), values.pop()
            values.append(old + (" " if old else "") + new)
        elif op == "substitute":
            before, after = data
            values.append(_builtin("patsubst", [
                before if "%" in before else "%" + before,
                after if "%" in before else "%" + after,
                values.pop(),
            ], sources))
        elif op == "if":
            condition = values.pop().strip()
            tasks.append(("expr", data[1] if condition else data[2] if len(data) > 2 else [], scope, chain))
        elif op == "logic":
            name, arguments, index = data
            value = values.pop()
            if (name == "or" and value.strip()) or (name == "and" and not value.strip()) or index + 1 == len(arguments):
                values.append(value)
            else:
                tasks.extend([("logic", (name, arguments, index + 1), scope, chain), ("expr", arguments[index + 1], scope, chain)])
        elif op == "foreach":
            words, name = _words(values.pop()), values.pop().strip()
            tasks.append(("join", len(words), scope, chain))
            for word in reversed(words):
                local = dict(scope)
                local[name] = {"value": word}
                tasks.append(("expr", data, local, chain))
        elif op == "call":
            args = values[len(values) - data:]
            values = values[:len(values) - data]
            local = dict(scope)
            for i, value in enumerate(args):
                local[str(i)] = {"value": value}
            tasks.append(("lookup", args[0].strip(), local, chain))
    fail("Make expression exceeded 100000 evaluation steps")

def source_index(paths):
    result = {p: True for p in paths}
    for path in paths:
        parts = path.split("/")
        for i in range(1, len(parts)):
            directory = "/".join(parts[:i]) + "/"
            if directory not in result:
                result[directory] = []
            result[directory].append(path)
    return result

def _condition(branch, variables, sources):
    condition = branch.get("condition")
    if condition == None:
        if branch["directive"] == "else" and not branch["arguments"]:
            return True
        fail("Missing native conditional operands at %s:%s; regenerate the AST" % (branch.get("file"), branch["line"]))
    args = [evaluate(a, variables, sources) for a in condition["operands"]]
    directive = condition["directive"]
    if directive in ("ifdef", "ifndef"):
        # Native ifdef tests the stored value, without recursively expanding it.
        cell = variables.get(args[0].strip(), {"value": variables.get("__config__", {}).get(args[0].strip(), "")})
        if "scope" in cell or "append" in cell or "parts" in cell:
            defined = bool(evaluate([{"op": "reference", "name": [{"op": "literal", "value": args[0].strip()}]}], variables, sources))
        else:
            defined = bool(cell.get("value", cell.get("raw", "")))
        return defined == (directive == "ifdef")
    return (args[0] == args[1]) == (directive == "ifeq")

def program(statements, initial, sources, cells = False):
    """Evaluate assignment/conditional order and produce configured rule edges."""
    variables = dict(initial) if cells else {name: {"value": value} for name, value in initial.items() if name != "__config__"}
    variables["__config__"] = initial.get("__config__", {})
    variables["__exports__"] = dict(initial.get("__exports__", {})) if cells else {}
    work = list(reversed(statements))
    rules = []
    current = None
    for _ in range(100000):
        if not work:
            return struct(variables = variables, rules = rules)
        node = work.pop()
        kind = node["kind"]
        if kind == "conditional":
            for branch in node["branches"]:
                if _condition(branch, variables, sources):
                    work.extend(reversed(branch["body"]))
                    break
        elif kind == "flag_includes":
            paths = _words(evaluate([{"op": "reference", "name": [{"op": "literal", "value": "include-y"}]}], variables, sources))
            for path in reversed(paths):
                if path not in node["files"]:
                    fail("Flag include was not exported: " + path)
                work.extend(reversed(node["files"][path]))
        elif kind == "expression":
            if "value_expression" not in node:
                fail("Standalone Make expression lacks native expression metadata")
            result = evaluate(node["value_expression"], variables, sources).strip()
            if result:
                fail("Standalone Make expression produced syntax requiring a native parse: " + result)
        elif kind in ("assignment", "define"):
            name = evaluate(node["name_expression"], variables, sources)
            if node.get("export") == 1:
                variables["__exports__"][name] = True
            elif node.get("export") == 2:
                variables["__exports__"].pop(name, None)
            expr, flavor = node["value_expression"], node["flavor"]
            old = variables.get(name)
            if flavor == "conditional" and old != None:
                continue
            if flavor == "shell":
                expr = [{"op": "function", "name": "shell", "arguments": [expr]}]
            if flavor == "append" and old != None:
                if "value" in old or "scope" in old or "parts" in old or "append" in old:
                    variables[name] = {"append": [old, {"expr": expr, "scope": dict(variables)}], "raw": "", "simple": True}
                else:
                    variables[name] = {
                        "expr": old["expr"] + ([{"op": "literal", "value": " "}] if old["raw"] else []) + expr,
                        "raw": old["raw"] + (" " if old["raw"] else "") + node["value"],
                    }
            elif flavor in ("simple", "expand"):
                variables[name] = {"expr": expr, "raw": node["value"], "scope": dict(variables)}
            else:
                variables[name] = {"expr": expr, "raw": node["value"]}
        elif kind == "rule":
            current = {
                "targets": _words(evaluate(node["targets_expression"], variables, sources)),
                "prerequisites": evaluate(node["tail_expression"], variables, sources),
                "grouped": node["separator"] in ("&:", "&::"),
                "recipe": False,
            }
            rules.append(current)
        elif kind == "recipe" and current != None:
            current["recipe"] = True
        elif kind == "undefine":
            variables.pop(node["name"], None)
        elif kind == "target_assignment":
            pass  # Command-specific attributes are retained for command lowering.
        elif kind == "directive" and node["directive"] in ("export", "unexport"):
            names = _words(evaluate(node.get("arguments_expression", [{"op": "literal", "value": node["arguments"]}]), variables, sources))
            if not names:
                fail("Make export-all requires explicit lowering")
            for name in names:
                if node["directive"] == "export":
                    variables["__exports__"][name] = True
                else:
                    variables["__exports__"].pop(name, None)
        elif kind == "directive" and node["directive"] == "vpath":
            pass
        elif kind in ("directive", "expression"):
            fail("Dependency program needs explicit lowering for %s at line %s" % (kind, node["line"]))
    fail("Make program exceeded 100000 statements")

match = _match

make_words = _words

def expand_eval_templates(nodes, scope, sources):
    """Complete eval's first expansion before running its parsed statements."""
    result = []
    pending = [(node, scope) for node in reversed(nodes)]
    for _node in range(100000):
        if not pending:
            return result
        node, current = pending.pop()
        if node["kind"] == "eval_foreach":
            name = evaluate(node["name_expression"], current, sources).strip()
            words = _words(evaluate(node["words_expression"], current, sources))
            for word in reversed(words):
                local = dict(current)
                local[name] = {"value": word}
                pending.extend([(child, local) for child in reversed(node["body"])])
        elif node["kind"] == "eval_template":
            bindings = {name: evaluate(expression, current, sources) for name, expression in node["inputs"].items()}
            if any(["$(" in text or "${" in text or "\n" in text for text in bindings.values()]):
                fail("Eval expansion introduced dynamic Make syntax requiring a native parse")
            result.extend(instantiate_eval_template(node["body"], bindings))
        else:
            fail("Unknown native eval template node: " + node["kind"])
    fail("Native eval expansion exceeded 100000 templates")

def instantiate_eval_template(body, bindings):
    """Freeze eval's first expansion in a natively parsed second-pass AST."""
    result = [None]
    pending = [(body, result, 0)]
    for _node in range(100000):
        if not pending:
            return result[0]
        value, parent, key = pending.pop()
        if type(value) == "dict":
            name = value.get("name", [])
            if value.get("op") == "reference" and type(name) == "list" and len(name) == 1 and name[0].get("op") == "literal" and name[0]["value"] in bindings:
                parent[key] = {"op": "literal", "value": bindings[name[0]["value"]]}
            else:
                copy = {}
                parent[key] = copy
                pending.extend([(child, copy, field) for field, child in value.items()])
        elif type(value) == "list":
            copy = [None] * len(value)
            parent[key] = copy
            pending.extend([(child, copy, i) for i, child in enumerate(value)])
        elif type(value) == "string":
            for name, text in bindings.items():
                value = value.replace("$(" + name + ")", text)
            parent[key] = value
        else:
            parent[key] = value
    fail("Native eval template exceeded 100000 syntax nodes")

def _probe_impl(ctx):
    sources = source_index(ctx.attr.paths)
    result = program(json.decode(ctx.attr.statements), ctx.attr.variables, sources)
    observed = {n: evaluate([{"op": "reference", "name": [{"op": "literal", "value": n}]}], result.variables, sources) for n in ctx.attr.observe}
    output = ctx.actions.declare_file(ctx.label.name + ".json")
    ctx.actions.write(output, json.encode({"values": observed, "rules": result.rules}))
    return [DefaultInfo(files = depset([output]))]

_probe = rule(implementation = _probe_impl, attrs = {
    "statements": attr.string(mandatory = True),
    "variables": attr.string_dict(),
    "paths": attr.string_list(),
    "observe": attr.string_list(),
})

def kbuild_expression_probe(name, statements, **kwargs):
    _probe(name = name, statements = json.encode(statements), **kwargs)
