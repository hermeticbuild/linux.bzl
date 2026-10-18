"""Link native file-local Kconfig syntax after platform selection in analysis."""

_Y = [["lit", "y", "tristate"]]
_N = [["lit", "n", "tristate"]]

def _code(code):
    return [t for t in code if t[0] != "end"]

def _join(a, b, op):
    identity, absorbing = (_Y, _N) if op == "and" else (_N, _Y)
    if a == identity:
        return b
    if b == identity or a == b:
        return a
    if a == absorbing or b == absorbing:
        return absorbing
    return a + b + [[op]]

def _rewrite(code, modules):
    # Linux rewrite_m() applies to dependencies/conditions, not default values.
    out = []
    code = _code(code)
    for i, token in enumerate(code):
        comparison_operand = any([j < len(code) and code[j][0] in ("eq", "ne", "lt", "le", "gt", "ge", "range") for j in [i + 1, i + 2]])
        if token[0] in ("sym", "lit") and token[1] == "m" and not comparison_operand:
            out.extend([token, ["sym", modules], ["and"]])
        else:
            out.append(token)
    return out

def _references(code):
    return {t[1]: True for t in code if t[0] == "sym" and t[1] not in ("y", "m", "n")}

def link_sources(files, architecture):
    """Follow symbolic source edges, propagate scopes, then order selected units."""
    modules = next_module(files)
    children = {}
    for path, nodes in files.items():
        groups = {}
        for node in nodes:
            groups.setdefault(node["parent"], []).append(node)
        children[path] = groups

    # Each frame carries the scope across a source edge as well as local menus.
    work = [("Kconfig", n, _Y, _Y, 0, ["Kconfig"]) for n in reversed(children["Kconfig"].get(0, []))]
    records = []
    for _ in range(200000):
        if not work:
            break
        path, node, inherited_dep, inherited_visibility, choice, chain = work.pop()
        dep = _join(inherited_dep, _rewrite(node["depends"], modules), "and")
        visibility = _join(inherited_visibility, _rewrite(node["visibility"], modules), "and")
        if node["kind"] == "source":
            target = node["path"].replace("__KCONFIG_SRCARCH", architecture.srcarch).replace("__KCONFIG_ARCH", architecture.arch).replace("__KCONFIG_HEADER_ARCH", architecture.header_arch).replace("__KCONFIG_SUBARCH", architecture.subarch)
            if target not in children:
                fail("Kconfig source not exported: %s from %s:%s" % (target, path, node["line"]))
            if target in chain:
                fail("Recursive Kconfig source: " + " -> ".join(chain + [target]))
            work.extend([(target, n, dep, visibility, choice, chain + [target]) for n in reversed(children[target].get(0, []))])
            continue
        if node["symbol"] or node["kind"] == "choice":
            order = len(records) + 1
            properties = []
            for p in node["properties"]:
                kind = "prompt" if p["kind"] == "menu" else p["kind"]
                when = _join(dep, _rewrite(p["when"], modules), "and")
                if kind == "prompt":
                    when = _join(when, inherited_visibility, "and")
                properties.append(dict(p, kind = kind, when = when, value = _code(p["value"])))
            is_choice = node["kind"] == "choice"
            records.append({
                "symbol": node["symbol"],
                "choice": is_choice,
                "order": order,
                "parent_choice": choice,
                "choice_group": order if is_choice else choice,
                "type": {"boolean": "bool", "integer": "int"}.get(node["type"], node["type"]),
                "depends_on": dep,
                "properties": properties,
                "direct": dep,
                "reverse": _N,
                "implied": _N,
                "modules": node["modules"],
                "transitional": node["transitional"],
                "source": path,
                "line": node["line"],
            })
            if is_choice:
                choice = order
        work.extend([(path, n, dep, visibility, choice, chain) for n in reversed(children[path].get(node["id"], []))])
    if work:
        fail("Kconfig source graph exceeded 200000 entries")
    symbols, choices = {}, {}
    for r in records:
        if r["choice"]:
            choices[r["order"]] = r
        else:
            symbols.setdefault(r["symbol"], []).append(r)
    direct, reverse, implied, kinds, units = {}, {}, {}, {}, {}
    for name, definitions in symbols.items():
        direct[name], reverse[name], implied[name] = _N, _N, _N
        kinds[name] = "unknown"
        for r in definitions:
            direct[name] = _join(direct[name], r["depends_on"], "or")
            if r["type"] != "unknown":
                kinds[name] = r["type"]
        group = definitions[0]["choice_group"]
        units[name] = "choice:" + str(group) if group else name
    for r in records:
        if r["choice"]:
            continue
        for p in r["properties"]:
            if p["kind"] not in ("select", "imply"):
                continue
            target = p["value"][0][1]
            mapping = reverse if p["kind"] == "select" else implied
            mapping[target] = _join(mapping.get(target, _N), _join([["sym", r["symbol"]]], p["when"], "and"), "or")
    edges = {}
    for r in records:
        unit = "choice:" + str(r["order"]) if r["choice"] else units[r["symbol"]]
        r["unit"] = unit
        dependencies = edges.setdefault(unit, {})
        if not r["choice"]:
            name = r["symbol"]
            r.update(type = kinds[name], direct = direct[name], reverse = reverse.get(name, _N), implied = implied.get(name, _N))
            for field in ("direct", "reverse", "implied"):
                dependencies.update(_references(r[field]))
        for p in r["properties"]:
            if p["kind"] in ("select", "imply"):
                continue
            dependencies.update(_references(p["when"]))
            if p["kind"] == "range" or (p["kind"] == "default" and not r["choice"]):
                dependencies.update(_references(p["value"]))
        if r["type"] == "tristate":
            dependencies[modules] = True
    normalized = {}
    for unit, refs in edges.items():
        normalized[unit] = {units.get(n, n): True for n in refs if units.get(n, n) != unit or not unit.startswith("choice:")}

    # Kahn's algorithm keeps the analysis cost linear in symbols plus edges.
    users, pending = {}, {}
    for unit, dependencies in normalized.items():
        pending[unit] = len(dependencies)
        for dependency in dependencies:
            users.setdefault(dependency, []).append(unit)
    for dependency in users:
        pending.setdefault(dependency, 0)
    ready = [unit for unit, count in pending.items() if count == 0]
    ranks = {}
    for _ in range(len(pending)):
        if not ready:
            break
        unit = ready.pop()
        ranks[unit] = len(ranks)
        for user in users.get(unit, []):
            pending[user] -= 1
            if pending[user] == 0:
                ready.append(user)
    if len(ranks) != len(pending):
        fail("Selected Kconfig dependency cycle: " + str({u: normalized.get(u, {}).keys() for u, count in pending.items() if count}))
    for r in records:
        r["evaluation_rank"] = ranks[r["unit"]]
    return records

def next_module(files):
    names = {n["symbol"]: True for body in files.values() for n in body if n["modules"]}
    if len(names) != 1:
        fail("Expected one Kconfig modules symbol, got " + str(names))
    return names.keys()[0]
