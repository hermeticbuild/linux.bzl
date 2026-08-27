package kconfig

import (
	"fmt"
	"strings"
)

// RenderProbeDependencyFragments evaluates the pure fragment grammar used by
// a derived text outcome. Both proberun and planner replay call this function,
// so the bytes accepted from the configured action are independently
// reproduced from the same declared dependency results.
func RenderProbeDependencyFragments(fragments []ProbeValueFragment, inputs map[string]ProbeResult) (string, error) {
	renderer := probeDependencyFragmentRenderer{inputs: inputs}
	if err := renderer.validate("derived text outcome", fragments, 0); err != nil {
		return "", err
	}
	value, err := renderer.render("derived text outcome", fragments, 0)
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("derived text outcome contains NUL")
	}
	return value, nil
}

// IsProbeResultPredicate reports whether predicate depends only on declared
// ProbeResult inputs. These predicates need no process, scratch directory, or
// filesystem view and can therefore be evaluated identically by proberun and
// replay-time closure of a newly exposed zero-step node.
func IsProbeResultPredicate(predicate ProbePredicate) bool {
	switch predicate.Operator {
	case "all", "any":
		for _, operand := range predicate.Operands {
			if !IsProbeResultPredicate(operand) {
				return false
			}
		}
		return true
	case "not":
		return len(predicate.Operands) == 1 && IsProbeResultPredicate(predicate.Operands[0])
	case "result-true", "result-false", "result-text-empty", "result-text-equals", "result-text-contains":
		return true
	default:
		return false
	}
}

// EvaluateProbeResultPredicate evaluates the result-only predicate subset with
// the same short-circuit behavior as proberun. Callers must first establish
// IsProbeResultPredicate; unsupported operators fail closed.
func EvaluateProbeResultPredicate(predicate ProbePredicate, inputs map[string]ProbeResult) (bool, error) {
	return evaluateProbeResultPredicate(predicate, inputs, 0)
}

func evaluateProbeResultPredicate(predicate ProbePredicate, inputs map[string]ProbeResult, depth int) (bool, error) {
	if depth > MaxProbeValueFragmentDepth {
		return false, fmt.Errorf("dependency predicate exceeds depth %d", MaxProbeValueFragmentDepth)
	}
	switch predicate.Operator {
	case "all", "any":
		if len(predicate.Operands) == 0 {
			return false, fmt.Errorf("predicate %q has no operands", predicate.Operator)
		}
		want := predicate.Operator == "all"
		for _, operand := range predicate.Operands {
			value, err := evaluateProbeResultPredicate(operand, inputs, depth+1)
			if err != nil {
				return false, err
			}
			if value != want {
				return !want, nil
			}
		}
		return want, nil
	case "not":
		if len(predicate.Operands) != 1 {
			return false, fmt.Errorf("predicate not has %d operands, want one", len(predicate.Operands))
		}
		value, err := evaluateProbeResultPredicate(predicate.Operands[0], inputs, depth+1)
		return !value, err
	case "result-true", "result-false":
		result, ok := inputs[predicate.Result]
		if !ok || result.Kind != "boolean" || result.Boolean == nil {
			return false, fmt.Errorf("result input %s is not boolean", predicate.Result)
		}
		return *result.Boolean == (predicate.Operator == "result-true"), nil
	case "result-text-empty", "result-text-equals", "result-text-contains":
		result, ok := inputs[predicate.Result]
		if !ok || result.Kind != "text" {
			return false, fmt.Errorf("result input %s is not text", predicate.Result)
		}
		switch predicate.Operator {
		case "result-text-empty":
			return result.Text == "", nil
		case "result-text-equals":
			return result.Text == predicate.Value, nil
		default:
			return strings.Contains(result.Text, predicate.Value), nil
		}
	default:
		return false, fmt.Errorf("predicate %q is not a pure dependency reduction", predicate.Operator)
	}
}

