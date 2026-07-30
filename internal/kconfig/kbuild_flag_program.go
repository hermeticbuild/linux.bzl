package kconfig

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type kbuildFlagExprKind uint8

const (
	kbuildFlagLiteral kbuildFlagExprKind = iota
	kbuildFlagConcat
	kbuildFlagSelect
)

// kbuildFlagExpr is the parser-internal form of a lazy Kbuild flag program.
// Nodes are immutable and hash-consed by kbuildFlagExprInterner.
type kbuildFlagExpr struct {
	kind      kbuildFlagExprKind
	id        string
	argv      []string
	children  []*kbuildFlagExpr
	probe     kbuildFlagProbe
	whenTrue  *kbuildFlagExpr
	whenFalse *kbuildFlagExpr
}

type kbuildFlagProbe struct {
	kind          string
	candidateArgv []string
	context       *kbuildFlagExpr
	language      string
	srcarch       string
}

type kbuildFlagExprInterner struct {
	expressions map[string]*kbuildFlagExpr
}

func kbuildFlagExpressionID(expression *kbuildFlagExpr) string {
	if expression == nil {
		return ""
	}
	return expression.id
}

func newKbuildFlagExprInterner() *kbuildFlagExprInterner {
	return &kbuildFlagExprInterner{
		expressions: map[string]*kbuildFlagExpr{},
	}
}

func (interner *kbuildFlagExprInterner) literal(argv []string) *kbuildFlagExpr {
	argv = append([]string(nil), argv...)
	hasher := newCompactContentHasher("linux-kbuild-flag-expr-literal-v1")
	hasher.writeValue("argc=", strconv.Itoa(len(argv)))
	for _, arg := range argv {
		hasher.writeValue("arg=", arg)
	}
	id := hasher.id()
	if existing := interner.expressions[id]; existing != nil {
		return existing
	}
	expression := &kbuildFlagExpr{
		kind: kbuildFlagLiteral,
		id:   id,
		argv: argv,
	}
	interner.expressions[id] = expression
	return expression
}

func (interner *kbuildFlagExprInterner) concat(
	expressions ...*kbuildFlagExpr,
) *kbuildFlagExpr {
	var children []*kbuildFlagExpr
	for _, expression := range expressions {
		if expression == nil {
			continue
		}
		switch expression.kind {
		case kbuildFlagLiteral:
			if len(expression.argv) == 0 {
				continue
			}
		case kbuildFlagConcat:
			children = append(children, expression.children...)
			continue
		}
		children = append(children, expression)
	}
	switch len(children) {
	case 0:
		return interner.literal(nil)
	case 1:
		return children[0]
	}
	hasher := newCompactContentHasher("linux-kbuild-flag-expr-concat-v1")
	for _, child := range children {
		hasher.writeValue("child=", child.id)
	}
	id := hasher.id()
	if existing := interner.expressions[id]; existing != nil {
		return existing
	}
	expression := &kbuildFlagExpr{
		kind:     kbuildFlagConcat,
		id:       id,
		children: append([]*kbuildFlagExpr(nil), children...),
	}
	interner.expressions[id] = expression
	return expression
}

func (interner *kbuildFlagExprInterner) selectExpr(
	probe kbuildFlagProbe,
	whenTrue *kbuildFlagExpr,
	whenFalse *kbuildFlagExpr,
) *kbuildFlagExpr {
	if whenTrue == nil {
		whenTrue = interner.literal(nil)
	}
	if whenFalse == nil {
		whenFalse = interner.literal(nil)
	}
	if whenTrue.id == whenFalse.id {
		return whenTrue
	}
	hasher := newCompactContentHasher("linux-kbuild-flag-expr-select-v1")
	hasher.writeValue("probe_kind=", probe.kind)
	hasher.writeValue("probe_language=", probe.language)
	hasher.writeValue("probe_srcarch=", probe.srcarch)
	if probe.context != nil {
		hasher.writeValue("probe_context=", probe.context.id)
	}
	for _, arg := range probe.candidateArgv {
		hasher.writeValue("probe_candidate_arg=", arg)
	}
	hasher.writeValue("when_true=", whenTrue.id)
	hasher.writeValue("when_false=", whenFalse.id)
	id := hasher.id()
	if existing := interner.expressions[id]; existing != nil {
		return existing
	}
	expression := &kbuildFlagExpr{
		kind:      kbuildFlagSelect,
		id:        id,
		probe:     probe,
		whenTrue:  whenTrue,
		whenFalse: whenFalse,
	}
	interner.expressions[id] = expression
	return expression
}

