package kconfig

// This file evaluates the deliberately small, data-only shell pipelines Linux
// source uses to turn a compiler target triple into its Make architecture.
// The compiler result is supplied by the execution-time bootstrap; no process
// is executed and no architecture or compiler-family mapping lives here.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// EvaluateLinuxCompilerMachineShell evaluates a source-owned text pipeline
// rooted at either `uname -m` or one of the exact configured compiler tokens
// followed by `-dumpmachine`. `uname -m` is virtualized with the first
// component of the selected compiler triple, so a cross build never observes
// the planner or remote worker architecture.
//
// The returned handled bit is false when the command is not a compiler-machine
// query. Once a machine query is recognized, every remaining stage must be one
// of the bounded, pure text filters below; unknown programs and general shell
// control flow fail closed.
func EvaluateLinuxCompilerMachineShell(
	command string,
	machine string,
	compilerTokens []string,
) (value string, handled bool, err error) {
	machine = strings.TrimSpace(machine)
	if !validLinuxCompilerMachine(machine) {
		return "", false, fmt.Errorf("invalid selected compiler machine %q", machine)
	}
	tokens, err := lexLinuxCompilerMachineShell(command)
	if err != nil {
		return "", false, err
	}
	stages, recognized, err := linuxCompilerMachinePipeline(tokens, machine, compilerTokens)
	if err != nil || !recognized {
		return "", recognized, err
	}
	value = stages.seed
	for index, stage := range stages.filters {
		value, err = evaluateLinuxCompilerMachineFilter(value, stage)
		if err != nil {
			return "", true, fmt.Errorf("compiler-machine pipeline stage %d: %w", index+1, err)
		}
	}
	return value, true, nil
}

// LinuxCompilerMachineArchitecture returns the compiler triple component that
// a source-owned `uname -m` query receives. It only separates the target
// triple; all normalization into a Linux ARCH remains in the selected source.
func LinuxCompilerMachineArchitecture(machine string) (string, error) {
	machine = strings.TrimSpace(machine)
	if !validLinuxCompilerMachine(machine) {
		return "", fmt.Errorf("invalid selected compiler machine %q", machine)
	}
	if index := strings.IndexByte(machine, '-'); index >= 0 {
		machine = machine[:index]
	}
	return machine, nil
}

func validLinuxCompilerMachine(value string) bool {
	if value == "" || strings.ContainsAny(value, "\x00\r\n/\\ ") {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("_.+-", character) {
			continue
		}
		return false
	}
	return true
}

type linuxCompilerMachinePipelineStages struct {
	seed    string
	filters [][]string
}

func linuxCompilerMachinePipeline(
	tokens []linuxCompilerMachineShellToken,
	machine string,
	compilerTokens []string,
) (linuxCompilerMachinePipelineStages, bool, error) {
	root := []string{}
	for _, token := range tokens {
		if token.operator {
			break
		}
		root = append(root, token.value)
	}
	seed := ""
	switch {
	case len(root) == 2 && root[0] == "uname" && root[1] == "-m":
		seed, _ = LinuxCompilerMachineArchitecture(machine)
	case len(root) == 2 && root[1] == "-dumpmachine" && stringInList(root[0], compilerTokens):
		seed = machine
	default:
		return linuxCompilerMachinePipelineStages{}, false, nil
	}

	commands := [][]string{{}}
	for _, token := range tokens {
		if token.operator {
			if token.value != "|" {
				return linuxCompilerMachinePipelineStages{}, true, fmt.Errorf("unsupported compiler-machine shell operator %q", token.value)
			}
			if len(commands[len(commands)-1]) == 0 {
				return linuxCompilerMachinePipelineStages{}, true, fmt.Errorf("compiler-machine pipeline has an empty stage")
			}
			commands = append(commands, []string{})
			continue
		}
		commands[len(commands)-1] = append(commands[len(commands)-1], token.value)
	}
	if len(commands) == 0 || len(commands[0]) == 0 {
		return linuxCompilerMachinePipelineStages{}, true, fmt.Errorf("compiler-machine pipeline has no root stage")
	}
	if len(commands[len(commands)-1]) == 0 {
		return linuxCompilerMachinePipelineStages{}, true, fmt.Errorf("compiler-machine pipeline has an empty final stage")
	}
	return linuxCompilerMachinePipelineStages{seed: seed, filters: commands[1:]}, true, nil
}

func stringInList(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if candidate != "" && value == candidate {
			return true
		}
	}
	return false
}

type linuxCompilerMachineShellToken struct {
	value    string
	operator bool
}