// validate traverses the entire declared projection before selecting any
// conditional fragments. Thus an unreachable branch cannot hide an unbound or
// kind-invalid dependency, an excessive aggregate, or predicate recursion.
func (r *probeDependencyFragmentRenderer) validate(owner string, fragments []ProbeValueFragment, depth int) error {
	if depth > MaxProbeValueFragmentDepth {
		return fmt.Errorf("%s exceeds aggregate depth %d", owner, MaxProbeValueFragmentDepth)
	}
	for fragmentIndex, fragment := range fragments {
		r.fragments++
		if r.fragments > maxProbeValueFragments {
			return fmt.Errorf("%s has more than %d fragments", owner, maxProbeValueFragments)
		}
		location := fmt.Sprintf("%s fragment %d", owner, fragmentIndex)
		switch {
		case fragment.Value != "" && len(fragment.Fragments) == 0:
			if _, err := r.expand(fragment.Value); err != nil {
				return fmt.Errorf("validate %s: %w", location, err)
			}
		case fragment.Value == "" && len(fragment.Fragments) != 0:
			if err := r.validate(location+" aggregate", fragment.Fragments, depth+1); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s must contain exactly one nonempty value or aggregate", location)
		}
		if fragment.When != nil {
			if err := r.validatePredicate(*fragment.When, 0); err != nil {
				return fmt.Errorf("validate %s condition: %w", location, err)
			}
		}
		for transformIndex, transform := range fragment.Transforms {
			r.transforms++
			if r.transforms > maxProbeValueTransforms {
				return fmt.Errorf("derived text outcome has more than %d value transforms", maxProbeValueTransforms)
			}
			if err := validateProbeValueTransformShape(transform); err != nil {
				return fmt.Errorf("validate %s transform %d: %w", location, transformIndex, err)
			}
			for argumentIndex, argument := range transform.Arguments {
				if argumentIndex == transform.InputArgument {
					continue
				}
				if _, err := r.expand(argument); err != nil {
					return fmt.Errorf("validate %s transform %d argument %d: %w", location, transformIndex, argumentIndex, err)
				}
			}
			for groupIndex, group := range transform.ArgumentFragments {
				if err := r.validate(
					fmt.Sprintf("%s transform %d argument group %d", location, transformIndex, groupIndex),
					group.Fragments,
					depth+1,
				); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type probeDependencyFragmentRenderer struct {
	inputs     map[string]ProbeResult
	fragments  int
	transforms int
}

func (r *probeDependencyFragmentRenderer) render(owner string, fragments []ProbeValueFragment, depth int) (string, error) {
	if depth > MaxProbeValueFragmentDepth {
		return "", fmt.Errorf("%s exceeds aggregate depth %d", owner, MaxProbeValueFragmentDepth)
	}
	var rendered strings.Builder
	for fragmentIndex, fragment := range fragments {
		location := fmt.Sprintf("%s fragment %d", owner, fragmentIndex)
		if fragment.When != nil {
			include, err := r.predicate(*fragment.When, 0)
			if err != nil {
				return "", fmt.Errorf("evaluate %s condition: %w", location, err)
			}
			if !include {
				continue
			}
		}

		var expanded string
		var err error
		switch {
		case fragment.Value != "" && len(fragment.Fragments) == 0:
			expanded, err = r.expand(fragment.Value)
		case fragment.Value == "" && len(fragment.Fragments) != 0:
			expanded, err = r.render(location+" aggregate", fragment.Fragments, depth+1)
		default:
			return "", fmt.Errorf("%s must contain exactly one nonempty value or aggregate", location)
		}
		if err != nil {
			return "", fmt.Errorf("render %s: %w", location, err)
		}

		for transformIndex, transform := range fragment.Transforms {
			transform.Arguments = append([]string(nil), transform.Arguments...)
			dynamicGroupIndex := 0
			for argumentIndex, argument := range transform.Arguments {
				if argumentIndex == transform.InputArgument {
					continue
				}
				if dynamicGroupIndex < len(transform.ArgumentFragments) && transform.ArgumentFragments[dynamicGroupIndex].Index == argumentIndex {
					group := transform.ArgumentFragments[dynamicGroupIndex]
					transform.Arguments[argumentIndex], err = r.render(
						fmt.Sprintf("%s transform %d argument group %d", location, transformIndex, dynamicGroupIndex),
						group.Fragments,
						depth+1,
					)
					dynamicGroupIndex++
				} else {
					transform.Arguments[argumentIndex], err = r.expand(argument)
				}
				if err != nil {
					return "", fmt.Errorf("render %s transform %d argument %d: %w", location, transformIndex, argumentIndex, err)
				}
			}
			if dynamicGroupIndex != len(transform.ArgumentFragments) {
				return "", fmt.Errorf("%s transform %d did not consume every dynamic argument group", location, transformIndex)
			}
			transform.ArgumentFragments = nil
			expanded, err = ApplyProbeValueTransform(transform, expanded)
			if err != nil {
				return "", fmt.Errorf("apply %s transform %d: %w", location, transformIndex, err)
			}
		}
		if strings.ContainsRune(expanded, 0) {
			return "", fmt.Errorf("%s contains NUL", location)
		}
		if rendered.Len()+len(expanded) > MaxProbeInterpolatedBytes {
			return "", fmt.Errorf("%s exceeds %d rendered bytes", owner, MaxProbeInterpolatedBytes)
		}
		rendered.WriteString(expanded)
	}
	return rendered.String(), nil
}

func (r *probeDependencyFragmentRenderer) expand(value string) (string, error) {
	if strings.Contains(probePlaceholder.ReplaceAllString(value, ""), "${") {
		return "", fmt.Errorf("malformed or unsupported dependency placeholder in %q", value)
	}
	var expansionErr error
	expanded := probePlaceholder.ReplaceAllStringFunc(value, func(match string) string {
		parts := probePlaceholder.FindStringSubmatch(match)
		if parts[1] != "result" {
			expansionErr = fmt.Errorf("non-result placeholder %q", match)
			return match
		}
		reference := probeResultReference.FindStringSubmatch(parts[2])
		if reference == nil || reference[2] != "text" {
			expansionErr = fmt.Errorf("unsupported dependency result placeholder %q", match)
			return match
		}
		result, ok := r.inputs[reference[1]]
		if !ok {
			expansionErr = fmt.Errorf("unbound dependency result %q", reference[1])
			return match
		}
		if result.Kind != "text" || result.Boolean != nil {
			expansionErr = fmt.Errorf("dependency result %q is not text", reference[1])
			return match
		}
		return result.Text
	})
	if expansionErr != nil {
		return "", expansionErr
	}
	if strings.ContainsRune(expanded, 0) {
		return "", fmt.Errorf("expanded dependency fragment contains NUL")
	}
	if len(expanded) > MaxProbeInterpolatedBytes {
		return "", fmt.Errorf("expanded dependency fragment exceeds %d bytes; rendered bytes are bounded", MaxProbeInterpolatedBytes)
	}
	return expanded, nil
}

func (r *probeDependencyFragmentRenderer) validatePredicate(predicate ProbePredicate, depth int) error {
	if depth > MaxProbeValueFragmentDepth {
		return fmt.Errorf("dependency predicate exceeds depth %d", MaxProbeValueFragmentDepth)
	}
	switch predicate.Operator {
	case "all", "any":
		if len(predicate.Operands) == 0 {
			return fmt.Errorf("predicate %q has no operands", predicate.Operator)
		}
		for _, operand := range predicate.Operands {
			if err := r.validatePredicate(operand, depth+1); err != nil {
				return err
			}
		}
		return nil
	case "not":
		if len(predicate.Operands) != 1 {
			return fmt.Errorf("predicate not has %d operands, want one", len(predicate.Operands))
		}
		return r.validatePredicate(predicate.Operands[0], depth+1)
	case "result-true", "result-false":
		result, ok := r.inputs[predicate.Result]
		if !ok {
			return fmt.Errorf("unbound dependency result %q", predicate.Result)
		}
		if result.Kind != "boolean" || result.Boolean == nil || result.Text != "" {
			return fmt.Errorf("dependency result %q is not boolean", predicate.Result)
		}
		return nil
	default:
		return fmt.Errorf("unsupported dependency predicate %q", predicate.Operator)
	}
}

func (r *probeDependencyFragmentRenderer) predicate(predicate ProbePredicate, depth int) (bool, error) {
	if depth > MaxProbeValueFragmentDepth {
		return false, fmt.Errorf("dependency predicate exceeds depth %d", MaxProbeValueFragmentDepth)
	}
	switch predicate.Operator {
	case "all", "any":
		if len(predicate.Operands) == 0 {
			return false, fmt.Errorf("predicate %q has no operands", predicate.Operator)
		}
		want := predicate.Operator == "all"
		for _, operand := range predicate.Operands {
			value, err := r.predicate(operand, depth+1)
			if err != nil {
				return false, err
			}
			if value != want {
				return !want, nil
			}
		}
		return want, nil
	case "not":
		if len(predicate.Operands) != 1 {
			return false, fmt.Errorf("predicate not has %d operands, want one", len(predicate.Operands))
		}
		value, err := r.predicate(predicate.Operands[0], depth+1)
		return !value, err
	case "result-true", "result-false":
		result, ok := r.inputs[predicate.Result]
		if !ok {
			return false, fmt.Errorf("unbound dependency result %q", predicate.Result)
		}
		if result.Kind != "boolean" || result.Boolean == nil || result.Text != "" {
			return false, fmt.Errorf("dependency result %q is not boolean", predicate.Result)
		}
		return *result.Boolean == (predicate.Operator == "result-true"), nil
	default:
		return false, fmt.Errorf("unsupported dependency predicate %q", predicate.Operator)
	}
}
