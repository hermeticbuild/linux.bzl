package kconfig

// This file is the shared discovery/runtime implementation for the proven
// ASCII subset of pure GNU Make transformations used by compiler-derived
// Kbuild values. Discovery records the function and its literal arguments;
// proberun supplies the completed text result and calls the same evaluator
// used by Kbuild replay. Every accepted operation is bounded before evaluation
// so an expanding Make function cannot allocate an unbounded result in a
// remote action.

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Planner-generated symbolic transform chains are already bounded to a depth
// of 32. Keep both the fragment-chain and request-wide wire limit close to
// that bound so a hostile request cannot repeatedly rescan a near-1 MiB
// dependency result thousands of times.
const maxProbeValueTransforms = 64
const maxProbeValueTransformWordOperations = 1 << 20

// ProbeValueTransform applies one context-free GNU Make function to a value
// fragment. Arguments contains one empty slot at InputArgument; the runner
// replaces that slot with the preceding value. Transforms are applied in
// order, which represents arbitrary chains rooted in one measured text
// result without exposing planner-only symbolic tokens to an action.
type ProbeValueTransform struct {
	Function          string                                 `json:"function"`
	Arguments         []string                               `json:"arguments"`
	InputArgument     int                                    `json:"input_argument"`
	ArgumentFragments []ProbeValueTransformArgumentFragments `json:"argument_fragments,omitempty"`
}

func pureKbuildMakeFunctionArity(function string, count int) bool {
	switch function {
	case "subst", "patsubst", "wordlist":
		return count == 3
	case "addprefix", "addsuffix", "filter", "filter-out", "findstring", "join", "word":
		return count == 2
	case "basename", "dir", "firstword", "lastword", "notdir", "sort", "strip", "suffix", "words":
		return count == 1
	case "intcmp":
		return count >= 2 && count <= 5
	default:
		return false
	}
}

func supportedProbeValueTransformFunction(function string) bool {
	switch function {
	case "subst", "addprefix", "addsuffix", "dir", "filter", "filter-out", "findstring", "patsubst", "sort", "strip":
		return true
	default:
		return false
	}
}

func validateProbeValueTransformText(function string, arguments []string) error {
	for argumentIndex, argument := range arguments {
		for _, character := range []byte(argument) {
			if character == 0 || character >= utf8.RuneSelf || character == '\r' || character == '\n' || character == '\v' || character == '\f' {
				return fmt.Errorf("pure Make function %q argument %d is outside the proven ASCII Make text grammar", function, argumentIndex)
			}
		}
	}
	switch function {
	case "subst":
		if arguments[0] == "" {
			return fmt.Errorf("pure Make function %q requires a nonempty search string", function)
		}
	case "addprefix", "addsuffix":
		if strings.ContainsAny(arguments[0], " \t") {
			return fmt.Errorf("pure Make function %q requires a separator-free affix", function)
		}
	case "filter", "filter-out":
		if strings.Contains(arguments[0], "\\") {
			return fmt.Errorf("pure Make function %q does not accept escaped protocol patterns", function)
		}
	case "patsubst":
		if strings.Contains(arguments[0], "\\") || strings.Contains(arguments[1], "\\") ||
			strings.ContainsAny(arguments[0], " \t") || strings.ContainsAny(arguments[1], " \t") {
			return fmt.Errorf("pure Make function %q requires simple separator-free protocol patterns", function)
		}
	}
	return nil
}

func validateProbeValueTransformShape(transform ProbeValueTransform) error {
	if !pureKbuildMakeFunctionArity(transform.Function, len(transform.Arguments)) {
		return fmt.Errorf("unsupported pure Make function %q with %d arguments", transform.Function, len(transform.Arguments))
	}
	if !supportedProbeValueTransformFunction(transform.Function) {
		return fmt.Errorf("pure Make function %q is not in the proven probe value transform subset", transform.Function)
	}
	if transform.InputArgument < 0 || transform.InputArgument >= len(transform.Arguments) {
		return fmt.Errorf("pure Make function %q has invalid input argument %d", transform.Function, transform.InputArgument)
	}
	if transform.Arguments[transform.InputArgument] != "" {
		return fmt.Errorf("pure Make function %q input argument %d is not an empty value slot", transform.Function, transform.InputArgument)
	}
	dynamicArguments := make(map[int]bool, len(transform.ArgumentFragments))
	previousDynamicArgument := -1
	for groupIndex, group := range transform.ArgumentFragments {
		if group.Index < 0 || group.Index >= len(transform.Arguments) || group.Index <= previousDynamicArgument {
			return fmt.Errorf("pure Make function %q dynamic argument group %d has an invalid or unsorted index", transform.Function, groupIndex)
		}
		if group.Index == transform.InputArgument {
			return fmt.Errorf("pure Make function %q dynamic argument group %d replaces the transform input", transform.Function, groupIndex)
		}
		if transform.Arguments[group.Index] != "" || len(group.Fragments) == 0 {
			return fmt.Errorf("pure Make function %q dynamic argument group %d must replace one empty argument", transform.Function, groupIndex)
		}
		dynamicArguments[group.Index] = true
		previousDynamicArgument = group.Index
	}
	for index, argument := range transform.Arguments {
		if strings.ContainsRune(argument, 0) {
			return fmt.Errorf("pure Make function %q argument %d contains NUL", transform.Function, index)
		}
		if linuxProbeSymbolPattern.MatchString(argument) {
			return fmt.Errorf("pure Make function %q argument %d contains a planner-only probe token", transform.Function, index)
		}
	}
	staticArguments := slices.Clone(transform.Arguments)
	// Use a benign nonempty ASCII word for the dynamic slot so static source
	// shape checks remain meaningful without guessing the measured value.
	staticArguments[transform.InputArgument] = "x"
	for index := range dynamicArguments {
		staticArguments[index] = "x"
	}
	if err := validateProbeValueTransformText(transform.Function, staticArguments); err != nil {
		return err
	}
	return nil
}

