"""Preserve native absolute utility references as execution-time tool lookups."""
import re

_PATH = re.compile(r'/(?:usr/)?bin/([A-Za-z0-9_.+-]+)(?=$|[\s;&|()])')


def _terms(terms):
    output = []
    quote, escaped = '', False
    for term in terms:
        if term.get('op') != 'literal':
            output.append(term)
            continue
        value, start, i = term['value'], 0, 0
        while i < len(value):
            char = value[i]
            if escaped:
                escaped = False
            elif char == '\\' and quote != "'":
                escaped = True
            elif quote:
                if char == quote:
                    quote = ''
            elif char in "'\"":
                quote = char
            else:
                match = _PATH.match(value, i)
                if match and (i == 0 or value[i - 1] in ' \t\n;&|('):
                    if i > start:
                        output.append({'op': 'literal', 'value': value[start:i]})
                    output.append({'op': 'function', 'name': 'kbuild_execution_tool',
                                   'arguments': [[{'op': 'literal', 'value': match[1]}]]})
                    i = start = match.end()
                    continue
            i += 1
        if start < len(value):
            output.append({'op': 'literal', 'value': value[start:]})
    return output


def lower_execution_paths(value):
    if isinstance(value, list):
        return [lower_execution_paths(v) for v in value]
    if not isinstance(value, dict):
        return value
    result = {k: lower_execution_paths(v) for k, v in value.items()}
    if result.get('kind') == 'recipe' or (result.get('kind') in ('assignment', 'define', 'target_assignment')
                                         and result.get('name', '').startswith(('cmd_', 'filechk_'))):
        if 'value_expression' in result:
            result['value_expression'] = _terms(result['value_expression'])
    return result
