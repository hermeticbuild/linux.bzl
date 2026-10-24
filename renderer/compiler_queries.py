"""Preserve selected native queries for declared execution tools."""
import json
from text_transforms import compile_ere

def lower_host_queries(value):
    """Measure shell expressions in explicitly selected native query scopes."""
    if isinstance(value, list):
        return [lower_host_queries(v) for v in value]
    if isinstance(value, dict):
        if value.get('op') == 'function' and value.get('name') == 'shell':
            return dict(value, name='kbuild_host_shell')
        return {k: lower_host_queries(v) for k, v in value.items()}
    return value


def lower_initramfs_inputs(body):
    """Retain native initramfs input normalization as a measured expression."""
    result = []
    for node in body:
        if node['kind'] == 'conditional':
            node = dict(node, branches=[dict(branch, body=lower_initramfs_inputs(branch['body']))
                                       for branch in node['branches']])
        elif node['kind'] in ('assignment', 'define') and node.get('name') == 'ramfs-input':
            node = dict(node, value_expression=lower_host_queries(node['value_expression']))
        result.append(node)
    return result


def lower_compiler_queries(value):
    if isinstance(value, list):
        return [lower_compiler_queries(v) for v in value]
    if not isinstance(value, dict):
        return value
    value = {k: lower_compiler_queries(v) for k, v in value.items()}
    if value.get('op') == 'function' and value.get('name') == 'shell':
        terms = value['arguments'][0]
        if terms == [{'op': 'literal', 'value': 'uname -m'}]:
            return {'op': 'function', 'name': 'kbuild_host_machine', 'arguments': []}
        if terms == [{'op': 'literal', 'value': "uname -m | sed -e 's/i.86/i386/'"}]:
            return {'op': 'function', 'name': 'kbuild_sed', 'arguments': [
                [{'op': 'function', 'name': 'kbuild_host_machine', 'arguments': []}],
                [{'op': 'literal', 'value': json.dumps({'code': compile_ere('i.86'), 'replacement': 'i386'})}]]}
        if (len(terms) == 3 and terms[0] == {'op': 'literal', 'value': 'if '} and
                terms[1] == {'op': 'reference', 'name': [{'op': 'literal', 'value': 'NM'}]} and
                terms[2] == {'op': 'literal', 'value': " --help 2>&1 | grep -- '--synthetic' > /dev/null; then echo y; else echo n; fi"}):
            # A tool capability query follows the same conservative policy as
            # native compiler try-run helpers, retaining its original program.
            return {'op': 'function', 'name': 'kbuild_probe', 'arguments': [
                [{'op': 'literal', 'value': 'try-run'}], terms,
                [{'op': 'literal', 'value': 'y'}], [{'op': 'literal', 'value': 'n'}]]}
        if len(terms) == 2 and terms[0] == {'op': 'reference', 'name': [{'op': 'literal', 'value': 'CC'}]} and terms[1].get('op') == 'literal' and terms[1]['value'].strip() == '-print-file-name=include':
            return {'op': 'function', 'name': 'kbuild_compiler_include', 'arguments': []}
        if len(terms) == 2 and terms[0] == {'op': 'reference', 'name': [{'op': 'literal', 'value': 'CC'}]} and terms[1].get('op') == 'literal' and terms[1]['value'].strip() == '--version 2>&1 | head -n 1 | grep clang':
            # Older native setup guards CLANG_FLAGS with the selected
            # compiler's actual banner. Preserve its command and stdout.
            return dict(value, name='kbuild_host_shell')
    return value