func addProbeTransformBound(total, amount int) (int, bool) {
	if amount < 0 || total < 0 || total > MaxProbeInterpolatedBytes-amount {
		return 0, false
	}
	return total + amount, true
}

func multiplyProbeTransformBound(left, right int) (int, bool) {
	if left < 0 || right < 0 || (left != 0 && right > MaxProbeInterpolatedBytes/left) {
		return 0, false
	}
	return left * right, true
}

func probeTransformWordBytes(words []string) (int, bool) {
	total := 0
	for index, word := range words {
		if index != 0 {
			var ok bool
			total, ok = addProbeTransformBound(total, 1)
			if !ok {
				return 0, false
			}
		}
		var ok bool
		total, ok = addProbeTransformBound(total, len(word))
		if !ok {
			return 0, false
		}
	}
	return total, true
}

func probeTransformWordCount(value string) (int, error) {
	words := 0
	inWord := false
	for _, character := range value {
		if unicode.IsSpace(character) {
			inWord = false
			continue
		}
		if !inWord {
			words++
			if words > MaxProbeDynamicArgumentWords {
				return 0, fmt.Errorf("value exceeds %d Make words", MaxProbeDynamicArgumentWords)
			}
			inWord = true
		}
	}
	return words, nil
}

// probeValueTransformOutputBound rejects an invocation before the ordinary
// exact evaluator can allocate more than MaxProbeInterpolatedBytes. The
// bounds are exact or conservative for every function accepted by the probe
// value transform subset.
func probeValueTransformOutputBound(function string, arguments []string) error {
	fail := func() error {
		return fmt.Errorf("pure Make function %q result may exceed %d bytes", function, MaxProbeInterpolatedBytes)
	}
	check := func(size int, ok bool) error {
		if !ok || size > MaxProbeInterpolatedBytes {
			return fail()
		}
		return nil
	}
	switch function {
	case "subst":
		old, replacement, input := arguments[0], arguments[1], arguments[2]
		if old == "" {
			return check(addProbeTransformBound(len(input), len(replacement)))
		}
		matches := strings.Count(input, old)
		removed, ok := multiplyProbeTransformBound(matches, len(old))
		if !ok || removed > len(input) {
			return fail()
		}
		added, ok := multiplyProbeTransformBound(matches, len(replacement))
		if !ok {
			return fail()
		}
		return check(addProbeTransformBound(len(input)-removed, added))
	case "addprefix", "addsuffix":
		words := strings.Fields(arguments[1])
		base, ok := probeTransformWordBytes(words)
		if !ok {
			return fail()
		}
		extra, ok := multiplyProbeTransformBound(len(words), len(strings.TrimSpace(arguments[0])))
		if !ok {
			return fail()
		}
		return check(addProbeTransformBound(base, extra))
	case "basename", "notdir", "sort", "strip", "suffix", "firstword", "lastword":
		return check(len(arguments[0]), true)
	case "dir":
		words := strings.Fields(arguments[0])
		base, ok := probeTransformWordBytes(words)
		if !ok {
			return fail()
		}
		extra, ok := multiplyProbeTransformBound(len(words), 2)
		if !ok {
			return fail()
		}
		return check(addProbeTransformBound(base, extra))
	case "filter", "filter-out":
		return check(len(arguments[1]), true)
	case "findstring":
		return check(len(arguments[0]), true)
	case "intcmp":
		maximum := 0
		for _, argument := range arguments[2:] {
			if len(argument) > maximum {
				maximum = len(argument)
			}
		}
		return check(maximum, true)
	case "join":
		left, right := strings.Fields(arguments[0]), strings.Fields(arguments[1])
		leftBytes, leftOK := probeTransformWordBytes(left)
		rightBytes, rightOK := probeTransformWordBytes(right)
		if !leftOK || !rightOK {
			return fail()
		}
		// Removing each side's separators and adding at most max(words)-1
		// output separators is never larger than this conservative sum.
		total, ok := addProbeTransformBound(leftBytes, rightBytes)
		if !ok {
			return fail()
		}
		separators := max(len(left), len(right))
		if separators != 0 {
			separators--
		}
		return check(addProbeTransformBound(total, separators))
	case "patsubst":
		pattern := strings.TrimSpace(arguments[0])
		replacement := strings.TrimSpace(arguments[1])
		total := 0
		words := strings.Fields(arguments[2])
		for index, word := range words {
			if index != 0 {
				var ok bool
				total, ok = addProbeTransformBound(total, 1)
				if !ok {
					return fail()
				}
			}
			mappedBytes := len(word)
			if makePatternMatch(pattern, word) {
				// patsubst substitutes at most one unescaped percent, so the
				// replacement plus the complete input word is a safe bound.
				var ok bool
				mappedBytes, ok = addProbeTransformBound(len(replacement), len(word))
				if !ok {
					return fail()
				}
			}
			var ok bool
			total, ok = addProbeTransformBound(total, mappedBytes)
			if !ok {
				return fail()
			}
		}
		return nil
	case "word":
		return check(len(arguments[1]), true)
	case "wordlist":
		return check(len(arguments[2]), true)
	case "words":
		return nil
	default:
		return fmt.Errorf("unsupported pure Make function %q", function)
	}
}