func lexLinuxCompilerMachineShell(command string) ([]linuxCompilerMachineShellToken, error) {
	var tokens []linuxCompilerMachineShellToken
	var word strings.Builder
	quote := byte(0)
	started := false
	flush := func() {
		if !started {
			return
		}
		tokens = append(tokens, linuxCompilerMachineShellToken{value: word.String()})
		word.Reset()
		started = false
	}
	for index := 0; index < len(command); index++ {
		character := command[index]
		if quote != 0 {
			switch {
			case character == quote:
				quote = 0
				started = true
			case character == '\\' && quote == '"':
				if index+1 == len(command) {
					return nil, fmt.Errorf("compiler-machine command has a trailing quoted escape")
				}
				index++
				word.WriteByte(command[index])
				started = true
			default:
				word.WriteByte(character)
				started = true
			}
			continue
		}
		switch {
		case character == '\'' || character == '"':
			quote = character
			started = true
		case character == '\\':
			if index+1 == len(command) {
				return nil, fmt.Errorf("compiler-machine command has a trailing escape")
			}
			index++
			word.WriteByte(command[index])
			started = true
		case strings.ContainsRune(" \t\r\n", rune(character)):
			flush()
		case strings.ContainsRune(";&|<>()", rune(character)):
			flush()
			operator := string(character)
			if index+1 < len(command) {
				next := command[index+1]
				if character == next && strings.ContainsRune("&|>", rune(character)) {
					operator += string(next)
					index++
				}
			}
			tokens = append(tokens, linuxCompilerMachineShellToken{value: operator, operator: true})
		default:
			word.WriteByte(character)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("compiler-machine command has an unterminated quote")
	}
	flush()
	return tokens, nil
}

func evaluateLinuxCompilerMachineFilter(value string, stage []string) (string, error) {
	if len(stage) == 0 {
		return "", fmt.Errorf("empty text filter")
	}
	switch stage[0] {
	case "sed":
		return evaluateLinuxCompilerMachineSed(value, stage[1:])
	case "cut":
		return evaluateLinuxCompilerMachineCut(value, stage[1:])
	case "tr":
		return evaluateLinuxCompilerMachineTr(value, stage[1:])
	case "head":
		return evaluateLinuxCompilerMachineHead(value, stage[1:])
	default:
		return "", fmt.Errorf("unbound text filter %q", stage[0])
	}
}

func evaluateLinuxCompilerMachineSed(value string, arguments []string) (string, error) {
	expressions := []string{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-e":
			if index+1 == len(arguments) {
				return "", fmt.Errorf("sed -e has no expression")
			}
			index++
			expressions = append(expressions, arguments[index])
		case strings.HasPrefix(argument, "-e") && len(argument) > 2:
			expressions = append(expressions, argument[2:])
		case strings.HasPrefix(argument, "-"):
			return "", fmt.Errorf("unsupported sed option %q", argument)
		default:
			expressions = append(expressions, argument)
		}
	}
	if len(expressions) == 0 {
		return "", fmt.Errorf("sed has no expression")
	}
	var err error
	for _, expression := range expressions {
		value, err = applyLinuxCompilerMachineSedExpression(value, expression)
		if err != nil {
			return "", err
		}
	}
	return value, nil
}

func applyLinuxCompilerMachineSedExpression(value, expression string) (string, error) {
	apply := true
	if strings.HasPrefix(expression, "/") {
		end, err := linuxCompilerMachineDelimitedEnd(expression, 1, '/')
		if err != nil {
			return "", fmt.Errorf("invalid addressed sed expression %q: %w", expression, err)
		}
		address := expression[1:end]
		rest := expression[end+1:]
		negate := strings.HasPrefix(rest, "!")
		if negate {
			rest = strings.TrimPrefix(rest, "!")
		}
		matched, err := regexp.MatchString(address, value)
		if err != nil {
			return "", fmt.Errorf("invalid sed address %q: %w", address, err)
		}
		apply = matched != negate
		expression = rest
	}
	if len(expression) < 4 || expression[0] != 's' {
		return "", fmt.Errorf("unsupported sed expression %q", expression)
	}
	delimiter := expression[1]
	patternEnd, err := linuxCompilerMachineDelimitedEnd(expression, 2, delimiter)
	if err != nil {
		return "", fmt.Errorf("malformed sed substitution %q: %w", expression, err)
	}
	replacementEnd, err := linuxCompilerMachineDelimitedEnd(expression, patternEnd+1, delimiter)
	if err != nil {
		return "", fmt.Errorf("malformed sed substitution %q: %w", expression, err)
	}
	pattern := expression[2:patternEnd]
	replacement := expression[patternEnd+1 : replacementEnd]
	flags := expression[replacementEnd+1:]
	if flags != "" && flags != "g" {
		return "", fmt.Errorf("unsupported sed flags %q", flags)
	}
	if !apply {
		return value, nil
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid sed pattern %q: %w", pattern, err)
	}
	if flags == "g" {
		return compiled.ReplaceAllString(value, replacement), nil
	}
	location := compiled.FindStringIndex(value)
	if location == nil {
		return value, nil
	}
	return value[:location[0]] + compiled.ReplaceAllString(value[location[0]:location[1]], replacement) + value[location[1]:], nil
}

