package kconfig

import "strings"

const (
	configDependencyConditionalMaximumBytes = 4096
	configDependencyConditionalMaximumDepth = 64
)

// configDependencyConditionalDefinition accepts only literal Boolean tokens,
// defined operands, and !/&&/||/parentheses. Validate the entire expression
// before reading macro state: a raw macro in 0 && M can expand to tokens such
// as 1 || CONFIG_X, so reducing a recognized prefix would be unsound.
//
// Both passes are bounded and retain no syntax tree. Evaluation deliberately
// visits every validated defined operand, preserving conservative dependency
// tracing even when another operand determines the Boolean result.
func configDependencyConditionalDefinition(expression string, state *configDependencyMacroState) configDependencyMacroDefinition {
	if len(expression) > configDependencyConditionalMaximumBytes {
		return configDependencyMacroUnknown
	}
	validator := configDependencyConditionalParser{text: expression}
	if _, valid := validator.parse(); !valid {
		return configDependencyMacroUnknown
	}
	evaluator := configDependencyConditionalParser{text: expression, state: state, evaluate: true}
	result, valid := evaluator.parse()
	if !valid {
		return configDependencyMacroUnknown
	}
	return result
}

type configDependencyConditionalParser struct {
	text     string
	position int
	depth    int
	state    *configDependencyMacroState
	evaluate bool
}

func (p *configDependencyConditionalParser) whitespace() {
	for p.position < len(p.text) {
		switch p.text[p.position] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			p.position++
		default:
			return
		}
	}
}

func (p *configDependencyConditionalParser) take(token string) bool {
	p.whitespace()
	if !strings.HasPrefix(p.text[p.position:], token) {
		return false
	}
	p.position += len(token)
	return true
}

func (p *configDependencyConditionalParser) identifier() (string, bool) {
	p.whitespace()
	name, valid := configDependencyMacroIdentifier(p.text[p.position:])
	if valid {
		p.position += len(name)
	}
	return name, valid
}

func (p *configDependencyConditionalParser) parse() (configDependencyMacroDefinition, bool) {
	result, valid := p.or()
	p.whitespace()
	return result, valid && p.position == len(p.text)
}

func (p *configDependencyConditionalParser) or() (configDependencyMacroDefinition, bool) {
	left, valid := p.and()
	if !valid {
		return configDependencyMacroUnknown, false
	}
	for p.take("||") {
		right, valid := p.and()
		if !valid {
			return configDependencyMacroUnknown, false
		}
		left = configDependencyMacroOr(left, right)
	}
	return left, true
}

func (p *configDependencyConditionalParser) and() (configDependencyMacroDefinition, bool) {
	left, valid := p.unary()
	if !valid {
		return configDependencyMacroUnknown, false
	}
	for p.take("&&") {
		right, valid := p.unary()
		if !valid {
			return configDependencyMacroUnknown, false
		}
		left = configDependencyMacroAnd(left, right)
	}
	return left, true
}

func (p *configDependencyConditionalParser) unary() (configDependencyMacroDefinition, bool) {
	negated := false
	for p.take("!") {
		negated = !negated
	}
	result, valid := p.primary()
	if negated {
		result = configDependencyMacroNot(result)
	}
	return result, valid
}

func (p *configDependencyConditionalParser) primary() (configDependencyMacroDefinition, bool) {
	if p.take("(") {
		if p.depth >= configDependencyConditionalMaximumDepth {
			return configDependencyMacroUnknown, false
		}
		p.depth++
		result, valid := p.or()
		p.depth--
		if !valid || !p.take(")") {
			return configDependencyMacroUnknown, false
		}
		return result, true
	}
	if p.take("0") {
		return configDependencyMacroUndefined, true
	}
	if p.take("1") {
		return configDependencyMacroDefined, true
	}
	keyword, valid := p.identifier()
	if !valid || keyword != "defined" {
		return configDependencyMacroUnknown, false
	}
	parenthesized := p.take("(")
	if parenthesized && p.depth >= configDependencyConditionalMaximumDepth {
		return configDependencyMacroUnknown, false
	}
	name, valid := p.identifier()
	if !valid || parenthesized && !p.take(")") {
		return configDependencyMacroUnknown, false
	}
	if !p.evaluate {
		return configDependencyMacroUnknown, true
	}
	return p.state.definition(name), true
}

func configDependencyMacroOr(left, right configDependencyMacroDefinition) configDependencyMacroDefinition {
	if left == configDependencyMacroDefined || right == configDependencyMacroDefined {
		return configDependencyMacroDefined
	}
	if left == configDependencyMacroUndefined && right == configDependencyMacroUndefined {
		return configDependencyMacroUndefined
	}
	return configDependencyMacroUnknown
}
