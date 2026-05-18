// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"fmt"
	"strings"
)

type token struct {
	value  string
	quoted bool
	pos    Position
}

func stripComment(line string) string {
	var quote rune
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if r == '\\' {
				escaped = true
				continue
			}
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == '#' {
			return line[:i]
		}
	}
	return line
}

func tokenize(stmt string, pos Position, pp *preprocessor) ([]token, error) {
	pp.setPosition(pos)
	var toks []token
	for i := 0; i < len(stmt); {
		if isSpace(stmt[i]) {
			i++
			continue
		}
		if stmt[i] == '"' || stmt[i] == '\'' {
			value, next, err := readQuoted(stmt, i)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", pos, err)
			}
			value, err = pp.expandString(value, nil)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{value: value, quoted: true, pos: pos})
			i = next
			continue
		}
		if op, ok := readOperator(stmt[i:]); ok {
			toks = append(toks, token{value: op, pos: pos})
			i += len(op)
			continue
		}
		word, next, err := readWord(stmt, i)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pos, err)
		}
		if word == "" {
			i++
			continue
		}
		word, err = pp.expandString(word, nil)
		if err != nil {
			return nil, err
		}
		if word != "" {
			toks = append(toks, token{value: word, pos: pos})
		}
		i = next
	}
	return toks, nil
}

func readQuoted(in string, start int) (string, int, error) {
	quote := in[start]
	var out strings.Builder
	for i := start + 1; i < len(in); i++ {
		if in[i] == '\\' {
			if i+1 < len(in) {
				out.WriteByte(in[i+1])
				i++
			}
			continue
		}
		if in[i] == quote {
			return out.String(), i + 1, nil
		}
		out.WriteByte(in[i])
	}
	return out.String(), len(in), fmt.Errorf("unterminated quoted string")
}

func readWord(in string, start int) (string, int, error) {
	var out strings.Builder
	for i := start; i < len(in); {
		if isSpace(in[i]) || startsOperator(in[i:]) || in[i] == '"' || in[i] == '\'' {
			return out.String(), i, nil
		}
		if in[i] == '$' && i+1 < len(in) && in[i+1] == '(' {
			end, err := matchingParen(in, i+1)
			if err != nil {
				return "", 0, err
			}
			out.WriteString(in[i : end+1])
			i = end + 1
			continue
		}
		out.WriteByte(in[i])
		i++
	}
	return out.String(), len(in), nil
}

func readOperator(in string) (string, bool) {
	for _, op := range []string{"||", "&&", "!=", "<=", ">=", ":=", "+=", "=", "<", ">", "!", "(", ")"} {
		if strings.HasPrefix(in, op) {
			return op, true
		}
	}
	return "", false
}

func startsOperator(in string) bool {
	_, ok := readOperator(in)
	return ok
}

func splitAssignment(stmt string) (name string, flavor variableFlavor, value string, ok bool) {
	i := 0
	for i < len(stmt) && isNameByte(stmt[i]) {
		i++
	}
	if i == 0 {
		return "", 0, "", false
	}
	name = stmt[:i]
	for i < len(stmt) && isSpace(stmt[i]) {
		i++
	}
	switch {
	case strings.HasPrefix(stmt[i:], ":="):
		flavor = varSimple
		i += 2
	case strings.HasPrefix(stmt[i:], "+="):
		flavor = varAppend
		i += 2
	case strings.HasPrefix(stmt[i:], "="):
		flavor = varRecursive
		i++
	default:
		return "", 0, "", false
	}
	return name, flavor, strings.TrimSpace(stmt[i:]), true
}

func isNameByte(b byte) bool {
	return b == '_' || b == '-' || ('0' <= b && b <= '9') || ('A' <= b && b <= 'Z') || ('a' <= b && b <= 'z')
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}