func linuxCompilerMachineDelimitedEnd(value string, start int, delimiter byte) (int, error) {
	escaped := false
	for index := start; index < len(value); index++ {
		switch {
		case escaped:
			escaped = false
		case value[index] == '\\':
			escaped = true
		case value[index] == delimiter:
			return index, nil
		}
	}
	return 0, fmt.Errorf("missing %q delimiter", delimiter)
}

func evaluateLinuxCompilerMachineCut(value string, arguments []string) (string, error) {
	delimiter := byte('\t')
	field := 0
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-d":
			if index+1 == len(arguments) || len(arguments[index+1]) != 1 {
				return "", fmt.Errorf("cut -d requires one byte")
			}
			index++
			delimiter = arguments[index][0]
		case strings.HasPrefix(argument, "-d") && len(argument) == 3:
			delimiter = argument[2]
		case argument == "-f":
			if index+1 == len(arguments) {
				return "", fmt.Errorf("cut -f requires one field")
			}
			index++
			parsed, err := strconv.Atoi(arguments[index])
			if err != nil || parsed < 1 {
				return "", fmt.Errorf("unsupported cut field %q", arguments[index])
			}
			field = parsed
		case strings.HasPrefix(argument, "-f") && len(argument) > 2:
			parsed, err := strconv.Atoi(argument[2:])
			if err != nil || parsed < 1 {
				return "", fmt.Errorf("unsupported cut field %q", argument[2:])
			}
			field = parsed
		default:
			return "", fmt.Errorf("unsupported cut argument %q", argument)
		}
	}
	if field == 0 {
		return "", fmt.Errorf("cut requires exactly one selected field")
	}
	fields := strings.Split(value, string(delimiter))
	if field > len(fields) {
		return "", nil
	}
	return fields[field-1], nil
}

func evaluateLinuxCompilerMachineTr(value string, arguments []string) (string, error) {
	if len(arguments) != 2 {
		return "", fmt.Errorf("tr requires exactly two character sets")
	}
	from, err := expandLinuxCompilerMachineCharacterSet(arguments[0])
	if err != nil {
		return "", fmt.Errorf("invalid tr source set: %w", err)
	}
	to, err := expandLinuxCompilerMachineCharacterSet(arguments[1])
	if err != nil {
		return "", fmt.Errorf("invalid tr replacement set: %w", err)
	}
	if len(from) == 0 || len(to) == 0 {
		return "", fmt.Errorf("tr character sets must not be empty")
	}
	replacements := make(map[byte]byte, len(from))
	for index, character := range from {
		replacementIndex := index
		if replacementIndex >= len(to) {
			replacementIndex = len(to) - 1
		}
		replacements[character] = to[replacementIndex]
	}
	output := []byte(value)
	for index, character := range output {
		if replacement, ok := replacements[character]; ok {
			output[index] = replacement
		}
	}
	return string(output), nil
}

func expandLinuxCompilerMachineCharacterSet(value string) ([]byte, error) {
	if strings.ContainsAny(value, "\x00/\\") {
		return nil, fmt.Errorf("unsafe character set %q", value)
	}
	var out []byte
	for index := 0; index < len(value); index++ {
		if index+2 < len(value) && value[index+1] == '-' {
			first, last := value[index], value[index+2]
			if first > last {
				return nil, fmt.Errorf("descending range %q", value[index:index+3])
			}
			for character := first; ; character++ {
				out = append(out, character)
				if character == last {
					break
				}
			}
			index += 2
			continue
		}
		out = append(out, value[index])
	}
	return out, nil
}

func evaluateLinuxCompilerMachineHead(value string, arguments []string) (string, error) {
	if len(arguments) == 1 && arguments[0] == "-n1" || len(arguments) == 2 && arguments[0] == "-n" && arguments[1] == "1" {
		if index := strings.IndexAny(value, "\r\n"); index >= 0 {
			value = value[:index]
		}
		return value, nil
	}
	return "", fmt.Errorf("unsupported head arguments %q", arguments)
}
