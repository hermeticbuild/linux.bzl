package kconfig

import (
	"fmt"
	"slices"
	"strings"
)

// probeSymbolicValueLowerer owns one request's result ordinals. It lowers
// planner-only probe atoms into protocol predicates and ${result:...}
// references so no LINUX_BZL_PROBE_* token can reach an executed process.
type probeSymbolicValueLowerer struct {
	evaluator    *LinuxProbeEvaluator
	dependencies []ProbeReference
	ordinals     map[ProbeReference]int
}

type probeSymbolicValueMode int

const (
	probeSymbolicValueExact probeSymbolicValueMode = iota
	probeSymbolicValueArgv
)

type probeSymbolicValueBudget struct {
	fragments  int
	transforms int
	bytes      int
	work       int
}

func (b *probeSymbolicValueBudget) step() error {
	if b.work >= maxProbeValueFragments {
		return fmt.Errorf("symbolic process value expansion exceeds %d work items", maxProbeValueFragments)
	}
	b.work++
	return nil
}

func (b *probeSymbolicValueBudget) addFragment(fragment ProbeValueFragment) error {
	if b.fragments >= maxProbeValueFragments {
		return fmt.Errorf("symbolic process value has more than %d fragments", maxProbeValueFragments)
	}
	if len(fragment.Value) > MaxProbeInterpolatedBytes-b.bytes {
		return fmt.Errorf("symbolic process value exceeds %d literal bytes", MaxProbeInterpolatedBytes)
	}
	b.fragments++
	b.bytes += len(fragment.Value)
	return nil
}

func (b *probeSymbolicValueBudget) addTransform(fragment ProbeValueFragment, transform ProbeValueTransform) error {
	if len(fragment.Transforms) >= maxProbeValueTransforms || b.transforms >= maxProbeValueTransforms {
		return fmt.Errorf("symbolic process value has more than %d transforms", maxProbeValueTransforms)
	}
	bytes := len(transform.Function)
	for _, argument := range transform.Arguments {
		if len(argument) > MaxProbeInterpolatedBytes-bytes {
			return fmt.Errorf("symbolic process value exceeds %d literal bytes", MaxProbeInterpolatedBytes)
		}
		bytes += len(argument)
	}
	if bytes > MaxProbeInterpolatedBytes-b.bytes {
		return fmt.Errorf("symbolic process value exceeds %d literal bytes", MaxProbeInterpolatedBytes)
	}
	b.transforms++
	b.bytes += bytes
	return nil
}

func newProbeSymbolicValueLowerer(e *LinuxProbeEvaluator) *probeSymbolicValueLowerer {
	return &probeSymbolicValueLowerer{evaluator: e, ordinals: map[ProbeReference]int{}}
}

func (l *probeSymbolicValueLowerer) reference(reference ProbeReference) int {
	if ordinal, exists := l.ordinals[reference]; exists {
		return ordinal
	}
	ordinal := len(l.dependencies)
	l.ordinals[reference] = ordinal
	l.dependencies = append(l.dependencies, reference)
	return ordinal
}

func (l *probeSymbolicValueLowerer) resultPredicate(reference ProbeReference, value bool) ProbePredicate {
	operator := "result-false"
	if value {
		operator = "result-true"
	}
	return ProbePredicate{Operator: operator, Result: fmt.Sprintf("%08d", l.reference(reference))}
}

func andProbeValuePredicate(outer *ProbePredicate, inner ProbePredicate) *ProbePredicate {
	if outer == nil {
		copy := inner
		return &copy
	}
	operands := make([]ProbePredicate, 0, 4)
	if outer.Operator == "all" {
		operands = append(operands, outer.Operands...)
	} else {
		operands = append(operands, *outer)
	}
	if inner.Operator == "all" {
		operands = append(operands, inner.Operands...)
	} else {
		operands = append(operands, inner)
	}
	return &ProbePredicate{Operator: "all", Operands: operands}
}

func (l *probeSymbolicValueLowerer) value(value string) ([]ProbeValueFragment, bool, error) {
	return l.valueForMode(value, probeSymbolicValueExact)
}

