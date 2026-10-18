"""Evaluate compiled, dependency-ordered Kconfig data without subprocesses."""

_TRI = {"n": 0, "m": 1, "y": 2}
_VALUES = ["n", "m", "y"]

def _number(text, base = 0):
    sign = -1 if text.startswith("-") else 1
    digits = text[1:] if text.startswith(("-", "+")) else text
    if base == 0:
        base = 16 if digits.lower().startswith("0x") else 8 if len(digits) > 1 and digits.startswith("0") else 10
    if base == 16 and digits.lower().startswith("0x"):
        digits = digits[2:]
    if not digits:
        return None
    result = 0
    for char in digits.lower().elems():
        digit = "0123456789abcdef".find(char)
        if digit < 0 or digit >= base:
            return None
        result = result * base + digit
    return result * sign

def _numeric(value):
    text, kind = value
    if kind in ("bool", "tristate"):
        return _TRI.get(text, -1)
    number = _number(text, 10 if kind in ("int", "integer") else 16 if kind == "hex" else 0)
    if number == None:
        return None
    if kind == "hex":
        return number % 18446744073709551616 if number >= -9223372036854775808 and number <= 18446744073709551615 else None
    return number if number >= -9223372036854775808 and number <= 9223372036854775807 else None

def _tri(value):
    return _TRI.get(value[0], 0) if value[1] in ("bool", "tristate") else 0

def _eval(code, values, types):
    stack = []
    for token in code:
        op = token[0]
        if op == "end":
            continue
        if op == "lit":
            stack.append((token[1], token[2]))
        elif op == "sym":
            stack.append((values.get(token[1], token[1]), types.get(token[1], "unknown")))
        elif op == "not":
            stack.append((_VALUES[2 - _tri(stack.pop())], "tristate"))
        else:
            right = stack.pop()
            left = stack.pop()
            if op in ("and", "or"):
                value = min(_tri(left), _tri(right)) if op == "and" else max(_tri(left), _tri(right))
                stack.append((_VALUES[value], "tristate"))
            elif op == "range":
                stack.append(([left, right], "range"))
            else:
                a, b = _numeric(left), _numeric(right)
                if (left[1] == "string" and right[1] == "string") or a == None or b == None:
                    a, b = left[0], right[0]
                elif left[1] == "hex" or right[1] == "hex":
                    a, b = a % 18446744073709551616, b % 18446744073709551616
                result = a == b if op == "eq" else a != b if op == "ne" else a < b if op == "lt" else a <= b if op == "le" else a > b if op == "gt" else a >= b if op == "ge" else None
                if result == None:
                    fail("Unknown compiled expression opcode: " + op)
                stack.append(("y" if result else "n", "tristate"))
    if len(stack) != 1:
        fail("Invalid compiled Kconfig expression: %s" % code)
    return stack[0]

def _condition(code, state):
    return _tri(_eval(code, state.values, state.types))

def _props(records, kind):
    return [p for r in records for p in r["properties"] if p["kind"] == kind]

def _visible(name, state):
    records = state.symbols[name]
    if records[0]["transitional"]:
        return 2
    result = 0
    for p in _props(records, "prompt"):
        result = max(result, _condition(p["when"], state))
    if result == 1 and (state.types[name] != "tristate" or state.values.get(state.modules, "n") == "n"):
        result = 2
    return result

def _first_property(records, kind, state):
    for p in _props(records, kind):
        if _condition(p["when"], state):
            return p
    return None

