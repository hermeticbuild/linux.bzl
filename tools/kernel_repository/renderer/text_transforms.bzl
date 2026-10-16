"""Evaluate exported text-only sed programs without executing a process."""

def sed_substitute(text, encoded):
    spec = json.decode(encoded)
    code = spec["code"]

    # Ordered backtracking implements greedy ERE repetition and captures.
    for start in range(len(text) + 1):
        pending = [(0, start, {})]
        seen = {}
        for _ in range(100000):
            if not pending:
                break
            pc, pos, captures = pending.pop()
            key = (pc, pos, str(captures))
            if key in seen:
                continue
            seen[key] = True
            op = code[pc]
            if op[0] == "accept":
                result, escaped = "", False
                for char in spec["replacement"].elems():
                    if escaped:
                        if char in "123456789":
                            group = int(char) * 2
                            result += text[captures[group]:captures[group + 1]] if group in captures and group + 1 in captures else ""
                        else:
                            result += char
                        escaped = False
                    elif char == "\\":
                        escaped = True
                    elif char == "&":
                        result += text[start:pos]
                    else:
                        result += char
                return text[:start] + result + text[pos:]
            if op[0] == "split":
                pending.append((op[2], pos, captures))
                pending.append((op[1], pos, captures))
            elif op[0] == "jump":
                pending.append((op[1], pos, captures))
            elif op[0] == "save":
                captures = dict(captures)
                captures[op[1]] = pos
                pending.append((pc + 1, pos, captures))
            elif pos < len(text) and ((op[0] == "char" and text[pos] == op[1]) or (op[0] == "class" and (text[pos] in op[2]) != op[1])):
                pending.append((pc + 1, pos + 1, captures))
        if pending:
            fail("Text transformation exceeded 100000 matching steps")
    return text