func (l *probeSymbolicValueLowerer) valueForMode(value string, mode probeSymbolicValueMode) ([]ProbeValueFragment, bool, error) {
	if !linuxProbeSymbolPattern.MatchString(value) {
		return nil, false, nil
	}
	budget := &probeSymbolicValueBudget{}
	fragments, err := l.valueWhen(value, nil, 0, map[string]bool{}, mode, budget)
	if err != nil {
		return nil, true, err
	}
	totalBytes := 0
	totalTransforms := 0
	totalFragments := 0
	var inspect func([]ProbeValueFragment, int) error
	inspect = func(values []ProbeValueFragment, depth int) error {
		if depth > MaxProbeValueFragmentDepth {
			return fmt.Errorf("symbolic process value exceeds aggregate depth %d", MaxProbeValueFragmentDepth)
		}
		for fragmentIndex, fragment := range values {
			totalFragments++
			if totalFragments > maxProbeValueFragments {
				return fmt.Errorf("symbolic process value has more than %d fragments", maxProbeValueFragments)
			}
			if len(fragment.Transforms) > maxProbeValueTransforms {
				return fmt.Errorf("symbolic process value fragment %d has %d transforms, maximum is %d", fragmentIndex, len(fragment.Transforms), maxProbeValueTransforms)
			}
			totalBytes += len(fragment.Value)
			for _, transform := range fragment.Transforms {
				totalTransforms++
				totalBytes += len(transform.Function)
				for _, argument := range transform.Arguments {
					totalBytes += len(argument)
				}
				for _, group := range transform.ArgumentFragments {
					if err := inspect(group.Fragments, depth+1); err != nil {
						return err
					}
				}
			}
			if len(fragment.Fragments) != 0 {
				if err := inspect(fragment.Fragments, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := inspect(fragments, 0); err != nil {
		return nil, true, err
	}
	if totalTransforms > maxProbeValueTransforms {
		return nil, true, fmt.Errorf("symbolic process value has %d transforms, maximum is %d", totalTransforms, maxProbeValueTransforms)
	}
	if totalBytes > MaxProbeInterpolatedBytes {
		return nil, true, fmt.Errorf("symbolic process value has %d literal bytes, maximum is %d", totalBytes, MaxProbeInterpolatedBytes)
	}
	return fragments, true, nil
}

func (l *probeSymbolicValueLowerer) valueWhen(
	value string,
	when *ProbePredicate,
	depth int,
	visiting map[string]bool,
	mode probeSymbolicValueMode,
	budget *probeSymbolicValueBudget,
) ([]ProbeValueFragment, error) {
	if depth > maxKbuildSymbolicComparisonDepth {
		return nil, fmt.Errorf("symbolic process value expansion is too deep")
	}
	if len(value) > maxKbuildSymbolicComparisonBytes {
		return nil, fmt.Errorf("symbolic process value exceeds %d bytes", maxKbuildSymbolicComparisonBytes)
	}
	matches := linuxProbeSymbolPattern.FindAllStringIndex(value, -1)
	if len(matches) == 0 {
		if value == "" {
			return nil, nil
		}
		fragment := ProbeValueFragment{Value: value, When: when}
		if err := budget.addFragment(fragment); err != nil {
			return nil, err
		}
		return []ProbeValueFragment{fragment}, nil
	}
	fragments := make([]ProbeValueFragment, 0, len(matches)*2+1)
	start := 0
	for _, match := range matches {
		if match[0] > start {
			fragment := ProbeValueFragment{Value: value[start:match[0]], When: when}
			if err := budget.addFragment(fragment); err != nil {
				return nil, err
			}
			fragments = append(fragments, fragment)
		}
		if err := budget.step(); err != nil {
			return nil, err
		}
		token := value[match[0]:match[1]]
		if visiting[token] {
			return nil, fmt.Errorf("cyclic Linux probe symbolic value %q in process value", token)
		}
		symbol, exists, adoptErr := l.evaluator.adoptSymbol(token)
		if adoptErr != nil {
			return nil, adoptErr
		}
		if !exists {
			return nil, fmt.Errorf("unknown Linux probe symbolic value %q in process value", token)
		}
		visiting[token] = true
		var expanded []ProbeValueFragment
		var err error
		switch symbol.kind {
		case "boolean":
			for _, branch := range []struct {
				selected bool
				text     string
			}{{selected: true, text: symbol.trueText}, {selected: false, text: symbol.falseText}} {
				if stepErr := budget.step(); stepErr != nil {
					err = stepErr
					break
				}
				predicate := andProbeValuePredicate(when, l.resultPredicate(symbol.reference, branch.selected))
				branchFragments, branchErr := l.valueWhen(branch.text, predicate, depth+1, visiting, mode, budget)
				if branchErr != nil {
					err = branchErr
					break
				}
				expanded = append(expanded, branchFragments...)
			}
		case "selection":
			for _, input := range symbol.selectionInputs {
				l.reference(input.reference)
			}
			for state, selected := range symbol.selectionValues {
				if stepErr := budget.step(); stepErr != nil {
					err = stepErr
					break
				}
				predicate := andProbeValuePredicate(when, probeSelectionStatePredicate(symbol.selectionInputs, state, l.ordinals))
				stateFragments, stateErr := l.valueWhen(selected, predicate, depth+1, visiting, mode, budget)
				if stateErr != nil {
					err = stateErr
					break
				}
				expanded = append(expanded, stateFragments...)
			}
		case "text":
			ordinal := l.reference(symbol.reference)
			fragment := ProbeValueFragment{
				Value: fmt.Sprintf("${result:%08d.text}", ordinal), When: when,
			}
			if fragmentErr := budget.addFragment(fragment); fragmentErr != nil {
				err = fragmentErr
				break
			}
			expanded = append(expanded, fragment)
		case "transformed-text":
			if symbol.textTransform == nil {
				err = fmt.Errorf("transformed Linux text probe value %q has no transform", token)
				break
			}
			transformed := symbol.textTransform
			sourceFragments, sourceErr := l.valueWhen(transformed.sourceToken, when, depth+1, visiting, mode, budget)
			if sourceErr != nil {
				err = sourceErr
				break
			}
			if len(sourceFragments) != 1 {
				err = fmt.Errorf("transformed Linux text probe value %q has %d source fragments, want one", token, len(sourceFragments))
				break
			}
			arguments := slices.Clone(transformed.arguments)
			if transformed.inputArgument < 0 || transformed.inputArgument >= len(arguments) || arguments[transformed.inputArgument] != transformed.sourceToken {
				err = fmt.Errorf("transformed Linux text probe value %q has an invalid input argument", token)
				break
			}
			arguments[transformed.inputArgument] = ""
			protocolTransform := ProbeValueTransform{
				Function: transformed.function, Arguments: arguments, InputArgument: transformed.inputArgument,
			}
			if transformErr := validateProbeValueTransformShape(protocolTransform); transformErr != nil {
				err = fmt.Errorf("transformed Linux text probe value %q: %w", token, transformErr)
				break
			}
			fragment := sourceFragments[0]
			if transformErr := budget.addTransform(fragment, protocolTransform); transformErr != nil {
				err = transformErr
				break
			}
			fragment.Transforms = append(slices.Clone(fragment.Transforms), protocolTransform)
			expanded = []ProbeValueFragment{fragment}
		case "make-text":
			if symbol.makeText == nil {
				err = fmt.Errorf("whole Make text symbolic value %q has no expression", token)
				break
			}
			if symbol.makeText.protocolMode == linuxProbeMakeTextProtocolUnusable {
				err = fmt.Errorf("whole Make text symbolic value %q has no proven protocol lowering", token)
				break
			}
			if len(symbol.makeText.protocolTransforms) != 0 {
				expanded, err = l.lowerMakeTextProtocolTransforms(
					symbol.makeText, when, depth, visiting, budget,
				)
				break
			}
			protocolMode := mode
			aggregate := false
			if mode != probeSymbolicValueArgv && symbol.makeText.protocolMode == linuxProbeMakeTextProtocolCanonicalWords {
				// argv-word equivalence becomes byte-exact only after normalizing
				// the complete protocol value. Keep surrounding literals outside
				// this aggregate and apply strip after all conditional children
				// have been concatenated.
				protocolMode = probeSymbolicValueArgv
				aggregate = true
			}
			if mode != probeSymbolicValueArgv && symbol.makeText.protocolMode == linuxProbeMakeTextProtocolArgvWords {
				err = fmt.Errorf("whole Make text symbolic value %q is argv-word-equivalent but not exact for environment or stdin lowering", token)
				break
			}
			expanded, err = l.valueWhen(symbol.makeText.protocolValue, when, depth+1, visiting, protocolMode, budget)
			if err == nil && aggregate && len(expanded) != 0 {
				transform := ProbeValueTransform{Function: "strip", Arguments: []string{""}, InputArgument: 0}
				group := ProbeValueFragment{Fragments: expanded}
				if transformErr := budget.addTransform(group, transform); transformErr != nil {
					err = transformErr
					break
				}
				group.Transforms = []ProbeValueTransform{transform}
				if fragmentErr := budget.addFragment(group); fragmentErr != nil {
					err = fragmentErr
					break
				}
				expanded = []ProbeValueFragment{group}
			}
		default:
			err = fmt.Errorf("Linux probe symbolic value %q has unsupported kind %q", token, symbol.kind)
		}
		delete(visiting, token)
		if err != nil {
			return nil, err
		}
		fragments = append(fragments, expanded...)
		start = match[1]
	}
	if start < len(value) {
		fragment := ProbeValueFragment{Value: value[start:], When: when}
		if err := budget.addFragment(fragment); err != nil {
			return nil, err
		}
		fragments = append(fragments, fragment)
	}
	return fragments, nil
}

func probeProtocolTransformValueMode(function string) probeSymbolicValueMode {
	if function != "subst" && function != "findstring" {
		// Every supported runtime transform except subst and findstring observes
		// its input as GNU Make words. An argv-word-equivalent nested protocol is
		// therefore sufficient; the transform itself emits the exact canonical
		// result. The two bytewise functions require exact fragment text.
		return probeSymbolicValueArgv
	}
	return probeSymbolicValueExact
}

func (l *probeSymbolicValueLowerer) lowerMakeTextProtocolTransforms(
	makeText *linuxProbeMakeText,
	when *ProbePredicate,
	depth int,
	visiting map[string]bool,
	budget *probeSymbolicValueBudget,
) ([]ProbeValueFragment, error) {
	if makeText == nil || len(makeText.protocolTransforms) == 0 {
		return nil, fmt.Errorf("whole Make text has no dynamic protocol transforms")
	}
	sourceMode := probeProtocolTransformValueMode(makeText.protocolTransforms[0].function)
	expanded, err := l.valueWhen(makeText.protocolValue, when, depth+1, visiting, sourceMode, budget)
	if err != nil {
		return nil, err
	}
	for transformIndex, plannerTransform := range makeText.protocolTransforms {
		if len(expanded) == 0 {
			// Every accepted protocol transform is zero-preserving; avoid an
			// invalid empty aggregate when the enclosing branch is empty.
			continue
		}
		transform := ProbeValueTransform{
			Function: plannerTransform.function, Arguments: slices.Clone(plannerTransform.arguments),
			InputArgument: plannerTransform.inputArgument,
		}
		argumentMode := probeProtocolTransformValueMode(plannerTransform.function)
		for argumentIndex, argument := range transform.Arguments {
			if argumentIndex == transform.InputArgument || !linuxProbeSymbolPattern.MatchString(argument) {
				continue
			}
			argumentFragments, lowerErr := l.valueWhen(argument, when, depth+1, visiting, argumentMode, budget)
			if lowerErr != nil {
				return nil, fmt.Errorf("lower whole Make text protocol transform %d argument %d: %w", transformIndex, argumentIndex, lowerErr)
			}
			transform.Arguments[argumentIndex] = ""
			if len(argumentFragments) != 0 {
				transform.ArgumentFragments = append(transform.ArgumentFragments, ProbeValueTransformArgumentFragments{
					Index: argumentIndex, Fragments: argumentFragments,
				})
			}
		}
		group := ProbeValueFragment{Fragments: expanded}
		if transformErr := budget.addTransform(group, transform); transformErr != nil {
			return nil, transformErr
		}
		group.Transforms = []ProbeValueTransform{transform}
		if fragmentErr := budget.addFragment(group); fragmentErr != nil {
			return nil, fragmentErr
		}
		expanded = []ProbeValueFragment{group}
	}
	return expanded, nil
}

type probeSymbolicArgumentBranch struct {
	state     int
	arguments []string
}

type probeSymbolicArgumentExpansion struct {
	inputs      []linuxProbeSelectionInput
	branches    []probeSymbolicArgumentBranch
	dynamicText bool
}

// expandProbeSymbolicArgument is the shared finite-graph boundary for probe
// argv validation and lowering. Each source argument is expanded independently
// so many unrelated top-level flags remain linear, while nested boolean and
// selection values are resolved completely before either consumer sees them.
func expandProbeSymbolicArgument(evaluator *LinuxProbeEvaluator, argument string) (probeSymbolicArgumentExpansion, error) {
	for _, token := range linuxProbeSymbolPattern.FindAllString(argument, -1) {
		if _, exists, err := evaluator.adoptSymbol(token); err != nil {
			return probeSymbolicArgumentExpansion{}, err
		} else if !exists {
			return probeSymbolicArgumentExpansion{}, fmt.Errorf("unknown Linux probe symbolic value %q in process argument", token)
		}
	}
	scope := evaluator.scope
	if scope != "target" && scope != "host" {
		// Unit-level evaluator seams predate explicit toolset scopes. Their
		// references are target-owned; keep the helper usable while production
		// evaluators continue to require an explicit scope.
		scope = "target"
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{scope: evaluator}}
	bindings, symbols, hasText, err := scopes.collectSymbolicComparison(argument)
	if err != nil {
		return probeSymbolicArgumentExpansion{}, err
	}
	if hasText {
		return probeSymbolicArgumentExpansion{dynamicText: true}, nil
	}
	if len(bindings) == 0 || len(bindings) > maxKbuildSymbolicComparisonReferences {
		return probeSymbolicArgumentExpansion{}, fmt.Errorf(
			"symbolic process argument has %d independent probe results, maximum is %d",
			len(bindings), maxKbuildSymbolicComparisonReferences,
		)
	}

	expansion := probeSymbolicArgumentExpansion{
		inputs:   make([]linuxProbeSelectionInput, len(bindings)),
		branches: make([]probeSymbolicArgumentBranch, 0, 1<<len(bindings)),
	}
	for index, binding := range bindings {
		if binding.evaluator != evaluator {
			return probeSymbolicArgumentExpansion{}, fmt.Errorf("symbolic process argument crosses configured compiler scopes")
		}
		expansion.inputs[index] = binding.input
	}
	for state := 0; state < 1<<len(bindings); state++ {
		expanded, err := expandSymbolicComparison(argument, state, bindings, symbols)
		if err != nil {
			return probeSymbolicArgumentExpansion{}, err
		}
		expansion.branches = append(expansion.branches, probeSymbolicArgumentBranch{
			state: state, arguments: strings.Fields(expanded),
		})
	}
	return expansion, nil
}

// arguments preserves shell-style field splitting of each selected Make
// value. One source argument is expanded independently, so a long flag list
// stays linear while nested branch atoms are fully resolved before execution.
func (l *probeSymbolicValueLowerer) arguments(arguments []string) ([]string, []ProbeConditionalArguments, []ProbeArgumentFragments, error) {
	base := make([]string, 0, len(arguments))
	var conditional []ProbeConditionalArguments
	var argumentFragments []ProbeArgumentFragments
	for _, argument := range arguments {
		if !linuxProbeSymbolPattern.MatchString(argument) {
			base = append(base, argument)
			continue
		}
		if len(argument) == len(linuxProbeSymbolPrefix)+linuxProbeSymbolDigestLength {
			symbol, exists, err := l.evaluator.adoptSymbol(argument)
			if err != nil {
				return nil, nil, nil, err
			}
			if exists && symbol.kind == "source-shell-words" {
				// Complete Make evaluation precedes shell word formation. Neither
				// finite-branch strings.Fields nor argv-only Make equivalence is
				// sufficient when the retained text contains shell quoting.
				fragments, symbolic, err := l.valueForMode(symbol.sourceShellWords, probeSymbolicValueExact)
				if err != nil {
					return nil, nil, nil, err
				}
				if !symbolic || len(fragments) == 0 {
					return nil, nil, nil, fmt.Errorf("source shell words have no exact symbolic expression")
				}
				index := len(base)
				base = append(base, "")
				argumentFragments = append(argumentFragments, ProbeArgumentFragments{
					Index: index, Mode: ProbeArgumentFragmentsModeSourceShellWords, Fragments: fragments,
				})
				continue
			}
		}
		expansion, err := expandProbeSymbolicArgument(l.evaluator, argument)
		if err != nil {
			return nil, nil, nil, err
		}
		if expansion.dynamicText {
			fragments, symbolic, err := l.valueForMode(argument, probeSymbolicValueArgv)
			if err != nil {
				return nil, nil, nil, err
			}
			if !symbolic || len(fragments) == 0 {
				return nil, nil, nil, fmt.Errorf("Linux text probe argument did not produce dynamic fragments: %q", argument)
			}
			index := len(base)
			base = append(base, "")
			argumentFragments = append(argumentFragments, ProbeArgumentFragments{Index: index, Fragments: fragments})
			continue
		}
		for _, input := range expansion.inputs {
			l.reference(input.reference)
		}
		for _, branch := range expansion.branches {
			if len(branch.arguments) == 0 {
				continue
			}
			conditional = append(conditional, ProbeConditionalArguments{
				Before: len(base), When: probeSelectionStatePredicate(expansion.inputs, branch.state, l.ordinals), Arguments: branch.arguments,
			})
		}
	}
	return base, conditional, argumentFragments, nil
}
