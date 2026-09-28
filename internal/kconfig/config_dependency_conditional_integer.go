package kconfig

import "strconv"

const (
	configDependencyConditionalIntegerMax       = int64(1<<63 - 1)
	configDependencyConditionalIntegerMin       = -configDependencyConditionalIntegerMax
	configDependencyConditionalIntegerMaxTokens = 4096
	configDependencyConditionalIntegerMaxBytes  = 65536
	configDependencyConditionalIntegerMaxDepth  = 128
	configDependencyConditionalIntegerMaxWork   = 16384
)

// configDependencyConditionalInteger evaluates an already fully expanded C
// preprocessing expression. The caller owns macro expansion, protection of
// original defined operands, CONFIG reads, and current namespace authority.
// Every surviving identifier requires an affirmative undefined lookup, even
// in a short-circuited arm. A surviving defined token is never an identifier.
//
// The supported numeric domain is the signed range [-INT64_MAX, INT64_MAX].
// C preprocessing uses intmax_t/uintmax_t representations even when converting
// integer tokens; the accepted signed literals and intermediate results are
// representable for every conforming intmax_t with at least 64 bits. Unsigned
// types, complement, negative bitwise operands and negative shifts are not
// guessed. This also avoids assuming two's-complement representation.
func configDependencyConditionalInteger(tokens []string, undefined func(string) bool) (configDependencyMacroDefinition, string) {
	value, reason := configDependencyConditionalIntegerValue(tokens, undefined)
	if reason != "" {
		return configDependencyMacroUnknown, reason
	}
	if value == 0 {
		return configDependencyMacroUndefined, ""
	}
	return configDependencyMacroDefined, ""
}

func configDependencyConditionalIntegerValue(tokens []string, undefined func(string) bool) (int64, string) {
	if len(tokens) == 0 || len(tokens) > configDependencyConditionalIntegerMaxTokens {
		return 0, "conditional integer token budget or empty expression"
	}
	bytes := 0
	for _, token := range tokens {
		if token == "" || len(token) > 128 || len(token) > configDependencyConditionalIntegerMaxBytes-bytes {
			return 0, "conditional integer token byte budget or empty token"
		}
		bytes += len(token)
	}
	p := configDependencyConditionalIntegerParser{tokens: tokens, undefined: undefined}
	value, reason := p.conditional(true, 0)
	if reason != "" {
		return 0, reason
	}
	if p.position != len(tokens) {
		return 0, "conditional integer expression has unconsumed tokens"
	}
	return value, ""
}

type configDependencyConditionalIntegerParser struct {
	tokens    []string
	position  int
	work      int
	undefined func(string) bool
}

func (p *configDependencyConditionalIntegerParser) enter(depth int) string {
	p.work++
	if depth > configDependencyConditionalIntegerMaxDepth || p.work > configDependencyConditionalIntegerMaxWork {
		return "conditional integer expression recursion or work budget"
	}
	return ""
}

func (p *configDependencyConditionalIntegerParser) peek() string {
	if p.position == len(p.tokens) {
		return ""
	}
	return p.tokens[p.position]
}

func (p *configDependencyConditionalIntegerParser) conditional(evaluate bool, depth int) (int64, string) {
	if reason := p.enter(depth); reason != "" {
		return 0, reason
	}
	condition, reason := p.binary(1, evaluate, depth+1)
	if reason != "" || p.peek() != "?" {
		return condition, reason
	}
	p.position++
	left, reason := p.conditional(evaluate && condition != 0, depth+1)
	if reason != "" {
		return 0, reason
	}
	if p.peek() != ":" {
		return 0, "conditional integer ternary has no colon"
	}
	p.position++
	right, reason := p.conditional(evaluate && condition == 0, depth+1)
	if reason != "" {
		return 0, reason
	}
	if !evaluate {
		return 0, ""
	}
	if condition != 0 {
		return left, ""
	}
	return right, ""
}

func configDependencyConditionalIntegerPrecedence(operator string) int {
	switch operator {
	case "||":
		return 1
	case "&&":
		return 2
	case "|":
		return 3
	case "^":
		return 4
	case "&":
		return 5
	case "==", "!=":
		return 6
	case "<", "<=", ">", ">=":
		return 7
	case "<<", ">>":
		return 8
	case "+", "-":
		return 9
	case "*", "/", "%":
		return 10
	}
	return 0
}

func (p *configDependencyConditionalIntegerParser) binary(minimum int, evaluate bool, depth int) (int64, string) {
	if reason := p.enter(depth); reason != "" {
		return 0, reason
	}
	left, reason := p.unary(evaluate, depth+1)
	if reason != "" {
		return 0, reason
	}
	for {
		operator := p.peek()
		precedence := configDependencyConditionalIntegerPrecedence(operator)
		if precedence < minimum {
			return left, ""
		}
		p.position++
		rightEvaluated := evaluate
		if operator == "&&" && left == 0 || operator == "||" && left != 0 {
			rightEvaluated = false
		}
		right, reason := p.binary(precedence+1, rightEvaluated, depth+1)
		if reason != "" {
			return 0, reason
		}
		if !evaluate {
			left = 0
			continue
		}
		left, reason = configDependencyConditionalIntegerBinary(operator, left, right)
		if reason != "" {
			return 0, reason
		}
	}
}