def _resolve_symbol(name, state):
    records = state.symbols[name]
    r = records[0]
    kind = state.types[name]
    boolean = kind == "bool" or (kind == "tristate" and state.values.get(state.modules, "n") == "n")
    visible = _visible(name, state)
    writable = visible != 0
    requested = state.requested.get(name)
    default = _first_property(records, "default", state)
    if kind in ("bool", "tristate"):
        reverse = _condition(r["reverse"], state)
        implied = _condition(r["implied"], state)
        direct = _condition(r["direct"], state)
        if boolean:
            reverse = 2 if reverse == 1 else reverse
            implied = 2 if implied == 1 else implied
            direct = 2 if direct == 1 else direct
        value = 0
        if visible and requested != None:
            value = min(_TRI[requested], visible)
        else:
            if default:
                value = min(_condition(default["value"], state), _condition(default["when"], state))
                writable = writable or value != 0
            if implied:
                writable = True
                value = min(max(value, implied), direct)
            writable = writable or reverse != 0
        value = max(value, reverse)
        if boolean and value == 1:
            value = 2
        result = _VALUES[value]
    else:
        result = "" if kind == "string" else "0x0" if kind == "hex" else "0"
        if visible and requested != None:
            result = json.decode(requested) if kind == "string" else requested
        elif default:
            result = _eval(default["value"], state.values, state.types)[0]
            writable = True
        constraint = _first_property(records, "range", state)
        if constraint:
            bounds = _eval(constraint["value"], state.values, state.types)[0]
            base = 16 if kind == "hex" else 10
            number = _number(result, base)
            low = _number(bounds[0][0], 16 if bounds[0][1] == "hex" else 10 if bounds[0][1] in ("int", "integer") else base)
            high = _number(bounds[1][0], 16 if bounds[1][1] == "hex" else 10 if bounds[1][1] in ("int", "integer") else base)
            number = number if number != None else 0
            low = low if low != None else 0
            high = high if high != None else 0
            if number < low:
                result = bounds[0][0]
            elif number > high:
                result = bounds[1][0]
    state.values[name] = result
    state.written[name] = writable and not r["transitional"]

def _resolve_choice(records, state):
    choices = [r for r in records if r["choice"]]
    members = list({r["symbol"]: True for r in records if not r["choice"]})
    visible = {name: _visible(name, state) != 0 for name in members}
    selected = None

    # Last user assignment has highest priority, as in conf_read_simple().
    for name in state.order:
        if name in visible and visible[name] and state.requested[name] == "y":
            selected = name
    if selected == None:
        for p in _props(choices, "default"):
            candidate = p["value"][0][1]
            if _condition(p["when"], state) and visible.get(candidate, False):
                selected = candidate
                break
        if selected != None and state.requested.get(selected) == "n":
            selected = None
    if selected == None:
        for name in members:
            if visible[name] and name not in state.requested:
                selected = name
                break
    if selected == None:
        for name in state.order:
            if name in visible and visible[name]:
                selected = name
                break
    for name in members:
        state.values[name] = "y" if name == selected else "n"
        state.written[name] = visible[name]

def resolve_options(records, requested, request_order):
    """Return all values and the subset Linux would write to .config."""
    symbols, units, ranks, types, values, written = {}, {}, {}, {}, {}, {}
    modules = ""
    for r in sorted(records, key = _record_order):
        units.setdefault(r["unit"], []).append(r)
        ranks[r["evaluation_rank"]] = r["unit"]
        if r["choice"]:
            continue
        name, kind = r["symbol"], r["type"]
        symbols.setdefault(name, []).append(r)
        types[name] = kind
        values[name] = "n" if kind in ("bool", "tristate") else "" if kind == "string" else "0x0" if kind == "hex" else "0"
        if r["modules"]:
            modules = name
    valid = {}
    for name, value in requested.items():
        if name not in symbols:
            continue
        kind = types[name]
        if kind in ("bool", "tristate"):
            if value not in (["n", "y"] if kind == "bool" else ["n", "m", "y"]):
                continue
        elif kind == "string":
            if not value.startswith('"') or not value.endswith('"'):
                continue
        elif _number(value, 16 if kind == "hex" else 10) == None:
            continue
        valid[name] = value
    state = struct(
        symbols = symbols,
        types = types,
        values = values,
        written = written,
        requested = valid,
        order = [n for n in request_order if n in valid],
        modules = modules,
    )
    for rank in sorted(ranks):
        group = units[ranks[rank]]
        if any([r["choice"] for r in group]):
            _resolve_choice(group, state)
        else:
            _resolve_symbol(group[0]["symbol"], state)
    output, headers, public = ["# Generated by Starlark kernel_config"], [], {}
    for name in sorted(symbols):
        value = values[name]
        public[name] = json.encode(value) if types[name] == "string" else value
        if not written.get(name, False):
            continue
        if types[name] in ("bool", "tristate"):
            output.append("# CONFIG_%s is not set" % name if value == "n" else "CONFIG_%s=%s" % (name, value))
            if value != "n":
                headers.append("#define CONFIG_%s%s 1" % (name, "_MODULE" if value == "m" else ""))
        else:
            output.append("CONFIG_%s=%s" % (name, public[name]))
            headers.append("#define CONFIG_%s %s" % (name, public[name]))
    return struct(values = public, config = "\n".join(output) + "\n", header = "\n".join(headers) + "\n")

def _record_order(record):
    return record["order"]
