"""Lower constant sed ERE programs, preserving symbolic Make input expressions."""
import json
import re
import shlex
from re import _parser as parser


def compile_ere(pattern):
    code = []
    def emit(nodes):
        for op, arg in nodes:
            name = str(op)
            if name == 'LITERAL':
                code.append(['char', chr(arg)])
            elif name == 'ANY':
                code.append(['class', True, ['\n']])
            elif name == 'NOT_LITERAL':
                code.append(['class', True, [chr(arg)]])
            elif name == 'IN':
                negated = bool(arg and str(arg[0][0]) == 'NEGATE')
                chars = []
                for kind, value in arg[int(negated):]:
                    if str(kind) == 'LITERAL':
                        chars.append(chr(value))
                    elif str(kind) == 'RANGE':
                        chars.extend(chr(n) for n in range(value[0], value[1] + 1))
                    else:
                        raise ValueError(f'Unsupported ERE character class: {kind}')
                code.append(['class', negated, chars])
            elif name == 'SUBPATTERN':
                group, add, remove, body = arg
                if add or remove:
                    raise ValueError('ERE inline flags are unsupported')
                code.append(['save', group * 2]); emit(body); code.append(['save', group * 2 + 1])
            elif name == 'BRANCH':
                jumps = []
                for branch in arg[1][:-1]:
                    split = len(code); code.append(['split', split + 1, 0])
                    emit(branch)
                    jumps.append(len(code)); code.append(['jump', 0])
                    code[split][2] = len(code)
                emit(arg[1][-1])
                for jump in jumps:
                    code[jump][1] = len(code)
            elif name == 'MAX_REPEAT':
                lo, hi, body = arg
                if lo != 0 or hi not in (1, parser.MAXREPEAT):
                    raise ValueError('Only ERE * and ? repetition currently supported')
                split = len(code); code.append(['split', split + 1, 0]); emit(body)
                if hi == parser.MAXREPEAT:
                    code.append(['jump', split])
                code[split][2] = len(code)
            else:
                raise ValueError(f'Unsupported ERE operation: {name}')
    emit(parser.parse(pattern, 0))
    code.append(['accept'])
    return code


def lower_text_transforms(value):
    if isinstance(value, list):
        return [lower_text_transforms(v) for v in value]
    if not isinstance(value, dict):
        return value
    value = {k: lower_text_transforms(v) for k, v in value.items()}
    if value.get('op') != 'function' or value.get('name') != 'shell':
        return value
    terms = value['arguments'][0]
    symbolic = {}
    text = ''
    for term in terms:
        if term['op'] == 'literal':
            text += term['value']
        else:
            key = f'__MAKE_TEXT_{len(symbolic)}__'
            symbolic[key] = term
            text += key
    lexer = shlex.shlex(text, posix=True, punctuation_chars='|;&<>')
    lexer.whitespace_split = True
    words = list(lexer)
    if len(words) < 6 or words[0] != 'echo' or words[-4:-1] != ['|', 'sed', '-E']:
        return value
    expression = words[-1]
    match = re.fullmatch(r's/([^/]+)/([^/]*)/', expression)
    if not match or any(w.startswith('-') or any(c in w for c in '$`;&<>|\\') for w in words[1:-4]):
        return value
    input_terms = []
    for i, word in enumerate(words[1:-4]):
        if i:
            input_terms.append({'op': 'literal', 'value': ' '})
        for part in re.split(r'(__MAKE_TEXT_\d+__)', word):
            if part:
                input_terms.append(symbolic[part] if part in symbolic else {'op': 'literal', 'value': part})
    program = {'code': compile_ere(match[1]), 'replacement': match[2]}
    return {'op': 'function', 'name': 'kbuild_sed', 'arguments': [input_terms, [{'op': 'literal', 'value': json.dumps(program)}]]}