type kbuildFlagExpansion struct {
	expressions map[string]*kbuildFlagExpr
	next        int
}

func (expansion *kbuildFlagExpansion) marker(expression *kbuildFlagExpr) string {
	expansion.next++
	marker := "\x1fKBUILD_FLAG_EXPR_" + strconv.Itoa(expansion.next) + "\x1f"
	expansion.expressions[marker] = expression
	return marker
}

func (p *kbuildParser) kbuildFlagArgsContainMarker(values []string) bool {
	for _, value := range values {
		if p.kbuildFlagValueContainsMarker(value) {
			return true
		}
	}
	return false
}

func (p *kbuildParser) kbuildFlagValueContainsMarker(value string) bool {
	if p.flagExpansion == nil {
		return false
	}
	for marker := range p.flagExpansion.expressions {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (p *kbuildParser) evalSymbolicKbuildFilter(
	name string,
	args []string,
) (string, error) {
	if len(args) != 2 {
		return "", fmt.Errorf(
			"symbolic Kbuild flag expression through Make function %q requires two arguments",
			name,
		)
	}
	if p.kbuildFlagValueContainsMarker(args[0]) {
		return "", fmt.Errorf(
			"unsupported symbolic Kbuild flag patterns in Make function %q",
			name,
		)
	}
	expression, err := p.kbuildFlagExpressionFromExpanded(args[1])
	if err != nil {
		return "", fmt.Errorf("model symbolic %s word list: %w", name, err)
	}
	invert := name == "filter-out"
	expression = mapKbuildFlagExpressionOutput(
		expression,
		func(argv []string) []string {
			return filterMakeWordSlice(args[0], argv, invert)
		},
	)
	return p.flagExpansion.marker(expression), nil
}

func (p *kbuildParser) expandKbuildFlagExpression(
	value string,
	depth int,
) (*kbuildFlagExpr, error) {
	if p.flagExpansion != nil {
		expanded, err := p.expandDepth(value, depth)
		if err != nil {
			return nil, err
		}
		return p.kbuildFlagExpressionFromExpanded(expanded)
	}
	expansion := &kbuildFlagExpansion{
		expressions: map[string]*kbuildFlagExpr{},
	}
	p.flagExpansion = expansion
	defer func() {
		p.flagExpansion = nil
	}()
	expanded, err := p.expandDepth(value, depth)
	if err != nil {
		return nil, err
	}
	return p.kbuildFlagExpressionFromExpanded(expanded)
}

func (p *kbuildParser) kbuildFlagExpressionFromExpanded(
	value string,
) (*kbuildFlagExpr, error) {
	fields := kbuildFields(value)
	parts := make([]*kbuildFlagExpr, 0, len(fields))
	var literals []string
	flushLiterals := func() {
		if len(literals) == 0 {
			return
		}
		parts = append(parts, p.flagInterner.literal(literals))
		literals = nil
	}
	for _, field := range fields {
		if expression := p.flagExpansion.expressions[field]; expression != nil {
			flushLiterals()
			parts = append(parts, expression)
			continue
		}
		for marker := range p.flagExpansion.expressions {
			if strings.Contains(field, marker) {
				return nil, fmt.Errorf(
					"symbolic Kbuild flag probe is embedded in argument %q",
					field,
				)
			}
		}
		literals = append(literals, field)
	}
	flushLiterals()
	return p.flagInterner.concat(parts...), nil
}

func (p *kbuildParser) symbolicKbuildKnownCall(
	name string,
	args []string,
	original string,
	srcarch string,
	depth int,
) (string, bool, error) {
	var kind, language string
	var candidate, whenTrue, whenFalse []string
	switch name {
	case "cc-disable-warning":
		if len(args) != 1 {
			return original, true, nil
		}
		warning := strings.Join(strings.Fields(args[0]), "")
		if warning == "" {
			return "", true, nil
		}
		kind, language = "cc_option", "c"
		candidate = []string{"-Wno-" + warning}
		whenTrue = candidate
	case "cc-option", "as-option", "ld-option":
		if len(args) < 1 || len(args) > 2 {
			return original, true, nil
		}
		candidate = kbuildFields(strings.TrimSpace(args[0]))
		if len(candidate) == 0 {
			if len(args) == 2 {
				return strings.TrimSpace(args[1]), true, nil
			}
			return "", true, nil
		}
		switch name {
		case "cc-option":
			kind, language = "cc_option", "c"
		case "as-option":
			kind, language = "as_option", "asm"
		case "ld-option":
			kind, language = "ld_option", "link"
		}
		whenTrue = candidate
		if len(args) == 2 {
			whenFalse = kbuildFields(strings.TrimSpace(args[1]))
		}
	case "cc-option-yn":
		if len(args) != 1 {
			return original, true, nil
		}
		candidate = kbuildFields(strings.TrimSpace(args[0]))
		if len(candidate) == 0 {
			return "n", true, nil
		}
		kind, language = "cc_option", "c"
		whenTrue, whenFalse = []string{"y"}, []string{"n"}
	default:
		return "", false, nil
	}
	for _, arg := range append(append([]string(nil), candidate...), whenFalse...) {
		for marker := range p.flagExpansion.expressions {
			if strings.Contains(arg, marker) {
				return "", true, fmt.Errorf(
					"%s candidate or fallback contains a nested symbolic probe",
					name,
				)
			}
		}
	}
	context, err := p.kbuildProbeContextExpression(name, depth)
	if err != nil {
		return "", true, err
	}
	expression := p.flagInterner.selectExpr(
		kbuildFlagProbe{
			kind:          kind,
			candidateArgv: candidate,
			context:       context,
			language:      language,
			srcarch:       srcarch,
		},
		p.flagInterner.literal(whenTrue),
		p.flagInterner.literal(whenFalse),
	)
	return p.flagExpansion.marker(expression), true, nil
}

func (p *kbuildParser) kbuildProbeContextExpression(
	kind string,
	depth int,
) (*kbuildFlagExpr, error) {
	var parts []*kbuildFlagExpr
	varNames := []string{}
	switch kind {
	case "cc-option", "cc-option-yn", "cc-disable-warning":
		parts = append(parts, p.flagInterner.literal([]string{"-Werror"}))
		varNames = append(varNames, "KBUILD_CPPFLAGS")
		if _, ok := p.lookupVariable("CC_OPTION_CFLAGS"); ok {
			varNames = append(varNames, "CC_OPTION_CFLAGS")
		} else {
			varNames = append(varNames, "KBUILD_CFLAGS")
		}
	case "as-option":
		parts = append(parts, p.flagInterner.literal([]string{"-Werror"}))
		varNames = append(varNames, "KBUILD_CPPFLAGS", "KBUILD_AFLAGS")
	case "ld-option":
		varNames = append(varNames, "KBUILD_LDFLAGS")
	}
	for _, name := range varNames {
		if expression := p.flagVars[name]; expression != nil {
			parts = append(parts, expression)
			continue
		}
		value, ok, err := p.expandVariable(name, "$("+name+")", depth)
		if err != nil {
			return nil, fmt.Errorf(
				"expand %s for %s symbolic probe: %w",
				name,
				kind,
				err,
			)
		}
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		expression, err := p.kbuildFlagExpressionFromExpanded(value)
		if err != nil {
			return nil, fmt.Errorf(
				"model %s for %s symbolic probe: %w",
				name,
				kind,
				err,
			)
		}
		parts = append(parts, expression)
	}
	return p.flagInterner.concat(parts...), nil
}

func mapKbuildFlagExpression(
	expression *kbuildFlagExpr,
	mapArgv func([]string) []string,
) *kbuildFlagExpr {
	if expression == nil {
		return nil
	}
	interner := newKbuildFlagExprInterner()
	memo := map[string]*kbuildFlagExpr{}
	var visit func(*kbuildFlagExpr) *kbuildFlagExpr
	visit = func(current *kbuildFlagExpr) *kbuildFlagExpr {
		if current == nil {
			return interner.literal(nil)
		}
		if mapped := memo[current.id]; mapped != nil {
			return mapped
		}
		var mapped *kbuildFlagExpr
		switch current.kind {
		case kbuildFlagLiteral:
			mapped = interner.literal(mapArgv(append([]string(nil), current.argv...)))
		case kbuildFlagConcat:
			children := make([]*kbuildFlagExpr, 0, len(current.children))
			for _, child := range current.children {
				children = append(children, visit(child))
			}
			mapped = interner.concat(children...)
		case kbuildFlagSelect:
			probe := current.probe
			probe.candidateArgv = mapArgv(append([]string(nil), probe.candidateArgv...))
			probe.context = visit(probe.context)
			mapped = interner.selectExpr(
				probe,
				visit(current.whenTrue),
				visit(current.whenFalse),
			)
		}
		memo[current.id] = mapped
		return mapped
	}
	return visit(expression)
}

// mapKbuildFlagExpressionOutput applies a Make word transform to values
// produced by a flag program. Probe candidates and their compiler context are
// decision inputs, not emitted words, so an output transform must preserve
// them exactly.
func mapKbuildFlagExpressionOutput(
	expression *kbuildFlagExpr,
	mapArgv func([]string) []string,
) *kbuildFlagExpr {
	if expression == nil {
		return nil
	}
	interner := newKbuildFlagExprInterner()
	memo := map[string]*kbuildFlagExpr{}
	var visit func(*kbuildFlagExpr) *kbuildFlagExpr
	visit = func(current *kbuildFlagExpr) *kbuildFlagExpr {
		if current == nil {
			return interner.literal(nil)
		}
		if mapped := memo[current.id]; mapped != nil {
			return mapped
		}
		var mapped *kbuildFlagExpr
		switch current.kind {
		case kbuildFlagLiteral:
			mapped = interner.literal(mapArgv(append([]string(nil), current.argv...)))
		case kbuildFlagConcat:
			children := make([]*kbuildFlagExpr, 0, len(current.children))
			for _, child := range current.children {
				children = append(children, visit(child))
			}
			mapped = interner.concat(children...)
		case kbuildFlagSelect:
			mapped = interner.selectExpr(
				current.probe,
				visit(current.whenTrue),
				visit(current.whenFalse),
			)
		}
		memo[current.id] = mapped
		return mapped
	}
	return visit(expression)
}

func concatKbuildFlagExpressions(expressions ...*kbuildFlagExpr) *kbuildFlagExpr {
	return newKbuildFlagExprInterner().concat(expressions...)
}

// kbuildFlagExpressionSourcePathFlags computes the union of explicit source
// inputs across every branch without enumerating the cross product of selects.
func kbuildFlagExpressionSourcePathFlags(
	expression *kbuildFlagExpr,
) ([]sourcePathFlag, error) {
	if expression == nil {
		return nil, nil
	}
	options := []string{
		"-idirafter",
		"-iquote",
		"-isystem",
		"-include",
		"-imacros",
		"-I",
	}
	unsupported := []string{
		"-F",
		"-iframework",
		"-iprefix",
		"-iwithprefix",
		"-Wp,-I",
		"-Wp,-include",
		"-Wp,-imacros",
		"--include",
		"--imacros",
		"-Xclang",
	}
	results := map[sourcePathFlag]bool{}
	var walk func(*kbuildFlagExpr, map[string]bool) (map[string]bool, error)
	walk = func(
		current *kbuildFlagExpr,
		pending map[string]bool,
	) (map[string]bool, error) {
		if current == nil {
			return pending, nil
		}
		switch current.kind {
		case kbuildFlagLiteral:
			states := make(map[string]bool, len(pending))
			for state := range pending {
				states[state] = true
			}
			for _, arg := range current.argv {
				next := map[string]bool{}
				for state := range states {
					if state != "" {
						path := strings.TrimSpace(arg)
						if path == "" {
							return nil, fmt.Errorf("%s requires a non-empty path", state)
						}
						results[sourcePathFlag{option: state, path: path}] = true
						next[""] = true
						continue
					}
					matched := false
					for _, option := range options {
						if arg == option {
							next[option] = true
							matched = true
							break
						}
						if path, ok := strings.CutPrefix(arg, option); ok && path != "" {
							path = strings.TrimPrefix(strings.TrimSpace(path), "=")
							if path == "" {
								return nil, fmt.Errorf("%s requires a non-empty path", option)
							}
							results[sourcePathFlag{option: option, path: path}] = true
							next[""] = true
							matched = true
							break
						}
					}
					if matched {
						continue
					}
					for _, prefix := range unsupported {
						if strings.HasPrefix(arg, prefix) {
							return nil, fmt.Errorf(
								"unsupported source include flag %q",
								arg,
							)
						}
					}
					next[""] = true
				}
				states = next
			}
			return states, nil
		case kbuildFlagConcat:
			states := pending
			var err error
			for _, child := range current.children {
				states, err = walk(child, states)
				if err != nil {
					return nil, err
				}
			}
			return states, nil
		case kbuildFlagSelect:
			probeStates, err := walk(
				current.probe.context,
				map[string]bool{"": true},
			)
			if err != nil {
				return nil, err
			}
			probeStates, err = walk(
				&kbuildFlagExpr{
					kind: kbuildFlagLiteral,
					argv: current.probe.candidateArgv,
				},
				probeStates,
			)
			if err != nil {
				return nil, err
			}
			for state := range probeStates {
				if state != "" {
					return nil, fmt.Errorf(
						"%s probe context/candidate requires a path",
						state,
					)
				}
			}
			whenTrue, err := walk(current.whenTrue, pending)
			if err != nil {
				return nil, err
			}
			whenFalse, err := walk(current.whenFalse, pending)
			if err != nil {
				return nil, err
			}
			for state := range whenFalse {
				whenTrue[state] = true
			}
			return whenTrue, nil
		default:
			return nil, fmt.Errorf("unknown symbolic Kbuild flag expression kind %d", current.kind)
		}
	}
	states, err := walk(expression, map[string]bool{"": true})
	if err != nil {
		return nil, err
	}
	for state := range states {
		if state != "" {
			return nil, fmt.Errorf("%s requires a path", state)
		}
	}
	out := make([]sourcePathFlag, 0, len(results))
	for flag := range results {
		out = append(out, flag)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].option != out[j].option {
			return out[i].option < out[j].option
		}
		return out[i].path < out[j].path
	})
	return out, nil
}

func appendKbuildFlagExpressionSourceInputs(
	flags []string,
	expression *kbuildFlagExpr,
) ([]string, error) {
	pathFlags, err := kbuildFlagExpressionSourcePathFlags(expression)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), flags...)
	for _, flag := range pathFlags {
		out = append(out, flag.option+flag.path)
	}
	return out, nil
}