func (p *configDependencyConditionalIntegerParser) unary(evaluate bool, depth int) (int64, string) {
	if reason := p.enter(depth); reason != "" {
		return 0, reason
	}
	token := p.peek()
	switch token {
	case "+", "-", "!":
		p.position++
		value, reason := p.unary(evaluate, depth+1)
		if reason != "" || !evaluate {
			return 0, reason
		}
		switch token {
		case "-":
			return -value, ""
		case "!":
			return configDependencyConditionalIntegerBool(value == 0), ""
		default:
			return value, ""
		}
	case "~":
		return 0, "conditional integer complement requires signed representation authority"
	case "(":
		p.position++
		value, reason := p.conditional(evaluate, depth+1)
		if reason != "" {
			return 0, reason
		}
		if p.peek() != ")" {
			return 0, "conditional integer expression has unmatched parentheses"
		}
		p.position++
		return value, ""
	case "defined":
		return 0, "conditional integer expression contains unprotected or expansion-generated defined"
	case "true", "false", "and", "and_eq", "bitand", "bitor", "compl", "not", "not_eq", "or", "or_eq", "xor", "xor_eq":
		return 0, "conditional integer expression contains a language-dependent keyword"
	case "":
		return 0, "conditional integer expression ends before an operand"
	}
	p.position++
	if configDependencyConditionalIntegerIdentifier(token) {
		if p.undefined == nil || !p.undefined(token) {
			return 0, "conditional integer identifier has no authenticated undefined lookup"
		}
		return 0, ""
	}
	value, reason := configDependencyConditionalIntegerLiteral(token)
	if reason != "" || !evaluate {
		return 0, reason
	}
	return value, ""
}

func configDependencyConditionalIntegerIdentifier(token string) bool {
	for index := 0; index < len(token); index++ {
		character := token[index]
		if character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			index != 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return token != ""
}

func configDependencyConditionalIntegerLiteral(token string) (int64, string) {
	end := len(token)
	for end > 0 && (token[end-1] == 'l' || token[end-1] == 'L') {
		end--
	}
	switch token[end:] {
	case "", "l", "L", "ll", "LL":
	default:
		return 0, "conditional integer literal has an unsupported signed suffix"
	}
	number := token[:end]
	if number == "" {
		return 0, "conditional integer literal has no digits"
	}
	base := 10
	if len(number) >= 2 && number[0] == '0' && (number[1] == 'x' || number[1] == 'X') {
		base = 16
		number = number[2:]
	} else if number[0] == '0' {
		base = 8
	}
	// ParseUint with an explicit base rejects signs, separators, suffixes,
	// floating constants, binary extensions and invalid octal/hex digits.
	value, err := strconv.ParseUint(number, base, 63)
	if err != nil {
		return 0, "conditional integer literal is unsupported or outside the portable signed range"
	}
	return int64(value), ""
}

func configDependencyConditionalIntegerBool(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func configDependencyConditionalIntegerAdd(left, right int64) (int64, string) {
	if right > 0 && left > configDependencyConditionalIntegerMax-right ||
		right < 0 && left < configDependencyConditionalIntegerMin-right {
		return 0, "conditional integer arithmetic exceeds the portable signed range"
	}
	return left + right, ""
}

func configDependencyConditionalIntegerBinary(operator string, left, right int64) (int64, string) {
	switch operator {
	case "+":
		return configDependencyConditionalIntegerAdd(left, right)
	case "-":
		return configDependencyConditionalIntegerAdd(left, -right)
	case "*":
		magnitudeLeft, magnitudeRight := left, right
		if magnitudeLeft < 0 {
			magnitudeLeft = -magnitudeLeft
		}
		if magnitudeRight < 0 {
			magnitudeRight = -magnitudeRight
		}
		if magnitudeRight != 0 && magnitudeLeft > configDependencyConditionalIntegerMax/magnitudeRight {
			return 0, "conditional integer multiplication exceeds the portable signed range"
		}
		return left * right, ""
	case "/", "%":
		if right == 0 {
			return 0, "conditional integer division by zero"
		}
		if operator == "/" {
			return left / right, ""
		}
		return left % right, ""
	case "<<", ">>":
		if left < 0 || right < 0 || right >= 64 {
			return 0, "conditional integer shift is signed or width-dependent"
		}
		if operator == ">>" {
			return left >> uint(right), ""
		}
		if left > configDependencyConditionalIntegerMax>>uint(right) {
			return 0, "conditional integer shift exceeds the portable signed range"
		}
		return left << uint(right), ""
	case "&", "|", "^":
		if left < 0 || right < 0 {
			return 0, "conditional integer bitwise operation has a representation-dependent operand"
		}
		switch operator {
		case "&":
			return left & right, ""
		case "|":
			return left | right, ""
		default:
			return left ^ right, ""
		}
	case "<":
		return configDependencyConditionalIntegerBool(left < right), ""
	case "<=":
		return configDependencyConditionalIntegerBool(left <= right), ""
	case ">":
		return configDependencyConditionalIntegerBool(left > right), ""
	case ">=":
		return configDependencyConditionalIntegerBool(left >= right), ""
	case "==":
		return configDependencyConditionalIntegerBool(left == right), ""
	case "!=":
		return configDependencyConditionalIntegerBool(left != right), ""
	case "&&":
		return configDependencyConditionalIntegerBool(left != 0 && right != 0), ""
	case "||":
		return configDependencyConditionalIntegerBool(left != 0 || right != 0), ""
	}
	return 0, "conditional integer expression has an unsupported operator"
}