func evalPureKbuildTextTransform(function string, arguments []string) (string, bool, error) {
	if function == "intcmp" {
		if _, ok := makeIntcmp(arguments); !ok {
			// Ordinary Make expansion preserves the original reference for an
			// invalid intcmp. A dependency-backed reference cannot preserve
			// that syntax after execution, so reject it instead of returning a
			// phase-dependent value.
			return "", true, fmt.Errorf("pure Make function %q requires numeric operands", function)
		}
	}
	return evalPureKbuildMakeFunction(function, arguments, "")
}

// ApplyProbeValueTransform applies one validated protocol transform. The
// caller expands protocol templates in Arguments first, leaving the input
// slot empty. The exact result is produced by evalPureKbuildMakeFunction,
// shared with discovery replay for the protocol's proven source shapes.
func ApplyProbeValueTransform(transform ProbeValueTransform, input string) (string, error) {
	if len(transform.ArgumentFragments) != 0 {
		return "", fmt.Errorf("pure Make function %q has unresolved dynamic arguments", transform.Function)
	}
	if err := validateProbeValueTransformShape(transform); err != nil {
		return "", err
	}
	if len(input) > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("pure Make function %q input exceeds %d bytes", transform.Function, MaxProbeInterpolatedBytes)
	}
	arguments := slices.Clone(transform.Arguments)
	argumentBytes := len(input)
	for index, argument := range arguments {
		if index == transform.InputArgument {
			arguments[index] = input
			continue
		}
		var ok bool
		argumentBytes, ok = addProbeTransformBound(argumentBytes, len(argument))
		if !ok {
			return "", fmt.Errorf("pure Make function %q arguments exceed %d bytes", transform.Function, MaxProbeInterpolatedBytes)
		}
	}
	if err := validateProbeValueTransformText(transform.Function, arguments); err != nil {
		return "", err
	}
	return applyBoundedPureKbuildTextTransform(transform.Function, arguments)
}

// applyBoundedPureKbuildTextTransform is shared by protocol execution and
// opaque make-text replay. Callers select the permitted function grammar;
// this helper supplies common allocation, word, and comparison bounds around
// the exact pure GNU Make evaluator.
func applyBoundedPureKbuildTextTransform(function string, arguments []string) (string, error) {
	argumentBytes := 0
	for _, argument := range arguments {
		if len(argument) > MaxProbeInterpolatedBytes || argumentBytes > MaxProbeInterpolatedBytes-len(argument) {
			return "", fmt.Errorf("pure Make function %q arguments exceed %d bytes", function, MaxProbeInterpolatedBytes)
		}
		argumentBytes += len(argument)
	}
	totalWords := 0
	wordCounts := make([]int, len(arguments))
	for index, argument := range arguments {
		count, err := probeTransformWordCount(argument)
		if err != nil {
			return "", fmt.Errorf("pure Make function %q argument %d: %w", function, index, err)
		}
		if totalWords > MaxProbeDynamicArgumentWords-count {
			return "", fmt.Errorf("pure Make function %q arguments exceed %d Make words", function, MaxProbeDynamicArgumentWords)
		}
		totalWords += count
		wordCounts[index] = count
	}
	if function == "filter" || function == "filter-out" {
		if wordCounts[0] != 0 && wordCounts[1] > maxProbeValueTransformWordOperations/wordCounts[0] {
			return "", fmt.Errorf("pure Make function %q exceeds %d pattern comparisons", function, maxProbeValueTransformWordOperations)
		}
	}
	if err := probeValueTransformOutputBound(function, arguments); err != nil {
		return "", err
	}
	value, recognized, err := evalPureKbuildTextTransform(function, arguments)
	if err != nil {
		return "", err
	}
	if !recognized {
		return "", fmt.Errorf("unsupported pure Make function %q", function)
	}
	if len(value) > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("pure Make function %q result exceeds %d bytes", function, MaxProbeInterpolatedBytes)
	}
	if _, err := probeTransformWordCount(value); err != nil {
		return "", fmt.Errorf("pure Make function %q result: %w", function, err)
	}
	return value, nil
}
