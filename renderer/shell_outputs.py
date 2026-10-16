"""Extract simple output redirection operands from native Make expression nodes.

This handles Kbuild post-link command templates, whose extra products are shell
side effects rather than Make rule targets. Make expressions remain untouched.
"""
import re


def output_redirections(terms):
    outputs, current = [], None
    for term in terms:
        if term['op'] != 'literal':
            if current is not None:
                current.append(term)
            continue
        text = term['value']
        # Post-link templates use unquoted >word operands. More complex shell
        # constructs need an explicit adapter, rather than guessed outputs.
        for token in re.split(r'(\s+|;|\|\||&&|\||>>|>)', text):
            if not token:
                continue
            if token in ('>', '>>'):
                if current:
                    outputs.append(current)
                current = []
            elif token.isspace() or token in (';', '||', '&&', '|'):
                if current:
                    outputs.append(current)
                    current = None
            elif current is not None:
                if any(c in token for c in "'\"`<"):
                    raise ValueError('Unsupported quoted post-link output: ' + token)
                current.append({'op': 'literal', 'value': token})
    if current:
        outputs.append(current)
    return outputs
