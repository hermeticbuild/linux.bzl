package kconfig

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// EvaluateKbuildIntegerExpression interprets the finite integer-only portion
// of POSIX expr used by source Makefiles. The lexer observes the shell source:
// in particular a multiplication operator must be escaped or quoted so an
// undeclared pathname glob cannot change the operands. No host program or
// ambient process state participates in this source calculation.
func EvaluateKbuildIntegerExpression(command string) (string, error) {
	if len(command) > 512 || !strings.HasPrefix(command, "expr ") || strings.ContainsAny(command, "\x00\r\n") {
		return "", fmt.Errorf("unsupported Kbuild integer expression %q", command)
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return "", fmt.Errorf("lex Kbuild integer expression: %w", err)
	}
	if len(tokens) < 2 || len(tokens) > 66 || len(tokens)%2 != 0 || tokens[0].operator || tokens[0].value != "expr" || command[tokens[0].start:tokens[0].end] != "expr" {
		return "", fmt.Errorf("unsupported Kbuild integer expression %q", command)
	}
	values := make([]*big.Int, 0, len(tokens)/2)
	operators := make([]string, 0, len(tokens)/2-1)
	for index, token := range tokens[1:] {
		raw := command[token.start:token.end]
		if token.operator || token.pathnameExpansion || token.shellExpansion || strings.Contains(token.value, compactKbuildLiteralDollarToken) {
			return "", fmt.Errorf("Kbuild integer expression has active shell syntax in %q", raw)
		}
		if index%2 == 0 {
			if raw != token.value && !sourceShellQueryQuotedLiteral(raw) {
				return "", fmt.Errorf("Kbuild integer operand %q has unsupported shell quoting", raw)
			}
			value, parseErr := strconv.ParseInt(token.value, 10, 64)
			if parseErr != nil {
				return "", fmt.Errorf("Kbuild integer operand %q is outside signed 64-bit decimal", raw)
			}
			values = append(values, big.NewInt(value))
			continue
		}
		switch token.value {
		case "+", "-", "*", "/", "%":
			if !safeDynamicExprOperator(raw, token.value) {
				return "", fmt.Errorf("Kbuild integer operator %q has unsupported shell quoting", raw)
			}
			operators = append(operators, token.value)
		default:
			return "", fmt.Errorf("unsupported Kbuild integer operator %q", raw)
		}
	}
	checked := func(value *big.Int) error {
		if !value.IsInt64() {
			return fmt.Errorf("Kbuild integer expression overflows signed 64-bit arithmetic")
		}
		return nil
	}
	for index := 0; index < len(operators); {
		operator := operators[index]
		if operator != "*" && operator != "/" && operator != "%" {
			index++
			continue
		}
		if (operator == "/" || operator == "%") && values[index+1].Sign() == 0 {
			return "", fmt.Errorf("Kbuild integer expression divides by zero")
		}
		result := new(big.Int)
		switch operator {
		case "*":
			result.Mul(values[index], values[index+1])
		case "/":
			result.Quo(values[index], values[index+1])
		case "%":
			result.Rem(values[index], values[index+1])
		}
		if err := checked(result); err != nil {
			return "", err
		}
		values[index] = result
		values = append(values[:index+1], values[index+2:]...)
		operators = append(operators[:index], operators[index+1:]...)
	}
	result := values[0]
	for index, operator := range operators {
		value := new(big.Int)
		if operator == "+" {
			value.Add(result, values[index+1])
		} else {
			value.Sub(result, values[index+1])
		}
		if err := checked(value); err != nil {
			return "", err
		}
		result = value
	}
	return result.String(), nil
}

// EvaluateKbuildNumericShellPredicate evaluates the finite POSIX `[` form
// used by source compiler-version guards. Its exit status is projected by
// `&& echo WORD`; false yields empty shell text, while malformed syntax and
// numbers fail instead of being mistaken for a false predicate.
func EvaluateKbuildNumericShellPredicate(command string) (string, error) {
	if len(command) > 512 || strings.ContainsAny(command, "\x00\r\n") {
		return "", fmt.Errorf("unsupported Kbuild numeric shell predicate %q", command)
	}
	tokens, err := lexCompactKbuildRecipe(command)
	if err != nil {
		return "", fmt.Errorf("lex Kbuild numeric shell predicate: %w", err)
	}
	if len(tokens) != 8 || !tokens[5].operator || tokens[5].value != "&&" {
		return "", fmt.Errorf("unsupported Kbuild numeric shell predicate %q", command)
	}
	for index, token := range tokens {
		if index == 5 {
			continue
		}
		// The standalone '[' and ']' argv words delimit the POSIX test
		// builtin. Neither word is a complete shell bracket glob; keep the
		// lexer's pathname rejection for every numeric/operator/echo word.
		bracketWord := index == 0 && token.value == "[" || index == 4 && token.value == "]"
		if token.operator || token.shellExpansion || token.pathnameExpansion && !bracketWord ||
			strings.Contains(token.value, compactKbuildLiteralDollarToken) ||
			command[token.start:token.end] != token.value {
			return "", fmt.Errorf("Kbuild numeric shell predicate has active or unsupported shell word %q", command[token.start:token.end])
		}
	}
	if tokens[0].value != "[" || tokens[4].value != "]" || tokens[6].value != "echo" {
		return "", fmt.Errorf("unsupported Kbuild numeric shell predicate %q", command)
	}
	word := tokens[7].value
	if len(word) == 0 || len(word) > 64 || (word[0] < 'A' || word[0] > 'Z') &&
		(word[0] < 'a' || word[0] > 'z') && (word[0] < '0' || word[0] > '9') {
		return "", fmt.Errorf("Kbuild numeric shell predicate has unsafe echo word %q", word)
	}
	for _, character := range word[1:] {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || character == '_' {
			continue
		}
		return "", fmt.Errorf("Kbuild numeric shell predicate has unsafe echo word %q", word)
	}
	parseOperand := func(token compactKbuildRecipeToken) (int64, error) {
		if err := ValidateProbeSignedDecimalArgument(token.value); err != nil {
			return 0, fmt.Errorf("Kbuild numeric shell predicate operand %q: %w", command[token.start:token.end], err)
		}
		value, err := strconv.ParseInt(token.value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("Kbuild numeric shell predicate operand %q is outside signed 64-bit decimal: %w", command[token.start:token.end], err)
		}
		return value, nil
	}
	left, err := parseOperand(tokens[1])
	if err != nil {
		return "", err
	}
	right, err := parseOperand(tokens[3])
	if err != nil {
		return "", err
	}
	var truth bool
	switch tokens[2].value {
	case "-ge":
		truth = left >= right
	case "-gt":
		truth = left > right
	case "-le":
		truth = left <= right
	case "-lt":
		truth = left < right
	case "-eq":
		truth = left == right
	case "-ne":
		truth = left != right
	default:
		return "", fmt.Errorf("unsupported Kbuild numeric shell predicate comparison %q", tokens[2].value)
	}
	if !truth {
		return "", nil
	}
	return word, nil
}
