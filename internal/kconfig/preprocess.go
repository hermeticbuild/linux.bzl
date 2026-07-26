package kconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

type variableFlavor int

const (
	varRecursive variableFlavor = iota
	varSimple
	varAppend
)

type variable struct {
	value    string
	flavor   variableFlavor
	expCount int
}

type preprocessor struct {
	ctx         context.Context
	opts        Options
	vars        map[string]*variable
	diagnostics *[]Diagnostic
	current     Position
}

func newPreprocessor(ctx context.Context, opts Options, diagnostics *[]Diagnostic) *preprocessor {
	p := &preprocessor{
		ctx:         ctx,
		opts:        opts,
		vars:        map[string]*variable{},
		diagnostics: diagnostics,
	}
	for name, value := range opts.Variables {
		p.vars[name] = &variable{value: value, flavor: varSimple}
	}
	return p
}

func (p *preprocessor) setPosition(pos Position) {
	p.current = pos
}

func (p *preprocessor) addVariable(name, value string, flavor variableFlavor, pos Position) error {
	p.setPosition(pos)
	v := p.vars[name]
	appendValue := false
	if v != nil {
		if flavor == varAppend {
			flavor = v.flavor
			appendValue = true
		}
	} else {
		if flavor == varAppend {
			flavor = varRecursive
		}
		v = &variable{}
		p.vars[name] = v
	}
	v.flavor = flavor

	newValue := value
	if flavor == varSimple {
		expanded, err := p.expandString(value, nil)
		if err != nil {
			return err
		}
		newValue = expanded
	}
	if appendValue && v.value != "" {
		v.value += " " + newValue
	} else if appendValue {
		v.value = newValue
	} else {
		v.value = newValue
	}
	return nil
}

func (p *preprocessor) expandString(in string, args []string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(in); {
		if in[i] != '$' {
			out.WriteByte(in[i])
			i++
			continue
		}
		if i+1 >= len(in) || in[i+1] != '(' {
			out.WriteByte('$')
			i++
			continue
		}
		end, err := matchingParen(in, i+1)
		if err != nil {
			return "", p.errorf("%v", err)
		}
		expanded, err := p.evalClause(in[i+2:end], args)
		if err != nil {
			return "", err
		}
		out.WriteString(expanded)
		i = end + 1
	}
	return out.String(), nil
}

func (p *preprocessor) evalClause(clause string, args []string) (string, error) {
	if n, err := strconv.Atoi(strings.TrimSpace(clause)); err == nil && n > 0 && n <= len(args) {
		return args[n-1], nil
	}

	parts := splitFunctionArgs(clause)
	if len(parts) == 0 {
		return "", nil
	}
	name, err := p.expandString(parts[0], args)
	if err != nil {
		return "", err
	}
	callArgs := make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		expanded, err := p.expandString(part, args)
		if err != nil {
			return "", err
		}
		callArgs = append(callArgs, expanded)
	}

	if value, ok, err := p.expandVariable(name, callArgs); ok || err != nil {
		return value, err
	}
	if value, ok, err := p.expandBuiltin(name, callArgs); ok || err != nil {
		return value, err
	}
	if len(callArgs) == 0 {
		if value, ok := p.expandEnv(name); ok {
			return value, nil
		}
	}
	return "", nil
}

func (p *preprocessor) expandVariable(name string, args []string) (string, bool, error) {
	v := p.vars[name]
	if v == nil {
		return "", false, nil
	}
	if len(args) == 0 && v.expCount > 0 {
		return "", true, p.errorf("recursive variable %q references itself", name)
	}
	if v.expCount > 1000 {
		return "", true, p.errorf("too deep recursive expansion")
	}
	v.expCount++
	defer func() { v.expCount-- }()
	if v.flavor == varRecursive {
		value, err := p.expandString(v.value, args)
		return value, true, err
	}
	return v.value, true, nil
}

func (p *preprocessor) expandBuiltin(name string, args []string) (string, bool, error) {
	checkArgs := func(min, max int) error {
		if len(args) < min {
			return p.errorf("too few function arguments passed to %q", name)
		}
		if len(args) > max {
			return p.errorf("too many function arguments passed to %q", name)
		}
		return nil
	}

	switch name {
	case "error-if":
		if err := checkArgs(2, 2); err != nil {
			return "", true, err
		}
		if args[0] == "y" {
			return "", true, p.errorf("%s", args[1])
		}
		return "", true, nil
	case "filename":
		if err := checkArgs(0, 0); err != nil {
			return "", true, err
		}
		return p.current.Filename, true, nil
	case "info":
		if err := checkArgs(1, 1); err != nil {
			return "", true, err
		}
		*p.diagnostics = append(*p.diagnostics, Diagnostic{Position: p.current, Message: args[0]})
		return "", true, nil
	case "lineno":
		if err := checkArgs(0, 0); err != nil {
			return "", true, err
		}
		return strconv.Itoa(p.current.Line), true, nil
	case "shell":
		if err := checkArgs(1, 1); err != nil {
			return "", true, err
		}
		value, err := p.runShell(args[0])
		return value, true, err
	case "warning-if":
		if err := checkArgs(2, 2); err != nil {
			return "", true, err
		}
		if args[0] == "y" {
			*p.diagnostics = append(*p.diagnostics, Diagnostic{Position: p.current, Message: args[1]})
		}
		return "", true, nil
	default:
		return "", false, nil
	}
}

func (p *preprocessor) expandEnv(name string) (string, bool) {
	if p.opts.Env != nil {
		value, ok := p.opts.Env[name]
		if ok {
			return value, true
		}
	}
	if p.opts.UseHostEnv {
		value, ok := os.LookupEnv(name)
		return value, ok
	}
	return "", false
}

func (p *preprocessor) runShell(command string) (string, error) {
	if !p.opts.AllowShell {
		return "", p.errorf("$(shell,...) is disabled for hermetic parsing")
	}
	var (
		out []byte
		err error
	)
	if p.opts.Shell != nil {
		outString, err := p.opts.Shell(p.ctx, command)
		return normalizeShellOutput([]byte(outString)), err
	}
	cmd := exec.CommandContext(p.ctx, "sh", "-c", command)
	cmd.Env = sortedEnv(p.opts.Env)
	if p.opts.UseHostEnv {
		cmd.Env = append(os.Environ(), cmd.Env...)
	}
	out, err = cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", p.errorf("shell command failed: %v", err)
		}
	}
	if err := p.ctx.Err(); err != nil {
		return "", p.errorf("shell command failed: %v", err)
	}
	return normalizeShellOutput(out), nil
}

func normalizeShellOutput(out []byte) string {
	out = bytes.TrimRight(out, "\n")
	out = bytes.ReplaceAll(out, []byte("\n"), []byte(" "))
	return string(out)
}

func sortedEnv(env map[string]string) []string {
	keys := slices.Sorted(maps.Keys(env))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func (p *preprocessor) errorf(format string, args ...any) error {
	return fmt.Errorf("%s: %s", p.current, fmt.Sprintf(format, args...))
}

func splitFunctionArgs(in string) []string {
	var args []string
	start := 0
	depth := 0
	for i := 0; i < len(in); i++ {
		switch in[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				args = append(args, in[start:i])
				start = i + 1
			}
		}
	}
	args = append(args, in[start:])
	return args
}

func matchingParen(in string, open int) (int, error) {
	depth := 0
	for i := open + 1; i < len(in); i++ {
		switch in[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return i, nil
			}
			depth--
		}
	}
	return 0, fmt.Errorf("unterminated reference %q: missing ')'", in[open:])
}
