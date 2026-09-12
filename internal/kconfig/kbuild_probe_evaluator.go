package kconfig

// This file adapts Kbuild's compiler helpers to the symbolic Kconfig probe
// evaluator. It contains no process or filesystem operations: discovery emits
// exact content-addressed requests, while replay rebuilds the same DAG and
// resolves its opaque results at Kbuild semantic boundaries.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// KbuildProbeWorkloadOptions binds both configured compiler scopes to one
// shared discovery DAG. Target is required; Host is optional unless the
// workload asks Scopes.Options for the host scope.
type KbuildProbeWorkloadOptions struct {
	Target KbuildProbeScopeOptions
	Host   *KbuildProbeScopeOptions
}

type KbuildProbeScopeOptions struct {
	Architecture       string
	SourceArchitecture string
	SourceRoot         string
	ScriptEnvironment  map[string]string
	Facts              *LinuxCompilerFacts
	Tools              map[string]string
	RustSourceRoot     string
}

// compilerPredefineProjectionUnsupportedError is returned before an initial
// compiler-state request is registered when its name vector, symbolic argv,
// or environment state cannot be represented by the probe protocol. The
// executable compiler action is still valid; config-dependency analysis must
// conservatively make it opaque instead of constructing a partial request.
type compilerPredefineProjectionUnsupportedError struct {
	cause error
}

func (e *compilerPredefineProjectionUnsupportedError) Error() string { return e.cause.Error() }
func (e *compilerPredefineProjectionUnsupportedError) Unwrap() error { return e.cause }

func unsupportedCompilerPredefineProjection(err error) error {
	if err == nil {
		return nil
	}
	return &compilerPredefineProjectionUnsupportedError{cause: err}
}

// KbuildProbeScopes supplies parser options whose callbacks are pinned to the
// requested toolchain scope. Callers may parse an arbitrary collection of
// source-derived Makefiles/profiles in one workload; every request enters the
// same ProbePlanBuilder and References returns their exact union.
type KbuildProbeScopes struct {
	evaluators             map[string]*LinuxProbeEvaluator
	baseScriptEnvironments map[string]map[string]string
	resolved               successfulStringMemo
	resolvedStructure      successfulStringMemo
	sourceGuardInventory   *configDependencyGuardInventory
	// exactScriptEnvironmentBindings intern complete source-owned process
	// environments. Profiles retain only the corresponding activation closure;
	// the binding and active evaluator set remain local to this workload.
	exactScriptEnvironmentBindings    map[string]map[string]map[string]string
	exactScriptEnvironmentActivations map[string]func() error
	activeExactScriptEnvironment      string
	activeScriptEnvironmentIdentity   string
}

const (
	// Compound Make comparisons are evaluated as a finite boolean language.
	// Keep the limits independent of Linux versions and compiler families: an
	// expression outside this deliberately small grammar fails closed instead
	// of making discovery exponential or guessing a branch.
	maxKbuildSymbolicComparisonDepth      = 32
	maxKbuildSymbolicComparisonSymbols    = 256
	maxKbuildSymbolicComparisonReferences = 8
	maxKbuildSymbolicComparisonBytes      = 64 << 10
)

type kbuildSymbolicComparisonBinding struct {
	evaluator *LinuxProbeEvaluator
	input     linuxProbeSelectionInput
}

type kbuildOwnedSymbol struct {
	evaluator *LinuxProbeEvaluator
	symbol    linuxProbeSymbol
}

// kbuildSymbolicEmptiness is a deliberately small proof lattice. Nonempty
// means that at least one non-space byte survives for every replay result;
// empty means the exact expression is the empty string for every result.
// Whitespace-only and all other shapes stay unknown unless a supported exact
// Make AST removes that ambiguity.
type kbuildSymbolicEmptiness uint8

const (
	kbuildSymbolicEmptinessUnknown kbuildSymbolicEmptiness = iota
	kbuildSymbolicEmptinessEmpty
	kbuildSymbolicEmptinessNonempty
)

// SelectSymbolic exposes the exact one-toolset Make selection contract for
// source phases which run before the shared target/host scope router exists.
// Every referenced atom must belong to this evaluator; cross-scope values are
// therefore rejected by the same implementation used during final planning.
func (e *LinuxProbeEvaluator) SelectSymbolic(value, expected string, equal bool, trueText, falseText string) (string, bool, error) {
	if e == nil {
		return "", true, fmt.Errorf("Linux probe evaluator is nil")
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{e.scope: e}}
	return scopes.selectSymbolic(value, expected, equal, trueText, falseText)
}

// TransformSymbolic exposes pure Make transformations during the same early
// one-toolset source phases. It shares the finite-selection and exact opaque
// Make-AST implementation used by KbuildProbeScopes.Options.
func (e *LinuxProbeEvaluator) TransformSymbolic(function string, args []string) (string, bool, error) {
	if e == nil {
		return "", true, fmt.Errorf("Linux probe evaluator is nil")
	}
	scopes := &KbuildProbeScopes{evaluators: map[string]*LinuxProbeEvaluator{e.scope: e}}
	return scopes.transformSymbolic(function, args)
}

// Options adds symbolic compiler callbacks to base. The final planning phase
// therefore cannot execute or stat a compiler. Linux 6.12 and 6.18 do not
// define hostcc-option/hostld-option:
// scripts/Makefile.compiler's source-defined try-run is tool agnostic. The
// shell router therefore chooses target versus host from the configured tool
// present in the expanded command, never from a helper name. scope is only the
// preferred evaluator for commands without a distinguishing tool token.
func (s *KbuildProbeScopes) Options(scope string, base KbuildOptions) (KbuildOptions, error) {
	if s == nil || s.evaluators[scope] == nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload has no %s scope", scope)
	}
	if base.ResolveSymbolic != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a symbolic resolver", scope)
	}
	if base.ResolveSymbolicWords != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a symbolic word resolver", scope)
	}
	if base.ResolveSymbolicStructure != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a symbolic structure resolver", scope)
	}
	if base.SelectSymbolic != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a symbolic selector", scope)
	}
	if base.TransformSymbolic != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a symbolic transformer", scope)
	}
	if base.SourceShell != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a source-shell evaluator", scope)
	}
	if base.shellResultAvailable != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a shell-result observer", scope)
	}
	if base.probeEnvironmentIdentity != nil {
		return KbuildOptions{}, fmt.Errorf("Kbuild probe workload %s options already contain a probe-environment identity", scope)
	}
	fallbackShell := base.Shell
	base.ResolveSymbolic = func(value string) (string, error) {
		return s.resolveSymbolicForScope(scope, value)
	}
	base.ResolveSymbolicWords = func(value string) (string, error) {
		return s.resolveSymbolicWordsForScope(scope, value)
	}
	base.ResolveSymbolicStructure = func(value string) (string, error) {
		return s.resolveSymbolicStructureForScope(scope, value)
	}
	base.SelectSymbolic = func(value, expected string, equal bool, trueText, falseText string) (string, bool, error) {
		if err := s.validateSymbolicConsumer(scope, value, expected, trueText, falseText); err != nil {
			return "", true, err
		}
		return s.selectSymbolic(value, expected, equal, trueText, falseText)
	}
	base.TransformSymbolic = func(function string, args []string) (string, bool, error) {
		if err := s.validateSymbolicConsumer(scope, args...); err != nil {
			return "", true, err
		}
		return s.transformSymbolic(function, args)
	}
	base.Shell = func(command string) (string, error) {
		return s.kbuildShell(context.Background(), scope, command, fallbackShell)
	}
	base.shellResultAvailable = func(command string) bool {
		return s.kbuildShellResultAvailable(scope, command)
	}
	base.probeEnvironmentIdentity = func() string {
		return s.activeScriptEnvironmentIdentity
	}
	base.SourceShell = func(command, workingDirectory string) (string, error) {
		return s.kbuildSourceShell(context.Background(), command, workingDirectory)
	}
	return base, nil
}

func (s *KbuildProbeScopes) validateSymbolicConsumer(scope string, values ...string) error {
	for _, value := range values {
		if linuxProbeSymbolPattern.MatchString(value) {
			_, err := s.compatibleSymbolicEvaluatorForConsumer(scope, values...)
			return err
		}
	}
	return nil
}

func (s *KbuildProbeScopes) transformSymbolic(function string, args []string) (string, bool, error) {
	if (function == "wildcard" && len(args) == 1) || (function == "foreach" && len(args) == 3) {
		// wildcard is deliberately not a runtime value transform: its result
		// depends on the Kbuild parser's source-root and predecessor-artifact
		// view. foreach can consume that contextual word list while forming an
		// include name. Preserve either exact expression for discovery. The
		// replay parser resolves the measured list first and invokes the ordinary
		// evaluator in context, so this opaque value must never be lowered into a
		// process argument.
		opaque, err := s.renderMakeText(function, args, "", linuxProbeMakeTextProtocolUnusable)
		return opaque, true, err
	}
	contextual, contextualErr := s.containsContextualMakeText(args...)
	if contextualErr != nil {
		return "", true, contextualErr
	}
	if contextual {
		if !pureKbuildMakeFunctionArity(function, len(args)) {
			return "", true, fmt.Errorf(
				"Make function %q cannot compose over a parser-context replay value",
				function,
			)
		}
		// Replay reparses the source expression after wildcard/foreach has a
		// concrete value, so keep every intervening pure Make operation as an
		// exact but deliberately non-lowerable AST. This avoids pretending a
		// filesystem observation is a context-free probe-value transform.
		opaque, err := s.renderMakeText(function, args, "", linuxProbeMakeTextProtocolUnusable)
		return opaque, true, err
	}
	if function == "findstring" {
		if transformed, recognized, err := s.transformDynamicSymbolicFindstring(args); recognized || err != nil {
			return transformed, recognized, err
		}
	}
	if transformed, recognized, err := s.transformChainedMakeTextProtocol(function, args); recognized || err != nil {
		return transformed, recognized, err
	}
	if function == "filter" || function == "filter-out" {
		// Filtering an empty word list is empty regardless of dynamic
		// patterns. Validate every atom, then discharge the result without
		// inventing a dependency on the pattern value.
		if len(args) == 2 && !linuxProbeSymbolPattern.MatchString(args[1]) && len(strings.Fields(args[1])) == 0 {
			if _, _, _, err := s.collectSymbolicComparison(args[0]); err != nil {
				return "", true, err
			}
			return "", true, nil
		}
		if function == "filter-out" && len(args) == 2 &&
			!linuxProbeSymbolPattern.MatchString(args[0]) && len(strings.Fields(args[0])) == 0 &&
			linuxProbeSymbolPattern.MatchString(args[1]) {
			inputMode, err := s.makeTextInputProtocolMode(args[1])
			if err != nil {
				return "", true, err
			}
			protocolMode := linuxProbeMakeTextProtocolCanonicalWords
			if inputMode == linuxProbeMakeTextProtocolUnusable {
				protocolMode = linuxProbeMakeTextProtocolUnusable
			}
			opaque, err := s.renderMakeText(function, args, args[1], protocolMode)
			return opaque, true, err
		}
		if transformed, recognized, err := s.transformFiniteSymbolicMakeFunction(function, args); recognized || err != nil {
			return transformed, recognized, err
		}
	}
	if function == "strip" {
		if transformed, recognized, err := s.transformCompositionalSymbolicStrip(args); recognized || err != nil {
			return transformed, recognized, err
		}
	}
	if function == "subst" {
		if transformed, recognized, err := s.transformCompositionalSymbolicSubst(args); recognized || err != nil {
			return transformed, recognized, err
		}
		bindings, _, hasText, collectErr := s.collectSymbolicComparison(args...)
		if collectErr != nil {
			return "", true, collectErr
		}
		if len(args) == 3 && (hasText || len(bindings) > maxKbuildSymbolicComparisonReferences) &&
			!s.isSingleCompleteSymbolicTextArgument(args) {
			// A multi-byte match can cross arbitrary symbolic fragment
			// boundaries. Retain the exact operation as an opaque replay AST
			// instead of enumerating every finite compiler-capability state.
			// There is no generally safe process lowering for this shape;
			// it remains usable at Make replay boundaries and fails closed if a
			// later action tries to consume it directly.
			opaque, err := s.renderMakeText("subst", args, args[2], linuxProbeMakeTextProtocolUnusable)
			return opaque, true, err
		}
		// A multi-byte search can match across a literal/symbol boundary, so it
		// is not separable. Finite inputs still have an exact bounded whole-
		// expression representation; unbounded text falls through to the
		// existing one-complete-input transform below.
		_, _, hasText, err := s.collectSymbolicComparison(args...)
		if err != nil {
			return "", true, err
		}
		if !hasText {
			return s.transformFiniteSymbolicMakeFunction(function, args)
		}
	}
	tokens := []struct {
		value string
		arg   int
	}{}
	for index, arg := range args {
		for token := range linuxProbeSymbolPattern.AllString(arg) {
			tokens = append(tokens, struct {
				value string
				arg   int
			}{value: token, arg: index})
		}
	}
	if len(tokens) == 0 {
		return "", false, nil
	}
	if transformed, recognized, err := s.transformPathComponentWordListFunction(function, args); recognized || err != nil {
		return transformed, recognized, err
	}
	if transformed, recognized, err := s.transformSingleMakeWordListFunction(function, args); recognized || err != nil {
		return transformed, recognized, err
	}
	if len(tokens) != 1 || args[tokens[0].arg] != tokens[0].value {
		// Embedded and multiple unbounded text atoms cannot be represented by a
		// finite truth table or a single source-text transform. Preserve the
		// complete pure Make expression as an exact replay AST. No process
		// protocol is inferred for this general shape.
		opaque, err := s.renderMakeText(function, args, "", linuxProbeMakeTextProtocolUnusable)
		return opaque, true, err
	}
	evaluator, _, ok := s.symbolOwner(tokens[0].value)
	if !ok {
		return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", tokens[0].value)
	}
	_, symbol, _ := s.symbolOwner(tokens[0].value)
	if symbol.kind == "boolean" || symbol.kind == "selection" {
		if transformed, recognized, err := s.transformFiniteSymbolicMakeFunction(function, args); recognized || err != nil {
			return transformed, recognized, err
		}
		// A finite outer choice may contain an unbounded text expression in
		// one of its branches. Preserve the full operation for exact replay
		// when the finite evaluator correctly declines that mixed graph.
		opaque, err := s.renderMakeText(function, args, "", linuxProbeMakeTextProtocolUnusable)
		return opaque, true, err
	}
	if symbol.kind == "make-text" {
		if symbol.makeText == nil {
			return "", true, fmt.Errorf("Linux whole Make text symbolic value %q has no expression", tokens[0].value)
		}
		protocolValue := ""
		protocolMode := linuxProbeMakeTextProtocolUnusable
		if len(symbol.makeText.protocolTransforms) == 0 {
			if outputMode, wordProtocol := makeTextWordProtocolOutputMode(function); symbol.makeText.protocolMode != linuxProbeMakeTextProtocolUnusable && wordProtocol {
				protocolArgs := slices.Clone(args)
				protocolArgs[tokens[0].arg] = symbol.makeText.protocolValue
				bindings, _, hasText, collectErr := s.collectSymbolicComparison(protocolArgs...)
				if collectErr == nil && !hasText && len(bindings) <= maxKbuildSymbolicComparisonReferences {
					lowered, recognized, lowerErr := "", false, error(nil)
					if len(bindings) == 0 {
						lowered, recognized, lowerErr = evalPureKbuildMakeFunction(function, protocolArgs, "")
					} else {
						lowered, recognized, lowerErr = s.transformFiniteSymbolicMakeFunction(function, protocolArgs)
					}
					if lowerErr == nil && recognized {
						protocolValue = lowered
						protocolMode = outputMode
					}
				}
			}
		}
		opaque, err := s.renderMakeText(function, args, protocolValue, protocolMode)
		return opaque, true, err
	}
	transformed, err := evaluator.renderTextTransform(tokens[0].value, function, args, tokens[0].arg)
	return transformed, true, err
}

func contextualKbuildReplayFunction(function string, argumentCount int) bool {
	switch function {
	case "wildcard":
		return argumentCount == 1
	case "foreach":
		return argumentCount == 3
	case "context-select":
		return argumentCount == 5
	default:
		return false
	}
}

// containsContextualMakeText distinguishes an exact parser replay expression
// from ordinary compiler-result text. Contextual expressions may flow through
// pure Make operations during discovery, but must never be materialized in a
// probe action: wildcard observes the invocation's source/artifact snapshot,
// and foreach observes the parser-local variable binding created from it.
func (s *KbuildProbeScopes) containsContextualMakeText(values ...string) (bool, error) {
	visiting := map[string]bool{}
	visited := map[string]bool{}
	work := 0
	var visitValue func(string, int) (bool, error)
	visitValue = func(value string, depth int) (bool, error) {
		if depth > maxKbuildSymbolicComparisonDepth {
			return false, fmt.Errorf("parser-context Make replay expression is too deep")
		}
		for token := range linuxProbeSymbolPattern.AllString(value) {
			if visited[token] {
				continue
			}
			if visiting[token] {
				return false, fmt.Errorf("cyclic Linux probe symbolic value %q in parser-context replay", token)
			}
			work++
			if work > maxProbeValueFragments {
				return false, fmt.Errorf("parser-context Make replay exceeds %d work items", maxProbeValueFragments)
			}
			_, symbol, ok := s.symbolOwner(token)
			if !ok {
				return false, fmt.Errorf("unknown Linux probe symbolic value %q in parser-context replay", token)
			}
			visiting[token] = true
			if symbol.kind == "make-text" {
				if symbol.makeText == nil {
					return false, fmt.Errorf("Linux whole Make text symbolic value %q has no expression", token)
				}
				if contextualKbuildReplayFunction(symbol.makeText.function, len(symbol.makeText.arguments)) {
					delete(visiting, token)
					visited[token] = true
					return true, nil
				}
				for _, nested := range append(slices.Clone(symbol.makeText.arguments), symbol.makeText.protocolValue) {
					contextual, err := visitValue(nested, depth+1)
					if err != nil || contextual {
						delete(visiting, token)
						return contextual, err
					}
				}
				for _, transform := range symbol.makeText.protocolTransforms {
					for _, nested := range transform.arguments {
						contextual, err := visitValue(nested, depth+1)
						if err != nil || contextual {
							delete(visiting, token)
							return contextual, err
						}
					}
				}
			} else if symbol.kind == "boolean" {
				for _, nested := range []string{symbol.falseText, symbol.trueText} {
					contextual, err := visitValue(nested, depth+1)
					if err != nil || contextual {
						delete(visiting, token)
						return contextual, err
					}
				}
			} else if symbol.kind == "selection" {
				for _, nested := range symbol.selectionValues {
					contextual, err := visitValue(nested, depth+1)
					if err != nil || contextual {
						delete(visiting, token)
						return contextual, err
					}
				}
			} else if symbol.kind == "transformed-text" && symbol.textTransform != nil {
				contextual, err := visitValue(symbol.textTransform.sourceToken, depth+1)
				if err != nil || contextual {
					delete(visiting, token)
					return contextual, err
				}
			}
			delete(visiting, token)
			visited[token] = true
		}
		return false, nil
	}
	for _, value := range values {
		contextual, err := visitValue(value, 0)
		if err != nil || contextual {
			return contextual, err
		}
	}
	return false, nil
}

// transformPathComponentWordListFunction evaluates operations whose result is
// invariant under replacing a probe token with its promised path component.
// The planner token is itself one slash-free component, so dir and notdir see
// exactly the same component boundaries as replay. dir may erase the dynamic
// name entirely; notdir retains it without changing its bytes. A mere
// SingleMakeWord contract is insufficient because that word may contain '/'.
func (s *KbuildProbeScopes) transformPathComponentWordListFunction(function string, args []string) (string, bool, error) {
	if (function != "dir" && function != "notdir") || len(args) != 1 {
		return "", false, nil
	}
	tokens := linuxProbeSymbolPattern.FindAllString(args[0], -1)
	if len(tokens) == 0 {
		return "", false, nil
	}
	for _, token := range tokens {
		_, symbol, ok := s.symbolOwner(token)
		if !ok {
			return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", token)
		}
		if symbol.kind != "text" || symbol.reference.Kind != "text" || !symbol.request.Outcome.PathComponent {
			return "", false, nil
		}
	}
	transformed, recognized, err := evalPureKbuildMakeFunction(function, args, "")
	if err != nil {
		return "", true, err
	}
	if !recognized {
		return "", true, fmt.Errorf("Make function %q has no hermetic path-component transform", function)
	}
	return transformed, true, nil
}

// transformSingleMakeWordListFunction compositionally evaluates the small
// pure-Make subset whose behavior depends only on the word boundaries of its
// value argument. Each dynamic leaf carries a runtime-enforced one-word
// contract, so replacing planner tokens with measured values cannot create or
// remove a boundary. The returned text deliberately retains the leaf tokens;
// replay reparses the same Makefile with concrete values and recomputes it.
func (s *KbuildProbeScopes) transformSingleMakeWordListFunction(function string, args []string) (string, bool, error) {
	if (function != "addprefix" && function != "addsuffix") || len(args) != 2 ||
		linuxProbeSymbolPattern.MatchString(args[0]) {
		return "", false, nil
	}
	tokens := linuxProbeSymbolPattern.FindAllString(args[1], -1)
	if len(tokens) == 0 {
		return "", false, nil
	}
	for _, token := range tokens {
		_, symbol, ok := s.symbolOwner(token)
		if !ok {
			return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", token)
		}
		if symbol.kind != "text" || symbol.reference.Kind != "text" ||
			(!symbol.request.Outcome.SingleMakeWord && !symbol.request.Outcome.PathComponent) {
			return "", false, nil
		}
	}
	transformed, recognized, err := evalPureKbuildMakeFunction(function, args, "")
	if err != nil {
		return "", true, err
	}
	if !recognized {
		return "", true, fmt.Errorf("Make function %q has no hermetic symbolic text transform", function)
	}
	return transformed, true, nil
}

func makeTextWordProtocolOutputMode(function string) (linuxProbeMakeTextProtocolMode, bool) {
	switch function {
	case "dir", "filter", "filter-out", "firstword", "lastword", "sort", "strip", "suffix", "word", "words":
		return linuxProbeMakeTextProtocolCanonicalWords, true
	case "addprefix", "addsuffix", "basename", "notdir", "patsubst", "wordlist":
		return linuxProbeMakeTextProtocolArgvWords, true
	default:
		return linuxProbeMakeTextProtocolUnusable, false
	}
}

func (s *KbuildProbeScopes) isSingleCompleteSymbolicTextArgument(args []string) bool {
	matchCount := 0
	completeToken := ""
	for _, argument := range args {
		matches := linuxProbeSymbolPattern.FindAllString(argument, -1)
		matchCount += len(matches)
		if len(matches) == 1 && matches[0] == argument {
			completeToken = matches[0]
		}
	}
	if matchCount != 1 || completeToken == "" {
		return false
	}
	_, symbol, ok := s.symbolOwner(completeToken)
	return ok && (symbol.kind == "text" || symbol.kind == "transformed-text" || symbol.kind == "toolset-path-literal")
}

func (s *KbuildProbeScopes) renderMakeText(function string, arguments []string, protocolValue string, protocolMode linuxProbeMakeTextProtocolMode) (string, error) {
	values := append(slices.Clone(arguments), protocolValue)
	evaluator, err := s.compatibleSymbolicEvaluator(values...)
	if err != nil {
		return "", fmt.Errorf("whole Make text function %q: %w", function, err)
	}
	return evaluator.renderMakeText(function, arguments, protocolValue, protocolMode)
}

func (s *KbuildProbeScopes) renderMakeTextWithProtocolTransforms(
	function string,
	arguments []string,
	protocolValue string,
	protocolTransforms []linuxProbeMakeTextProtocolTransform,
	protocolMode linuxProbeMakeTextProtocolMode,
) (string, error) {
	values := append(slices.Clone(arguments), protocolValue)
	for _, transform := range protocolTransforms {
		values = append(values, transform.arguments...)
	}
	evaluator, err := s.compatibleSymbolicEvaluator(values...)
	if err != nil {
		return "", fmt.Errorf("whole Make text function %q: %w", function, err)
	}
	return evaluator.renderMakeTextWithProtocolTransforms(function, arguments, protocolValue, protocolTransforms, protocolMode)
}

// transformChainedMakeTextProtocol composes another word-normalizing
// operation onto an already exact runtime protocol. Keeping the source and
// its prior transforms opaque avoids reintroducing a finite-state product.
func (s *KbuildProbeScopes) transformChainedMakeTextProtocol(function string, args []string) (string, bool, error) {
	inputArgument := -1
	switch function {
	case "addprefix", "addsuffix", "filter", "filter-out":
		if len(args) == 2 {
			inputArgument = 1
		}
	case "patsubst", "subst":
		if len(args) == 3 {
			inputArgument = 2
		}
	case "dir", "sort", "strip":
		if len(args) == 1 {
			inputArgument = 0
		}
	}
	if inputArgument < 0 {
		return "", false, nil
	}
	matches := linuxProbeSymbolPattern.FindAllString(args[inputArgument], -1)
	if len(matches) != 1 {
		return "", false, nil
	}
	completeInput := matches[0] == args[inputArgument]
	wordInput := function != "subst" && strings.Trim(args[inputArgument], " \t\r\n\v\f") == matches[0]
	if !completeInput && !wordInput {
		return "", false, nil
	}
	_, symbol, ok := s.symbolOwner(matches[0])
	if !ok || symbol.kind != "make-text" || symbol.makeText == nil {
		return "", false, nil
	}
	// A zero-transform protocol normally keeps the compact direct lowering
	// chosen by the originating Make function. strip is the exception needed
	// by recursive Make: it turns that usable word protocol into exact text
	// without changing the child analysis expression.
	if len(symbol.makeText.protocolTransforms) == 0 && function != "strip" {
		return "", false, nil
	}
	if symbol.makeText.protocolMode == linuxProbeMakeTextProtocolUnusable {
		return "", false, nil
	}
	// Most supported runtime transforms consume their input as a GNU Make word
	// list. subst is bytewise and requires an exact input.
	if function == "subst" && symbol.makeText.protocolMode != linuxProbeMakeTextProtocolExact {
		return "", false, nil
	}
	if function == "filter" || function == "filter-out" {
		mode, err := s.makeTextInputProtocolMode(args[0])
		if err != nil {
			return "", true, err
		}
		if mode == linuxProbeMakeTextProtocolUnusable {
			return "", false, nil
		}
	} else {
		// The current generic protocol permits dynamic filter patterns because
		// filters observe a list of Make words. Other transform parameters are
		// byte-sensitive affixes or patterns and remain source-owned literals.
		for index, argument := range args {
			if index != inputArgument && linuxProbeSymbolPattern.MatchString(argument) {
				return "", false, nil
			}
		}
	}
	transformArgs := slices.Clone(args)
	transformArgs[inputArgument] = ""
	transforms := cloneLinuxProbeMakeTextProtocolTransforms(symbol.makeText.protocolTransforms)
	transforms = append(transforms, linuxProbeMakeTextProtocolTransform{
		function: function, arguments: transformArgs, inputArgument: inputArgument,
	})
	value, err := s.renderMakeTextWithProtocolTransforms(
		function, args, symbol.makeText.protocolValue, transforms, linuxProbeMakeTextProtocolExact,
	)
	return value, true, err
}

// transformDynamicSymbolicFindstring retains both complete expanded operands
// and applies GNU Make's bytewise search in the configured probe action. This
// deliberately does not use the word-oriented chained-transform shortcut:
// leading and trailing function-argument bytes are observable to findstring,
// and a canonical-word child must be normalized before it becomes exact text.
// Lowering the complete fragment graphs handles both properties while also
// supporting the real Kbuild shape where _c_flags combines filtered compiler
// selections with source-owned literal flags.
func (s *KbuildProbeScopes) transformDynamicSymbolicFindstring(args []string) (string, bool, error) {
	if len(args) != 2 ||
		(!linuxProbeSymbolPattern.MatchString(args[0]) && !linuxProbeSymbolPattern.MatchString(args[1])) {
		return "", false, nil
	}
	for _, argument := range args {
		mode, err := s.makeTextInputProtocolMode(argument)
		if err != nil {
			return "", true, err
		}
		if mode == linuxProbeMakeTextProtocolArgvWords || mode == linuxProbeMakeTextProtocolUnusable {
			// Preserve the exact Make expression for replay-only consumers, but do
			// not claim that it has a configured-action protocol.
			opaque, renderErr := s.renderMakeText("findstring", args, "", linuxProbeMakeTextProtocolUnusable)
			return opaque, true, renderErr
		}
	}
	transformArgs := slices.Clone(args)
	transformArgs[1] = ""
	returnValue, err := s.renderMakeTextWithProtocolTransforms(
		"findstring",
		args,
		args[1],
		[]linuxProbeMakeTextProtocolTransform{{
			function: "findstring", arguments: transformArgs, inputArgument: 1,
		}},
		linuxProbeMakeTextProtocolExact,
	)
	return returnValue, true, err
}

func (s *KbuildProbeScopes) compatibleSymbolicEvaluator(values ...string) (*LinuxProbeEvaluator, error) {
	return s.compatibleSymbolicEvaluatorInScopes([]string{"host", "target"}, values...)
}

func (s *KbuildProbeScopes) compatibleSymbolicEvaluatorForConsumer(scope string, values ...string) (*LinuxProbeEvaluator, error) {
	switch scope {
	case "host":
		return s.compatibleSymbolicEvaluatorInScopes([]string{"host"}, values...)
	case "target":
		return s.compatibleSymbolicEvaluatorInScopes([]string{"host", "target"}, values...)
	default:
		return nil, fmt.Errorf("unknown Kbuild probe consumer scope %q", scope)
	}
}

func (s *KbuildProbeScopes) compatibleSymbolicEvaluatorInScopes(scopes []string, values ...string) (*LinuxProbeEvaluator, error) {
	tokens := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		for token := range linuxProbeSymbolPattern.AllString(value) {
			if !seen[token] {
				seen[token] = true
				tokens = append(tokens, token)
			}
		}
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("value has no symbolic input")
	}
	var compatibilityErrors []string
	// Prefer host when the complete leaf graph is host-compatible. This keeps a
	// target-first host getconf wrapper adoptable by the later host action even
	// though the target evaluator happened to intern the original text first.
	for _, scope := range scopes {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		compatible := true
		for _, token := range tokens {
			if _, exists, err := evaluator.adoptSymbol(token); err != nil {
				compatibilityErrors = append(compatibilityErrors, scope+": "+err.Error())
				compatible = false
				break
			} else if !exists {
				compatibilityErrors = append(compatibilityErrors, scope+": unknown nested value "+token)
				compatible = false
				break
			}
		}
		if compatible {
			return evaluator, nil
		}
	}
	return nil, fmt.Errorf("value has no compatible evaluator: %s", strings.Join(compatibilityErrors, "; "))
}

// transformCompositionalSymbolicSubst retains GNU Make's bytewise subst over
// arbitrary interleavings of literals and independent probe atoms. A
// one-byte search cannot straddle a fragment boundary, so transforming every
// literal and every finite/text atom independently is exactly equivalent to
// applying subst after replay has concatenated the complete string. This is
// the shape used by Kbuild's make-cmd dollar escaping and stays linear even
// when a command carries many independently probed compiler flags.
//
// Multi-byte searches deliberately return recognized=false: an occurrence may
// begin in one fragment and end in the next. The caller uses a bounded finite
// truth table when possible and otherwise retains the complete exact Make AST.
func (s *KbuildProbeScopes) transformCompositionalSymbolicSubst(args []string) (string, bool, error) {
	if len(args) != 3 || len(args[0]) != 1 ||
		linuxProbeSymbolPattern.MatchString(args[0]) || linuxProbeSymbolPattern.MatchString(args[1]) ||
		!linuxProbeSymbolPattern.MatchString(args[2]) {
		return "", false, nil
	}
	state := &kbuildSymbolicSubstState{
		visiting: map[string]bool{},
		memo:     map[string]kbuildSymbolicSubstMemo{},
	}
	transformed, protocolMode, err := s.transformCompositionalSubstText(args[2], args[0], args[1], state, 0)
	if err != nil {
		return "", true, err
	}
	if protocolMode == linuxProbeMakeTextProtocolExact {
		return transformed, true, nil
	}
	opaque, err := s.renderMakeText("subst", args, transformed, protocolMode)
	return opaque, true, err
}

type kbuildSymbolicSubstMemo struct {
	value        string
	protocolMode linuxProbeMakeTextProtocolMode
}

type kbuildSymbolicSubstState struct {
	visiting map[string]bool
	memo     map[string]kbuildSymbolicSubstMemo
	work     int
}

func (s *kbuildSymbolicSubstState) reserve(count int) error {
	if count < 0 || count > maxProbeValueFragments-s.work {
		return fmt.Errorf("symbolic Make subst expansion exceeds %d work items", maxProbeValueFragments)
	}
	s.work += count
	return nil
}

func (s *KbuildProbeScopes) transformCompositionalSubstText(
	value, old, replacement string,
	state *kbuildSymbolicSubstState,
	depth int,
) (string, linuxProbeMakeTextProtocolMode, error) {
	if depth > maxKbuildSymbolicComparisonDepth {
		return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("symbolic Make subst expansion is too deep")
	}
	if len(value) > maxKbuildSymbolicComparisonBytes {
		return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("symbolic Make subst input exceeds %d bytes", maxKbuildSymbolicComparisonBytes)
	}

	var transformed strings.Builder
	protocolMode := linuxProbeMakeTextProtocolExact
	last := 0
	for start, end := range linuxProbeSymbolPattern.AllStringIndex(value) {
		if err := appendBoundedSymbolicSubstLiteral(&transformed, value[last:start], old, replacement); err != nil {
			return "", linuxProbeMakeTextProtocolUnusable, err
		}
		token := value[start:end]
		if memo, ok := state.memo[token]; ok {
			transformed.WriteString(memo.value)
			if transformed.Len() > maxKbuildSymbolicComparisonBytes {
				return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("symbolic Make subst result exceeds %d bytes", maxKbuildSymbolicComparisonBytes)
			}
			protocolMode = combineLinuxProbeMakeTextProtocolMode(protocolMode, memo.protocolMode)
			last = end
			continue
		}
		if state.visiting[token] {
			return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("cyclic Linux probe symbolic value %q in Make subst", token)
		}
		if err := state.reserve(1); err != nil {
			return "", linuxProbeMakeTextProtocolUnusable, err
		}
		evaluator, symbol, ok := s.symbolOwner(token)
		if !ok {
			return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("unknown Linux probe symbolic value %q", token)
		}

		var tokenValue string
		tokenMode := linuxProbeMakeTextProtocolExact
		switch symbol.kind {
		case "text", "transformed-text", "toolset-path-literal":
			var err error
			tokenValue, err = evaluator.renderTextTransform(token, "subst", []string{old, replacement, token}, 2)
			if err != nil {
				return "", linuxProbeMakeTextProtocolUnusable, err
			}
		case "make-text":
			if symbol.makeText == nil {
				return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("Linux whole Make text symbolic value %q has no expression", token)
			}
			if len(symbol.makeText.protocolTransforms) != 0 {
				if symbol.makeText.protocolMode != linuxProbeMakeTextProtocolExact {
					return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("Make subst requires an exact dynamic whole-value protocol")
				}
				transformArgs := []string{old, replacement, ""}
				wireShape := ProbeValueTransform{Function: "subst", Arguments: slices.Clone(transformArgs), InputArgument: 2}
				if err := validateProbeValueTransformShape(wireShape); err != nil {
					return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("Make subst dynamic whole-value protocol: %w", err)
				}
				transforms := cloneLinuxProbeMakeTextProtocolTransforms(symbol.makeText.protocolTransforms)
				transforms = append(transforms, linuxProbeMakeTextProtocolTransform{
					function: "subst", arguments: transformArgs, inputArgument: 2,
				})
				var err error
				tokenValue, err = s.renderMakeTextWithProtocolTransforms(
					"subst", []string{old, replacement, token}, symbol.makeText.protocolValue,
					transforms, linuxProbeMakeTextProtocolExact,
				)
				if err != nil {
					return "", linuxProbeMakeTextProtocolUnusable, err
				}
				break
			}
			state.visiting[token] = true
			protocolValue, nestedMode, err := s.transformCompositionalSubstText(
				symbol.makeText.protocolValue, old, replacement, state, depth+1,
			)
			delete(state.visiting, token)
			if err != nil {
				return "", linuxProbeMakeTextProtocolUnusable, err
			}
			tokenValue = protocolValue
			tokenMode = combineLinuxProbeMakeTextProtocolMode(
				linuxProbeMakeTextSubstProtocolMode(symbol.makeText.protocolMode, old, replacement),
				nestedMode,
			)
		case "boolean", "selection":
			var inputs []linuxProbeSelectionInput
			var values []string
			if symbol.kind == "boolean" {
				if symbol.reference.Kind != "boolean" {
					return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("Linux boolean probe symbolic value %q has %q reference", token, symbol.reference.Kind)
				}
				inputs = []linuxProbeSelectionInput{{
					reference: symbol.reference, request: symbol.request,
					dependencies: slices.Clone(symbol.dependencies),
				}}
				values = []string{symbol.falseText, symbol.trueText}
			} else {
				inputs = symbol.selectionInputs
				values = symbol.selectionValues
			}

			if err := state.reserve(len(values)); err != nil {
				return "", linuxProbeMakeTextProtocolUnusable, err
			}
			state.visiting[token] = true
			transformedValues := make([]string, len(values))
			var err error
			for index, branch := range values {
				var branchMode linuxProbeMakeTextProtocolMode
				transformedValues[index], branchMode, err = s.transformCompositionalSubstText(branch, old, replacement, state, depth+1)
				if err != nil {
					delete(state.visiting, token)
					return "", linuxProbeMakeTextProtocolUnusable, err
				}
				tokenMode = combineLinuxProbeMakeTextProtocolMode(tokenMode, branchMode)
			}
			delete(state.visiting, token)
			tokenValue, err = evaluator.renderSelection(inputs, transformedValues)
			if err != nil {
				return "", linuxProbeMakeTextProtocolUnusable, err
			}
		default:
			return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("Linux probe symbolic value %q has unsupported kind %q", token, symbol.kind)
		}
		state.memo[token] = kbuildSymbolicSubstMemo{value: tokenValue, protocolMode: tokenMode}
		transformed.WriteString(tokenValue)
		if transformed.Len() > maxKbuildSymbolicComparisonBytes {
			return "", linuxProbeMakeTextProtocolUnusable, fmt.Errorf("symbolic Make subst result exceeds %d bytes", maxKbuildSymbolicComparisonBytes)
		}
		protocolMode = combineLinuxProbeMakeTextProtocolMode(protocolMode, tokenMode)
		last = end
	}
	if err := appendBoundedSymbolicSubstLiteral(&transformed, value[last:], old, replacement); err != nil {
		return "", linuxProbeMakeTextProtocolUnusable, err
	}
	return transformed.String(), protocolMode, nil
}

func combineLinuxProbeMakeTextProtocolMode(modes ...linuxProbeMakeTextProtocolMode) linuxProbeMakeTextProtocolMode {
	combined := linuxProbeMakeTextProtocolExact
	for _, mode := range modes {
		if mode == linuxProbeMakeTextProtocolUnusable {
			return mode
		}
		if mode == linuxProbeMakeTextProtocolArgvWords {
			combined = mode
		} else if mode == linuxProbeMakeTextProtocolCanonicalWords && combined == linuxProbeMakeTextProtocolExact {
			combined = mode
		}
	}
	return combined
}

func linuxProbeMakeTextSubstProtocolMode(mode linuxProbeMakeTextProtocolMode, old, replacement string) linuxProbeMakeTextProtocolMode {
	if mode != linuxProbeMakeTextProtocolArgvWords && mode != linuxProbeMakeTextProtocolCanonicalWords {
		return mode
	}
	if old == "" || replacement == "" || strings.ContainsAny(old, " \t\r\n\v\f") || strings.ContainsAny(replacement, " \t\r\n\v\f") {
		return linuxProbeMakeTextProtocolUnusable
	}
	// Compositional subst preserves argv words under these restrictions, but it
	// is not a canonical-output proof: spacing can be retained or introduced by
	// the exact source expression.
	return linuxProbeMakeTextProtocolArgvWords
}

func (s *KbuildProbeScopes) makeTextInputProtocolMode(values ...string) (linuxProbeMakeTextProtocolMode, error) {
	mode := linuxProbeMakeTextProtocolExact
	visiting := map[string]bool{}
	visited := map[string]bool{}
	work := 0
	step := func() error {
		if work >= maxProbeValueFragments {
			return fmt.Errorf("whole Make text protocol proof exceeds %d work items", maxProbeValueFragments)
		}
		work++
		return nil
	}
	var inspect func(string, int) error
	inspect = func(value string, depth int) error {
		if depth > maxKbuildSymbolicComparisonDepth {
			return fmt.Errorf("whole Make text protocol proof is too deep")
		}
		for token := range linuxProbeSymbolPattern.AllString(value) {
			if visited[token] {
				continue
			}
			if visiting[token] {
				return fmt.Errorf("cyclic Linux probe symbolic value %q in protocol proof", token)
			}
			if err := step(); err != nil {
				return err
			}
			_, symbol, ok := s.symbolOwner(token)
			if !ok {
				return fmt.Errorf("unknown Linux probe symbolic value %q in protocol proof", token)
			}
			visiting[token] = true
			switch symbol.kind {
			case "boolean":
				if err := step(); err != nil {
					return err
				}
				if err := inspect(symbol.trueText, depth+1); err != nil {
					return err
				}
				if err := step(); err != nil {
					return err
				}
				if err := inspect(symbol.falseText, depth+1); err != nil {
					return err
				}
			case "selection":
				for _, selected := range symbol.selectionValues {
					if err := step(); err != nil {
						return err
					}
					if err := inspect(selected, depth+1); err != nil {
						return err
					}
				}
			case "transformed-text":
				if symbol.textTransform == nil {
					return fmt.Errorf("Linux transformed text symbolic value %q has no transform", token)
				}
				if err := inspect(symbol.textTransform.sourceToken, depth+1); err != nil {
					return err
				}
			case "make-text":
				if symbol.makeText == nil {
					return fmt.Errorf("Linux whole Make text symbolic value %q has no expression", token)
				}
				mode = combineLinuxProbeMakeTextProtocolMode(mode, symbol.makeText.protocolMode)
			case "text", "toolset-path-literal":
			default:
				return fmt.Errorf("Linux probe symbolic value %q has unsupported kind %q in protocol proof", token, symbol.kind)
			}
			delete(visiting, token)
			visited[token] = true
		}
		return nil
	}
	for _, value := range values {
		if err := inspect(value, 0); err != nil {
			return linuxProbeMakeTextProtocolUnusable, err
		}
	}
	return mode, nil
}

func appendBoundedSymbolicSubstLiteral(out *strings.Builder, literal, old, replacement string) error {
	remaining := maxKbuildSymbolicComparisonBytes - out.Len()
	if remaining < 0 || len(literal) > remaining {
		return fmt.Errorf("symbolic Make subst result exceeds %d bytes", maxKbuildSymbolicComparisonBytes)
	}
	if extra := len(replacement) - len(old); extra > 0 {
		count := strings.Count(literal, old)
		if count > (remaining-len(literal))/extra {
			return fmt.Errorf("symbolic Make subst result exceeds %d bytes", maxKbuildSymbolicComparisonBytes)
		}
	}
	out.WriteString(strings.ReplaceAll(literal, old, replacement))
	return nil
}

// transformFiniteSymbolicMakeFunction evaluates one pure GNU Make function
// over finite boolean/selection inputs. filter and filter-out are wordwise, so
// concrete patterns and sufficiently large word-aligned atom sets are
// transformed compositionally: every atom keeps its own finite selection
// instead of forming a global 2^N table. Small and coupled expressions retain
// the bounded whole-expression fallback below.
func (s *KbuildProbeScopes) transformFiniteSymbolicMakeFunction(function string, args []string) (string, bool, error) {
	bindings, symbols, hasText, err := s.collectSymbolicComparison(args...)
	if err != nil {
		return "", true, err
	}
	if hasText {
		// filter and filter-out are compositional over complete Make words.
		// This is the real Kbuild shape for source-derived flag lists: retain
		// one runtime transform per dynamic word and filter literals now.
		if transformed, recognized, transformErr := s.transformWordwiseFiniteSymbolicFilter(function, args); recognized || transformErr != nil {
			return transformed, recognized, transformErr
		}
		// A single complete text atom is handled by the generic transformed-
		// text path. Mixed, coupled, or embedded text graphs fall through to
		// the exact opaque Make-AST fallback.
		return "", false, nil
	}
	if len(bindings) == 0 {
		return "", true, fmt.Errorf("Make function %q has no finite symbolic input", function)
	}
	if function == "filter" || function == "filter-out" {
		if transformed, recognized, transformErr := s.transformWordwiseFiniteSymbolicFilter(function, args); recognized || transformErr != nil {
			return transformed, true, transformErr
		}
	}
	// Guard before both the shift and allocation. The fallback is intentionally
	// bounded; large independent word-aligned expressions are handled by the
	// compositional path, while large coupled expressions fail closed.
	if len(bindings) > maxKbuildSymbolicComparisonReferences {
		if transformed, recognized, transformErr := s.transformWordwiseFiniteSymbolicFilter(function, args); recognized || transformErr != nil {
			return transformed, true, transformErr
		}
		return "", true, fmt.Errorf(
			"symbolic Make function %q has %d independent probe results, maximum is %d",
			function, len(bindings), maxKbuildSymbolicComparisonReferences,
		)
	}
	evaluator := bindings[0].evaluator
	inputs := make([]linuxProbeSelectionInput, len(bindings))
	for index, binding := range bindings {
		if binding.evaluator != evaluator {
			return "", true, fmt.Errorf("symbolic Make function %q crosses configured compiler scopes", function)
		}
		inputs[index] = binding.input
	}
	values := make([]string, 1<<len(bindings))
	for state := range values {
		expandedArgs := slices.Clone(args)
		for index := range expandedArgs {
			expandedArgs[index], err = expandSymbolicComparison(expandedArgs[index], state, bindings, symbols)
			if err != nil {
				return "", true, err
			}
		}
		value, recognized, evalErr := evalPureKbuildMakeFunction(function, expandedArgs, "")
		if evalErr != nil {
			return "", true, evalErr
		}
		if !recognized {
			return "", true, fmt.Errorf("Make function %q has no finite symbolic evaluator", function)
		}
		values[state] = value
	}
	transformed, err := evaluator.renderSelection(inputs, values)
	return transformed, true, err
}

// transformWordwiseFiniteSymbolicFilter is the exact O(n) representation for
// the common Kbuild shape:
//
//	$(filter concrete-patterns,literal $(finite-choice) ...)
//
// GNU Make applies filter/filter-out independently to each text word. A finite
// atom occupying one complete word can therefore be filtered in isolation and
// re-interned over only its own inputs. Concatenating those results in source
// word order preserves duplicate words and exact replay identity without ever
// enumerating the product of unrelated probe results.
//
// Symbolic patterns are not separable because one selected pattern may match
// any value word. They use the v6 dynamic-transform protocol below, preserving
// the exact cross-product semantics at execution without enumerating it in the
// planner. Symbols embedded in a larger value word use that same aggregate
// runtime protocol: concatenating the fragments before filtering preserves
// the word exactly without enumerating every possible spelling.
func (s *KbuildProbeScopes) transformWordwiseFiniteSymbolicFilter(function string, args []string) (string, bool, error) {
	if len(args) != 2 {
		return "", false, nil
	}
	dynamicValueProtocol, err := s.containsDynamicMakeTextProtocol(args[1])
	if err != nil {
		return "", true, err
	}
	if linuxProbeSymbolPattern.MatchString(args[0]) || dynamicValueProtocol {
		return s.transformDynamicSymbolicFilter(function, args)
	}

	patterns := args[0]
	words := strings.Fields(args[1])
	transformedWords := make([]string, 0, len(words))
	hasSymbol := false
	for _, word := range words {
		matches := linuxProbeSymbolPattern.FindAllString(word, -1)
		if len(matches) == 0 {
			transformed, recognized, err := evalPureKbuildMakeFunction(function, []string{patterns, word}, "")
			if err != nil {
				return "", true, err
			}
			if !recognized {
				return "", true, fmt.Errorf("Make function %q has no finite symbolic evaluator", function)
			}
			transformedWords = append(transformedWords, strings.Fields(transformed)...)
			continue
		}
		if len(matches) != 1 || matches[0] != word {
			return s.transformDynamicSymbolicFilter(function, args)
		}

		evaluator, symbol, ok := s.symbolOwner(word)
		if !ok {
			return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", word)
		}
		hasSymbol = true

		var inputs []linuxProbeSelectionInput
		var values []string
		switch symbol.kind {
		case "boolean", "selection":
			// One complete source word remains separable from every neighboring
			// word even when its selected branch contains another finite probe
			// atom (the ARM Kbuild nested cc-option fallback is the canonical
			// example). Flatten only this word's bounded local graph instead of
			// forcing all independent words into one global truth table.
			bindings, symbols, hasText, collectErr := s.collectSymbolicComparison(word)
			if collectErr != nil {
				return "", true, collectErr
			}
			if hasText || len(bindings) == 0 || len(bindings) > maxKbuildSymbolicComparisonReferences {
				return "", false, nil
			}
			evaluator = bindings[0].evaluator
			inputs = make([]linuxProbeSelectionInput, len(bindings))
			for index, binding := range bindings {
				if binding.evaluator != evaluator {
					return "", true, fmt.Errorf("symbolic Make function %q crosses configured compiler scopes", function)
				}
				inputs[index] = binding.input
			}
			values = make([]string, 1<<len(bindings))
			for state := range values {
				values[state], collectErr = expandSymbolicComparison(word, state, bindings, symbols)
				if collectErr != nil {
					return "", true, collectErr
				}
			}
		case "text", "transformed-text", "toolset-path-literal":
			transformed, err := evaluator.renderTextTransform(word, function, []string{patterns, word}, 1)
			if err != nil {
				return "", true, err
			}
			transformedWords = append(transformedWords, transformed)
			continue
		case "make-text":
			if symbol.makeText == nil {
				return "", true, fmt.Errorf("Linux whole Make text symbolic value %q has no expression", word)
			}
			transformed, recognized, err := s.transformWordwiseFiniteSymbolicFilter(
				function, []string{patterns, symbol.makeText.protocolValue},
			)
			if err != nil {
				return "", true, err
			}
			if !recognized {
				// The nested value has an exact Make AST even when it has no
				// safe argv/word protocol. Decline the compositional lowering so
				// transformSymbolic can retain this complete outer expression as
				// another exact AST. This is required by Kbuild's cmd-check while
				// automatic variables are deliberately unbound during discovery.
				return "", false, nil
			}
			transformedWords = append(transformedWords, transformed)
			continue
		default:
			return "", true, fmt.Errorf("Linux probe symbolic value %q has unsupported kind %q", word, symbol.kind)
		}

		filteredValues := make([]string, len(values))
		for index, value := range values {
			// A branch containing another atom is coupled to that atom. Let the
			// bounded fallback flatten it as one expression instead of treating
			// the nested token as a literal Make word.
			if linuxProbeSymbolPattern.MatchString(value) {
				return "", false, nil
			}
			filtered, recognized, err := evalPureKbuildMakeFunction(function, []string{patterns, value}, "")
			if err != nil {
				return "", true, err
			}
			if !recognized {
				return "", true, fmt.Errorf("Make function %q has no finite symbolic evaluator", function)
			}
			filteredValues[index] = filtered
		}
		transformed := word
		if !slices.Equal(filteredValues, values) {
			var err error
			transformed, err = evaluator.renderSelection(inputs, filteredValues)
			if err != nil {
				return "", true, err
			}
		}
		transformedWords = append(transformedWords, strings.Fields(transformed)...)
	}
	if !hasSymbol {
		return "", false, nil
	}
	transformed := strings.Join(transformedWords, " ")
	inputMode, err := s.makeTextInputProtocolMode(args...)
	if err != nil {
		return "", true, err
	}
	protocolMode := linuxProbeMakeTextProtocolCanonicalWords
	if inputMode == linuxProbeMakeTextProtocolUnusable {
		protocolMode = linuxProbeMakeTextProtocolUnusable
	}
	opaque, err := s.renderMakeText(function, args, transformed, protocolMode)
	return opaque, true, err
}

func (s *KbuildProbeScopes) containsDynamicMakeTextProtocol(value string) (bool, error) {
	if !linuxProbeSymbolPattern.MatchString(value) {
		return false, nil
	}
	_, symbols, _, err := s.collectSymbolicComparison(value)
	if err != nil {
		return false, err
	}
	for _, owned := range symbols {
		if owned.symbol.kind == "make-text" && owned.symbol.makeText != nil && len(owned.symbol.makeText.protocolTransforms) != 0 {
			return true, nil
		}
	}
	return false, nil
}

// transformDynamicSymbolicFilter retains the complete filter/filter-out as an
// exact replay AST and describes its process lowering as one runtime pure-Make
// transform. Both the pattern list and value list are lowered as fragment
// graphs, so the representation is linear in probe atoms and preserves cases
// where one dynamic pattern matches several unrelated dynamic or literal
// value words.
func (s *KbuildProbeScopes) transformDynamicSymbolicFilter(function string, args []string) (string, bool, error) {
	if (function != "filter" && function != "filter-out") || len(args) != 2 ||
		(!linuxProbeSymbolPattern.MatchString(args[0]) && !linuxProbeSymbolPattern.MatchString(args[1])) {
		return "", false, nil
	}
	if _, _, _, err := s.collectSymbolicComparison(args...); err != nil {
		return "", true, err
	}
	inputMode, err := s.makeTextInputProtocolMode(args...)
	if err != nil {
		return "", true, err
	}
	if inputMode == linuxProbeMakeTextProtocolUnusable {
		opaque, renderErr := s.renderMakeText(function, args, "", linuxProbeMakeTextProtocolUnusable)
		return opaque, true, renderErr
	}
	transformArgs := slices.Clone(args)
	transformArgs[1] = ""
	protocolTransform := linuxProbeMakeTextProtocolTransform{
		function: function, arguments: transformArgs, inputArgument: 1,
	}
	// filter and filter-out consume word lists and always emit one canonical
	// word list. Thus argv-word-equivalent nested inputs are sufficient to
	// reproduce the exact Make result even though their source separators may
	// differ.
	opaque, renderErr := s.renderMakeTextWithProtocolTransforms(
		function, args, args[1], []linuxProbeMakeTextProtocolTransform{protocolTransform}, linuxProbeMakeTextProtocolExact,
	)
	return opaque, true, renderErr
}

// transformDynamicSymbolicStrip applies strip to one complete runtime fragment
// graph. It is the fallback for embedded atoms and for whole-text values that
// cannot use the single-root chained path. The aggregate input must be exact:
// canonical-word and argv-word protocols may have already discarded a word
// separator at a literal/symbol boundary. Parser-context expressions remain
// excluded by transformSymbolic before this function is reached.
func (s *KbuildProbeScopes) transformDynamicSymbolicStrip(args []string) (string, bool, error) {
	if len(args) != 1 || !linuxProbeSymbolPattern.MatchString(args[0]) {
		return "", false, nil
	}
	mode, err := s.makeTextInputProtocolMode(args[0])
	if err != nil {
		return "", true, err
	}
	if mode != linuxProbeMakeTextProtocolExact {
		opaque, renderErr := s.renderMakeText("strip", args, "", linuxProbeMakeTextProtocolUnusable)
		return opaque, true, renderErr
	}
	returnValue, err := s.renderMakeTextWithProtocolTransforms(
		"strip",
		args,
		args[0],
		[]linuxProbeMakeTextProtocolTransform{{
			function: "strip", arguments: []string{""}, inputArgument: 0,
		}},
		linuxProbeMakeTextProtocolExact,
	)
	return returnValue, true, err
}

// transformCompositionalSymbolicStrip retains GNU Make's word normalization
// over a sequence of complete dynamic words. Each dynamic word is stripped
// independently and the symbolic sequence is marked for whole-value
// normalization during replay. This is exact because strip discards only word
// separators; embedded atoms are deliberately rejected.
func (s *KbuildProbeScopes) transformCompositionalSymbolicStrip(args []string) (string, bool, error) {
	if len(args) != 1 || !linuxProbeSymbolPattern.MatchString(args[0]) {
		return "", false, nil
	}
	words := strings.Fields(args[0])
	if len(words) == 1 {
		matches := linuxProbeSymbolPattern.FindAllString(words[0], -1)
		if len(matches) == 1 && matches[0] == words[0] {
			_, symbol, ok := s.symbolOwner(words[0])
			if ok && symbol.kind == "make-text" && symbol.makeText != nil &&
				symbol.makeText.protocolMode == linuxProbeMakeTextProtocolUnusable {
				mode, canonical := makeTextWordProtocolOutputMode(symbol.makeText.function)
				if canonical && mode == linuxProbeMakeTextProtocolCanonicalWords {
					// Every result of this Make function is already one canonical
					// word list. GNU Make strip is therefore an exact identity even
					// when the expression's action protocol is deliberately unusable.
					return words[0], true, nil
				}
			}
		}
	}
	transformedWords := make([]string, 0, len(words))
	for _, word := range words {
		matches := linuxProbeSymbolPattern.FindAllString(word, -1)
		if len(matches) == 0 {
			transformedWords = append(transformedWords, word)
			continue
		}
		if len(matches) != 1 || matches[0] != word {
			return s.transformDynamicSymbolicStrip(args)
		}
		evaluator, symbol, ok := s.symbolOwner(word)
		if !ok {
			return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", word)
		}
		switch symbol.kind {
		case "text", "transformed-text", "toolset-path-literal":
			transformed, err := evaluator.renderTextTransform(word, "strip", []string{word}, 0)
			if err != nil {
				return "", true, err
			}
			transformedWords = append(transformedWords, transformed)
		case "make-text":
			if symbol.makeText == nil {
				return "", true, fmt.Errorf("Linux whole Make text symbolic value %q has no expression", word)
			}
			if symbol.makeText.protocolMode == linuxProbeMakeTextProtocolCanonicalWords ||
				symbol.makeText.protocolMode == linuxProbeMakeTextProtocolArgvWords {
				// strings.Fields proved this child occupies one complete Make
				// word boundary. The ordinary whole-root strip chain can therefore
				// turn either word-equivalent protocol into exact canonical text;
				// this would not be sound for a token adjacent to literal bytes.
				transformed, recognized, err := s.transformChainedMakeTextProtocol("strip", []string{word})
				if err != nil {
					return "", true, err
				}
				if recognized {
					transformedWords = append(transformedWords, transformed)
					continue
				}
			}
			if symbol.makeText.protocolMode == linuxProbeMakeTextProtocolExact ||
				len(symbol.makeText.protocolTransforms) != 0 {
				return s.transformDynamicSymbolicStrip(args)
			}
			transformed, recognized, err := s.transformCompositionalSymbolicStrip([]string{symbol.makeText.protocolValue})
			if err != nil {
				return "", true, err
			}
			if !recognized {
				return "", true, fmt.Errorf(
					"Make strip cannot lower nested whole Make text %q from %s with %s protocol",
					word,
					symbol.makeText.function,
					symbol.makeText.protocolMode,
				)
			}
			transformedWords = append(transformedWords, transformed)
		case "boolean", "selection":
			var inputs []linuxProbeSelectionInput
			var values []string
			if symbol.kind == "boolean" {
				inputs = []linuxProbeSelectionInput{{
					reference: symbol.reference, request: symbol.request,
					dependencies: slices.Clone(symbol.dependencies),
				}}
				values = []string{symbol.falseText, symbol.trueText}
			} else {
				inputs = symbol.selectionInputs
				values = symbol.selectionValues
			}
			stripped := make([]string, len(values))
			for index, value := range values {
				if linuxProbeSymbolPattern.MatchString(value) {
					return "", false, nil
				}
				stripped[index] = strings.Join(strings.Fields(value), " ")
			}
			transformed, err := evaluator.renderSelection(inputs, stripped)
			if err != nil {
				return "", true, err
			}
			transformedWords = append(transformedWords, transformed)
		default:
			return "", true, fmt.Errorf("Linux probe symbolic value %q has unsupported kind %q", word, symbol.kind)
		}
	}
	transformed := strings.Join(transformedWords, " ")
	inputMode, err := s.makeTextInputProtocolMode(args...)
	if err != nil {
		return "", true, err
	}
	protocolMode := linuxProbeMakeTextProtocolCanonicalWords
	if inputMode == linuxProbeMakeTextProtocolUnusable {
		protocolMode = linuxProbeMakeTextProtocolUnusable
	}
	opaque, err := s.renderMakeText("strip", args, transformed, protocolMode)
	return opaque, true, err
}

func (s *KbuildProbeScopes) symbolOwner(token string) (*LinuxProbeEvaluator, linuxProbeSymbol, bool) {
	for _, scope := range []string{"target", "host"} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		if symbol, ok := evaluator.symbols[token]; ok {
			return evaluator, symbol, true
		}
	}
	return nil, linuxProbeSymbol{}, false
}

// selectSymbolic turns a Make comparison containing probe atoms into either a
// statically proven selection or one exact derived boolean atom. It
// deliberately does not consult the replay oracle: discovery and replay must
// construct the same dependency graph.
//
// Boolean atoms have finite source-defined strings, so a bounded truth table
// can retain compound and ordered selections over several independent probe
// results. The resulting atom lowers to composite ProbePredicates; nested
// symbolic branch text is flattened before it reaches argv.
func (s *KbuildProbeScopes) selectSymbolic(value, expected string, equal bool, trueText, falseText string) (string, bool, error) {
	valueMatches := linuxProbeSymbolPattern.FindAllString(value, -1)
	expectedMatches := linuxProbeSymbolPattern.FindAllString(expected, -1)
	if len(valueMatches) == 0 && len(expectedMatches) == 0 {
		return "", false, nil
	}
	if trueText == falseText {
		if _, _, _, err := s.collectSymbolicComparison(value, expected); err != nil {
			return "", true, err
		}
		return trueText, true, nil
	}
	contextual, contextualErr := s.containsContextualMakeText(value, expected, trueText, falseText)
	if contextualErr != nil {
		return "", true, contextualErr
	}
	if contextual {
		comparison := "not-equal"
		if equal {
			comparison = "equal"
		}
		// This token is only a discovery guard. Replay reparses wildcard and
		// foreach concretely before reaching the comparison, so resolving this
		// internal selection outside parser context remains an error by design.
		opaque, err := s.renderMakeText(
			"context-select",
			[]string{value, expected, comparison, trueText, falseText},
			"",
			linuxProbeMakeTextProtocolUnusable,
		)
		return opaque, true, err
	}
	if len(valueMatches) == 0 && len(expectedMatches) == 1 && expectedMatches[0] == expected {
		value, expected = expected, value
		valueMatches, expectedMatches = expectedMatches, valueMatches
	}
	if len(valueMatches) == 1 && valueMatches[0] == value && len(expectedMatches) == 0 {
		_, symbol, ok := s.symbolOwner(value)
		if !ok {
			return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", value)
		}
		// Text results are unbounded, but the protocol can compare one complete
		// text result to one concrete string exactly.
		if symbol.kind == "text" {
			// A target consumer may previously have adopted a host text token.
			// Select the evaluator from the complete reference graph instead of
			// target-first local ownership, or the comparison request itself would
			// be created in the wrong scope.
			comparisonEvaluator, compatibilityErr := s.compatibleSymbolicEvaluator(value)
			if compatibilityErr != nil {
				return "", true, compatibilityErr
			}
			if linuxProbeSymbolPattern.MatchString(trueText) || linuxProbeSymbolPattern.MatchString(falseText) {
				return "", true, fmt.Errorf("text-probe Make selection has nested symbolic branch text")
			}
			truth, err := comparisonEvaluator.compareSymbolicString(value, expected, equal)
			if err != nil {
				return "", true, err
			}
			selected, err := comparisonEvaluator.renderTruth(truth, trueText, falseText)
			return selected, true, err
		}
	}
	bindings, symbols, hasText, err := s.collectSymbolicComparison(value, expected, trueText, falseText)
	if err != nil {
		return "", true, err
	}
	if comparisonEqual, known := symbolicComparisonLiteralProof(value, expected); known {
		if comparisonEqual == equal {
			return trueText, true, nil
		}
		return falseText, true, nil
	}
	if expected == "" && len(valueMatches) == 1 && valueMatches[0] == value {
		_, symbol, ok := s.symbolOwner(value)
		if !ok {
			return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", value)
		}
		if len(bindings) > maxKbuildSymbolicComparisonReferences &&
			(symbol.kind == "boolean" || symbol.kind == "selection") {
			selected, projected, projectionErr := s.selectFiniteSymbolicEmptiness(
				value, symbol, equal, trueText, falseText,
			)
			if projectionErr != nil {
				return "", true, projectionErr
			}
			if projected {
				return selected, true, nil
			}
		}
		if symbol.kind == "make-text" {
			emptiness, proofErr := s.symbolicMakeTextEmptiness(value)
			if proofErr != nil {
				return "", true, proofErr
			}
			if emptiness != kbuildSymbolicEmptinessUnknown {
				comparisonEqual := emptiness == kbuildSymbolicEmptinessEmpty
				if comparisonEqual == equal {
					return trueText, true, nil
				}
				return falseText, true, nil
			}
			selected, materializeErr := s.selectMaterializedMakeTextEmpty(value, equal, trueText, falseText)
			if materializeErr != nil {
				return "", true, materializeErr
			}
			return selected, true, nil
		}
	}
	if hasText || len(bindings) > maxKbuildSymbolicComparisonReferences {
		// Text in a comparison operand is unbounded and cannot be enumerated.
		// Text or many independent atoms solely in the selected branches are
		// different: the discriminator is still finite, so retain each branch as
		// an opaque nested symbolic value in the selection table. This is the
		// shape produced by Kbuild's nested $(if ...): the first pass classifies a
		// dynamic command check to one boolean atom, and the second pass selects
		// command text containing many independently probed compiler flags.
		comparisonBindings, comparisonSymbols, comparisonHasText, comparisonErr :=
			s.collectSymbolicComparison(value, expected)
		if comparisonErr != nil {
			return "", true, comparisonErr
		}
		if !comparisonHasText && len(comparisonBindings) != 0 {
			if len(comparisonBindings) > maxKbuildSymbolicComparisonReferences {
				return "", true, fmt.Errorf(
					"symbolic Make comparison has %d independent probe results, maximum is %d",
					len(comparisonBindings), maxKbuildSymbolicComparisonReferences,
				)
			}
			selectedValues := make([]string, 1<<len(comparisonBindings))
			for state := range selectedValues {
				expandedValue, expandErr := expandSymbolicComparison(value, state, comparisonBindings, comparisonSymbols)
				if expandErr != nil {
					return "", true, expandErr
				}
				expandedExpected, expandErr := expandSymbolicComparison(expected, state, comparisonBindings, comparisonSymbols)
				if expandErr != nil {
					return "", true, expandErr
				}
				selectedValues[state] = falseText
				if (expandedValue == expandedExpected) == equal {
					selectedValues[state] = trueText
				}
			}
			evaluator := comparisonBindings[0].evaluator
			for _, binding := range comparisonBindings[1:] {
				if binding.evaluator != evaluator {
					return "", true, fmt.Errorf("symbolic Make selection crosses configured compiler scopes")
				}
			}
			// renderSelection owns the complete nested branch graph. Validate
			// that its evaluator may consume every branch atom now, rather than
			// deferring a cross-scope failure until argv lowering.
			for _, branch := range []string{trueText, falseText} {
				for token := range linuxProbeSymbolPattern.AllString(branch) {
					if _, exists, adoptErr := evaluator.adoptSymbol(token); adoptErr != nil {
						return "", true, adoptErr
					} else if !exists {
						return "", true, fmt.Errorf("unknown Linux probe symbolic value %q", token)
					}
				}
			}
			selected, renderErr := evaluator.renderSelection(comparisonBindingsToInputs(comparisonBindings), selectedValues)
			return selected, true, renderErr
		}
		return "", true, fmt.Errorf(
			"symbolic Make comparison with compound text probe values cannot be represented exactly: %q and %q",
			value, expected,
		)
	}
	if len(bindings) > maxKbuildSymbolicComparisonReferences {
		return "", true, fmt.Errorf(
			"symbolic Make comparison has %d independent probe results, maximum is %d",
			len(bindings), maxKbuildSymbolicComparisonReferences,
		)
	}

	selectedValues := make([]string, 1<<len(bindings))
	for state := range selectedValues {
		expandedValue, expandErr := expandSymbolicComparison(value, state, bindings, symbols)
		if expandErr != nil {
			return "", true, expandErr
		}
		expandedExpected, expandErr := expandSymbolicComparison(expected, state, bindings, symbols)
		if expandErr != nil {
			return "", true, expandErr
		}
		selected := falseText
		if (expandedValue == expandedExpected) == equal {
			selected = trueText
		}
		selectedValues[state], expandErr = expandSymbolicComparison(selected, state, bindings, symbols)
		if expandErr != nil {
			return "", true, expandErr
		}
	}
	evaluator := bindings[0].evaluator
	inputs := make([]linuxProbeSelectionInput, len(bindings))
	for index, binding := range bindings {
		if binding.evaluator != evaluator {
			return "", true, fmt.Errorf("symbolic Make selection crosses configured compiler scopes")
		}
		inputs[index] = binding.input
	}
	selected, err := evaluator.renderSelection(inputs, selectedValues)
	return selected, true, err
}

func comparisonBindingsToInputs(bindings []kbuildSymbolicComparisonBinding) []linuxProbeSelectionInput {
	inputs := make([]linuxProbeSelectionInput, len(bindings))
	for index, binding := range bindings {
		inputs[index] = binding.input
	}
	return inputs
}

// selectFiniteSymbolicEmptiness projects a completed finite selection onto
// whether each of its source-defined result strings is empty. Nested compiler
// atoms in a command branch are not discriminator inputs when a fixed literal
// already proves that branch non-empty. Keeping only the original selection
// inputs prevents Kbuild's outer cmd helper from turning every compiler flag
// in cmd_* into one exponential truth table, while the selected command graph
// remains intact in trueText or falseText.
func (s *KbuildProbeScopes) selectFiniteSymbolicEmptiness(
	value string,
	symbol linuxProbeSymbol,
	equal bool,
	trueText string,
	falseText string,
) (string, bool, error) {
	var inputs []linuxProbeSelectionInput
	var branches []string
	switch symbol.kind {
	case "boolean":
		if symbol.reference.Kind != "boolean" {
			return "", true, fmt.Errorf("Linux boolean probe symbolic value %q has %q reference", value, symbol.reference.Kind)
		}
		inputs = []linuxProbeSelectionInput{{
			reference: symbol.reference, request: symbol.request,
			dependencies: slices.Clone(symbol.dependencies),
		}}
		branches = []string{symbol.falseText, symbol.trueText}
	case "selection":
		inputs = slices.Clone(symbol.selectionInputs)
		branches = slices.Clone(symbol.selectionValues)
	default:
		return "", false, nil
	}
	if len(inputs) == 0 || len(inputs) > maxKbuildSymbolicComparisonReferences || len(branches) != 1<<len(inputs) {
		return "", true, fmt.Errorf("invalid finite Linux probe symbolic value %q with %d inputs and %d values", value, len(inputs), len(branches))
	}
	selectedValues := make([]string, len(branches))
	for index, branch := range branches {
		emptiness, err := s.symbolicMakeTextEmptiness(branch)
		if err != nil {
			return "", true, err
		}
		if emptiness == kbuildSymbolicEmptinessUnknown {
			return "", false, nil
		}
		selectedValues[index] = falseText
		if (emptiness == kbuildSymbolicEmptinessEmpty) == equal {
			selectedValues[index] = trueText
		}
	}
	evaluator, err := s.compatibleSymbolicEvaluator(value, trueText, falseText)
	if err != nil {
		return "", true, err
	}
	selected, err := evaluator.renderSelection(inputs, selectedValues)
	return selected, true, err
}

// selectMaterializedMakeTextEmpty lowers one exact whole-Make-text value into
// a pure derived-text probe node, then compares that measured value with the
// empty string through the ordinary dependency predicate protocol. The first
// node runs no process: it applies the same bounded fragment transforms as
// process arguments and stdin, preserving map_directory's configured-action
// phase ordering without teaching the planner any compiler-specific facts.
func (s *KbuildProbeScopes) selectMaterializedMakeTextEmpty(
	value string,
	equal bool,
	trueText string,
	falseText string,
) (string, error) {
	evaluator, err := s.compatibleSymbolicEvaluator(value)
	if err != nil {
		return "", err
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	fragments, symbolic, err := lowerer.value(value)
	if err != nil {
		return "", fmt.Errorf("lower whole Make text for empty comparison: %w", err)
	}
	if !symbolic || len(fragments) == 0 || len(lowerer.dependencies) == 0 {
		return "", fmt.Errorf("whole Make text empty comparison has no dynamic protocol value")
	}
	materialized, err := evaluator.requestText(ProbeRequest{
		Schema:     LinuxProbeRequestSchema,
		InputCount: len(lowerer.dependencies),
		Outcome: ProbeOutcome{
			Kind:      "text",
			Fragments: fragments,
		},
	}, lowerer.dependencies...)
	if err != nil {
		return "", err
	}
	truth, err := evaluator.compareSymbolicString(materialized, "", equal)
	if err != nil {
		return "", err
	}
	return evaluator.renderTruth(truth, trueText, falseText)
}

// collectSymbolicComparison validates the complete expansion graph and returns
// unique boolean results in deterministic first-use order. Text atoms are
// recorded but never enumerated; callers either use a separately proven
// lowering or retain the complete expression as an exact opaque Make AST.
func (s *KbuildProbeScopes) collectSymbolicComparison(values ...string) (
	[]kbuildSymbolicComparisonBinding,
	map[string]kbuildOwnedSymbol,
	bool,
	error,
) {
	bindings := []kbuildSymbolicComparisonBinding{}
	bindingIndex := map[ProbeReference]int{}
	symbols := map[string]kbuildOwnedSymbol{}
	visiting := map[string]bool{}
	validated := map[string]bool{}
	hasText := false
	var collect func(string, int) error
	collect = func(value string, depth int) error {
		if depth > maxKbuildSymbolicComparisonDepth {
			return fmt.Errorf("symbolic Make comparison expansion is too deep")
		}
		for token := range linuxProbeSymbolPattern.AllString(value) {
			if validated[token] {
				continue
			}
			if visiting[token] {
				return fmt.Errorf("cyclic Linux probe symbolic value %q in Make comparison", token)
			}
			evaluator, symbol, ok := s.symbolOwner(token)
			if !ok {
				return fmt.Errorf("unknown Linux probe symbolic value %q", token)
			}
			if len(symbols) >= maxKbuildSymbolicComparisonSymbols {
				return fmt.Errorf(
					"symbolic Make comparison has more than %d nested values",
					maxKbuildSymbolicComparisonSymbols,
				)
			}
			owned := kbuildOwnedSymbol{evaluator: evaluator, symbol: symbol}
			symbols[token] = owned
			visiting[token] = true
			switch symbol.kind {
			case "boolean":
				if symbol.reference.Kind != "boolean" {
					return fmt.Errorf("Linux boolean probe symbolic value %q has %q reference", token, symbol.reference.Kind)
				}
				if _, exists := bindingIndex[symbol.reference]; !exists {
					bindingIndex[symbol.reference] = len(bindings)
					bindings = append(bindings, kbuildSymbolicComparisonBinding{
						evaluator: evaluator,
						input: linuxProbeSelectionInput{
							reference: symbol.reference, request: symbol.request,
							dependencies: slices.Clone(symbol.dependencies),
						},
					})
				}
				if err := collect(symbol.trueText, depth+1); err != nil {
					return err
				}
				if err := collect(symbol.falseText, depth+1); err != nil {
					return err
				}
			case "selection":
				for _, input := range symbol.selectionInputs {
					if _, exists := bindingIndex[input.reference]; exists {
						continue
					}
					bindingIndex[input.reference] = len(bindings)
					bindings = append(bindings, kbuildSymbolicComparisonBinding{evaluator: evaluator, input: input})
				}
				for _, selected := range symbol.selectionValues {
					if err := collect(selected, depth+1); err != nil {
						return err
					}
				}
			case "text", "transformed-text", "toolset-path-literal":
				if symbol.reference.Kind != "text" {
					if symbol.kind == "text" {
						return fmt.Errorf("Linux text probe symbolic value %q has %q reference", token, symbol.reference.Kind)
					}
				}
				hasText = true
			case "make-text":
				if symbol.makeText == nil {
					return fmt.Errorf("Linux whole Make text symbolic value %q has no expression", token)
				}
				hasText = true
				for _, nested := range append(slices.Clone(symbol.makeText.arguments), symbol.makeText.protocolValue) {
					if err := collect(nested, depth+1); err != nil {
						return err
					}
				}
				for _, transform := range symbol.makeText.protocolTransforms {
					for _, nested := range transform.arguments {
						if err := collect(nested, depth+1); err != nil {
							return err
						}
					}
				}
			default:
				return fmt.Errorf("Linux probe symbolic value %q has unsupported kind %q", token, symbol.kind)
			}
			delete(visiting, token)
			validated[token] = true
		}
		return nil
	}
	for _, value := range values {
		if err := collect(value, 0); err != nil {
			return nil, nil, false, err
		}
	}
	return bindings, symbols, hasText, nil
}

// symbolicMakeTextEmptiness proves only the exact-AST invariants needed by
// Kbuild's cmd-check idiom. It never consults makeText.protocolValue: that
// value is a process/word lowering proof, not the expression replayed by Make.
func (s *KbuildProbeScopes) symbolicMakeTextEmptiness(value string) (kbuildSymbolicEmptiness, error) {
	proof := &kbuildSymbolicEmptinessProof{
		visiting: map[string]bool{},
		memo:     map[string]kbuildSymbolicEmptiness{},
	}
	return s.symbolicMakeTextEmptinessAt(value, proof, 0)
}

type kbuildSymbolicEmptinessProof struct {
	visiting map[string]bool
	memo     map[string]kbuildSymbolicEmptiness
	work     int
}

func (p *kbuildSymbolicEmptinessProof) step() error {
	if p.work >= maxProbeValueFragments {
		return fmt.Errorf("symbolic Make emptiness proof exceeds %d work items", maxProbeValueFragments)
	}
	p.work++
	return nil
}

func (s *KbuildProbeScopes) symbolicMakeTextEmptinessAt(
	value string,
	proof *kbuildSymbolicEmptinessProof,
	depth int,
) (kbuildSymbolicEmptiness, error) {
	if depth > maxKbuildSymbolicComparisonDepth {
		return kbuildSymbolicEmptinessUnknown, fmt.Errorf("symbolic Make emptiness proof is too deep")
	}
	if len(value) > maxKbuildSymbolicComparisonBytes {
		return kbuildSymbolicEmptinessUnknown, fmt.Errorf(
			"symbolic Make emptiness proof exceeds %d bytes",
			maxKbuildSymbolicComparisonBytes,
		)
	}

	state := kbuildSymbolicEmptinessEmpty
	last := 0
	for start, end := range linuxProbeSymbolPattern.AllStringIndex(value) {
		literal := value[last:start]
		if strings.TrimSpace(literal) != "" {
			state = kbuildSymbolicEmptinessNonempty
		} else if literal != "" && state == kbuildSymbolicEmptinessEmpty {
			state = kbuildSymbolicEmptinessUnknown
		}

		token := value[start:end]
		if tokenState, ok := proof.memo[token]; ok {
			state = concatKbuildSymbolicEmptiness(state, tokenState)
			last = end
			continue
		}
		if proof.visiting[token] {
			return kbuildSymbolicEmptinessUnknown, fmt.Errorf("cyclic Linux probe symbolic value %q in emptiness proof", token)
		}
		if err := proof.step(); err != nil {
			return kbuildSymbolicEmptinessUnknown, err
		}
		_, symbol, ok := s.symbolOwner(token)
		if !ok {
			return kbuildSymbolicEmptinessUnknown, fmt.Errorf("unknown Linux probe symbolic value %q in emptiness proof", token)
		}
		proof.visiting[token] = true
		tokenState, err := s.symbolEmptiness(symbol, proof, depth+1)
		delete(proof.visiting, token)
		if err != nil {
			return kbuildSymbolicEmptinessUnknown, err
		}
		proof.memo[token] = tokenState
		state = concatKbuildSymbolicEmptiness(state, tokenState)
		last = end
	}
	literal := value[last:]
	if strings.TrimSpace(literal) != "" {
		state = kbuildSymbolicEmptinessNonempty
	} else if literal != "" && state == kbuildSymbolicEmptinessEmpty {
		state = kbuildSymbolicEmptinessUnknown
	}
	return state, nil
}

func concatKbuildSymbolicEmptiness(left, right kbuildSymbolicEmptiness) kbuildSymbolicEmptiness {
	if left == kbuildSymbolicEmptinessNonempty || right == kbuildSymbolicEmptinessNonempty {
		return kbuildSymbolicEmptinessNonempty
	}
	if left == kbuildSymbolicEmptinessEmpty && right == kbuildSymbolicEmptinessEmpty {
		return kbuildSymbolicEmptinessEmpty
	}
	return kbuildSymbolicEmptinessUnknown
}

func uniformKbuildSymbolicEmptiness(values []kbuildSymbolicEmptiness) kbuildSymbolicEmptiness {
	if len(values) == 0 {
		return kbuildSymbolicEmptinessUnknown
	}
	state := values[0]
	for _, value := range values[1:] {
		if value != state {
			return kbuildSymbolicEmptinessUnknown
		}
	}
	return state
}

func (s *KbuildProbeScopes) symbolEmptiness(
	symbol linuxProbeSymbol,
	proof *kbuildSymbolicEmptinessProof,
	depth int,
) (kbuildSymbolicEmptiness, error) {
	switch symbol.kind {
	case "boolean":
		branches := []string{symbol.falseText, symbol.trueText}
		states := make([]kbuildSymbolicEmptiness, len(branches))
		for index, branch := range branches {
			if err := proof.step(); err != nil {
				return kbuildSymbolicEmptinessUnknown, err
			}
			state, err := s.symbolicMakeTextEmptinessAt(branch, proof, depth)
			if err != nil {
				return kbuildSymbolicEmptinessUnknown, err
			}
			states[index] = state
		}
		return uniformKbuildSymbolicEmptiness(states), nil
	case "selection":
		states := make([]kbuildSymbolicEmptiness, len(symbol.selectionValues))
		for index, branch := range symbol.selectionValues {
			if err := proof.step(); err != nil {
				return kbuildSymbolicEmptinessUnknown, err
			}
			state, err := s.symbolicMakeTextEmptinessAt(branch, proof, depth)
			if err != nil {
				return kbuildSymbolicEmptinessUnknown, err
			}
			states[index] = state
		}
		return uniformKbuildSymbolicEmptiness(states), nil
	case "text", "transformed-text", "toolset-path-literal":
		return kbuildSymbolicEmptinessUnknown, nil
	case "make-text":
		if symbol.makeText == nil {
			return kbuildSymbolicEmptinessUnknown, fmt.Errorf("Linux whole Make text symbolic value has no expression")
		}
		makeText := symbol.makeText
		switch makeText.function {
		case "sort", "strip":
			if len(makeText.arguments) != 1 {
				return kbuildSymbolicEmptinessUnknown, nil
			}
			return s.symbolicMakeTextEmptinessAt(makeText.arguments[0], proof, depth)
		case "subst":
			if len(makeText.arguments) != 3 || linuxProbeSymbolPattern.MatchString(makeText.arguments[0]) || strings.TrimSpace(makeText.arguments[0]) != "" {
				return kbuildSymbolicEmptinessUnknown, nil
			}
			inputState, err := s.symbolicMakeTextEmptinessAt(makeText.arguments[2], proof, depth)
			if err != nil {
				return kbuildSymbolicEmptinessUnknown, err
			}
			if inputState == kbuildSymbolicEmptinessNonempty {
				return inputState, nil
			}
			return kbuildSymbolicEmptinessUnknown, nil
		case "filter-out":
			if len(makeText.arguments) != 2 || linuxProbeSymbolPattern.MatchString(makeText.arguments[0]) || len(strings.Fields(makeText.arguments[0])) != 0 {
				return kbuildSymbolicEmptinessUnknown, nil
			}
			return s.symbolicMakeTextEmptinessAt(makeText.arguments[1], proof, depth)
		default:
			return kbuildSymbolicEmptinessUnknown, nil
		}
	default:
		return kbuildSymbolicEmptinessUnknown, fmt.Errorf("Linux probe symbolic value has unsupported kind %q in emptiness proof", symbol.kind)
	}
}

// symbolicComparisonLiteralProof recognizes equal expressions and the common
// empty-string comparison where a literal non-space byte survives every atom
// substitution. It is valid for both finite boolean and arbitrary text atoms.
func symbolicComparisonLiteralProof(value, expected string) (bool, bool) {
	if value == expected {
		return true, true
	}
	fixedNonempty := func(candidate string) bool {
		literal := linuxProbeSymbolPattern.ReplaceAllString(candidate, "")
		return strings.TrimSpace(literal) != ""
	}
	if expected == "" && fixedNonempty(value) {
		return false, true
	}
	if value == "" && fixedNonempty(expected) {
		return false, true
	}
	return false, false
}

func expandSymbolicComparison(
	value string,
	state int,
	bindings []kbuildSymbolicComparisonBinding,
	symbols map[string]kbuildOwnedSymbol,
) (string, error) {
	bindingIndex := make(map[ProbeReference]int, len(bindings))
	for index, binding := range bindings {
		bindingIndex[binding.input.reference] = index
	}
	for depth := 0; depth <= maxKbuildSymbolicComparisonDepth; depth++ {
		if len(value) > maxKbuildSymbolicComparisonBytes {
			return "", fmt.Errorf(
				"symbolic Make comparison expansion exceeds %d bytes",
				maxKbuildSymbolicComparisonBytes,
			)
		}
		if !linuxProbeSymbolPattern.MatchString(value) {
			return value, nil
		}
		before := value
		var expandErr error
		value = linuxProbeSymbolPattern.ReplaceAllStringFunc(value, func(token string) string {
			if expandErr != nil {
				return token
			}
			binding, ok := symbols[token]
			if !ok {
				expandErr = fmt.Errorf("unknown Linux probe symbolic value %q", token)
				return token
			}
			switch binding.symbol.kind {
			case "boolean":
				index, ok := bindingIndex[binding.symbol.reference]
				if !ok {
					expandErr = fmt.Errorf("Linux probe symbolic value %q has no comparison binding", token)
					return token
				}
				if state&(1<<index) != 0 {
					return binding.symbol.trueText
				}
				return binding.symbol.falseText
			case "selection":
				selectionState := 0
				for inputIndex, input := range binding.symbol.selectionInputs {
					index, ok := bindingIndex[input.reference]
					if !ok {
						expandErr = fmt.Errorf("Linux probe symbolic selection %q has no binding for %q", token, input.reference.NodeID)
						return token
					}
					if state&(1<<index) != 0 {
						selectionState |= 1 << inputIndex
					}
				}
				if selectionState >= len(binding.symbol.selectionValues) {
					expandErr = fmt.Errorf("Linux probe symbolic selection %q has invalid state %d", token, selectionState)
					return token
				}
				return binding.symbol.selectionValues[selectionState]
			default:
				expandErr = fmt.Errorf("compound Linux text probe symbolic value %q cannot be expanded finitely", token)
				return token
			}
		})
		if expandErr != nil {
			return "", expandErr
		}
		if value == before {
			return "", fmt.Errorf("cyclic Linux probe symbolic value %q in Make comparison", value)
		}
	}
	return "", fmt.Errorf("symbolic Make comparison expansion is too deep")
}

func (s *KbuildProbeScopes) resolveSymbolicForScope(scope, value string) (string, error) {
	cacheKey := scope + "\x00" + value
	if resolved, ok := s.resolved.load(cacheKey); ok {
		return resolved, nil
	}
	if linuxProbeSymbolPattern.MatchString(value) {
		evaluator, err := s.compatibleSymbolicEvaluatorForConsumer(scope, value)
		if err != nil {
			return "", err
		}
		resolved, err := evaluator.ResolveSymbolic(value)
		if err != nil {
			return "", err
		}
		s.resolved.store(cacheKey, resolved)
		return resolved, nil
	}
	s.resolved.store(cacheKey, value)
	return value, nil
}

func (s *KbuildProbeScopes) resolveSymbolicStructureForScope(scope, value string) (string, error) {
	cacheKey := scope + "\x00" + value
	if resolved, ok := s.resolvedStructure.load(cacheKey); ok {
		return resolved, nil
	}
	if linuxProbeSymbolPattern.MatchString(value) {
		evaluator, err := s.compatibleSymbolicEvaluatorForConsumer(scope, value)
		if err != nil {
			return "", err
		}
		resolved, err := evaluator.ResolveSymbolicStructure(value)
		if err != nil {
			return "", err
		}
		s.resolvedStructure.store(cacheKey, resolved)
		return resolved, nil
	}
	s.resolvedStructure.store(cacheKey, value)
	return value, nil
}

func (s *KbuildProbeScopes) resolveSymbolicWordsForScope(scope, value string) (string, error) {
	for _, evaluator := range s.evaluators {
		if evaluator != nil && evaluator.oracle != nil {
			return s.resolveSymbolicForScope(scope, value)
		}
	}
	var consumer *LinuxProbeEvaluator
	if linuxProbeSymbolPattern.MatchString(value) {
		var err error
		consumer, err = s.compatibleSymbolicEvaluatorForConsumer(scope, value)
		if err != nil {
			return "", err
		}
	}
	for depth := 0; depth <= maxKbuildSymbolicComparisonDepth; depth++ {
		expanded, changed, expandErr := replaceKbuildSymbolicTokensBounded(value, func(token string) (string, bool, error) {
			symbol, ok, err := consumer.adoptSymbol(token)
			if err != nil {
				return "", false, err
			}
			if !ok {
				return "", false, fmt.Errorf("unknown Linux probe symbolic value %q at Make word boundary", token)
			}
			if symbol.kind != "make-text" {
				return token, false, nil
			}
			if symbol.makeText == nil {
				return "", false, fmt.Errorf("Linux whole Make text symbolic value %q has no expression", token)
			}
			if symbol.makeText.protocolMode == linuxProbeMakeTextProtocolUnusable {
				return "", false, fmt.Errorf(
					"Linux whole Make text symbolic value %q from Make function %q has no proven Make-word lowering",
					token,
					symbol.makeText.function,
				)
			}
			if len(symbol.makeText.protocolTransforms) != 0 {
				return token, false, nil
			}
			return symbol.makeText.protocolValue, true, nil
		})
		if expandErr != nil {
			return "", expandErr
		}
		value = expanded
		if !changed {
			return value, nil
		}
		if len(value) > MaxProbeInterpolatedBytes {
			return "", fmt.Errorf("symbolic Make word value exceeds %d bytes", MaxProbeInterpolatedBytes)
		}
	}
	return "", fmt.Errorf("symbolic Make word lowering is too deep")
}

func replaceKbuildSymbolicTokensBounded(
	value string,
	replace func(string) (string, bool, error),
) (string, bool, error) {
	if len(value) > MaxProbeInterpolatedBytes {
		return "", false, fmt.Errorf("symbolic Make word value exceeds %d bytes", MaxProbeInterpolatedBytes)
	}
	indices := linuxProbeSymbolPattern.FindAllStringIndex(value, -1)
	if len(indices) == 0 {
		return value, false, nil
	}
	var expanded strings.Builder
	changed := false
	appendValue := func(fragment string) error {
		if len(fragment) > MaxProbeInterpolatedBytes-expanded.Len() {
			return fmt.Errorf("symbolic Make word value exceeds %d bytes", MaxProbeInterpolatedBytes)
		}
		expanded.WriteString(fragment)
		return nil
	}
	last := 0
	for _, index := range indices {
		if err := appendValue(value[last:index[0]]); err != nil {
			return "", false, err
		}
		token := value[index[0]:index[1]]
		replacement, replaced, err := replace(token)
		if err != nil {
			return "", false, err
		}
		if err := appendValue(replacement); err != nil {
			return "", false, err
		}
		changed = changed || replaced
		last = index[1]
	}
	if err := appendValue(value[last:]); err != nil {
		return "", false, err
	}
	return expanded.String(), changed, nil
}

func (s *KbuildProbeScopes) kbuildShell(
	ctx context.Context,
	preferredScope string,
	command string,
	fallback func(string) (string, error),
) (string, error) {
	toolScopes, err := s.firstConfiguredToolScopes(command, preferredScope)
	if err != nil {
		return "", err
	}
	if len(toolScopes) != 0 {
		selected := toolScopes[0]
		value, err := s.evaluators[selected].KbuildShell(ctx, command)
		if err == nil {
			return value, nil
		}
		if !IsLinuxProbeUnsupportedCommand(err) {
			return "", fmt.Errorf("evaluate %s-scoped Kbuild compiler command: %w", selected, err)
		}
		// The selected scoped token is authoritative. Do not reinterpret a
		// malformed configured-tool command as a noncompiler shell expression.
		return "", err
	}
	var unsupported []error
	for _, scope := range []string{preferredScope, oppositeKbuildProbeScope(preferredScope)} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		value, err := evaluator.KbuildShell(ctx, command)
		if err == nil {
			return value, nil
		}
		if !IsLinuxProbeUnsupportedCommand(err) {
			return "", fmt.Errorf("evaluate %s-scoped Kbuild compiler command: %w", scope, err)
		}
		unsupported = append(unsupported, err)
	}
	if fallback != nil {
		value, fallbackErr := fallback(command)
		if fallbackErr == nil {
			return value, nil
		}
		// Preserve the evaluator's narrow unhandled-command provenance while
		// retaining the fallback's more specific diagnostic. The parser may then
		// offer this command to the generic immutable-source query lowerer; owned
		// compiler/source-script failures never enter this branch.
		unsupported = append(unsupported, fallbackErr)
	}
	return "", errors.Join(unsupported...)
}

// kbuildShellResultAvailable performs the same source-provenance routing as
// kbuildShell, but it only observes completed symbolic evaluations. It never
// runs a probe or falls back to an arbitrary shell evaluator.
func (s *KbuildProbeScopes) kbuildShellResultAvailable(preferredScope, command string) bool {
	command = strings.TrimSpace(command)
	toolScopes, err := s.firstConfiguredToolScopes(command, preferredScope)
	if err != nil {
		return false
	}
	if len(toolScopes) != 0 {
		evaluator := s.evaluators[toolScopes[0]]
		if evaluator == nil {
			return false
		}
		_, ok := evaluator.kbuildShellResults.load(command)
		return ok
	}
	for _, scope := range []string{preferredScope, oppositeKbuildProbeScope(preferredScope)} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		if _, ok := evaluator.kbuildShellResults.load(command); ok {
			return true
		}
	}
	return false
}

// firstConfiguredToolScopes returns the concrete scope selected by the first
// source-traced action-role token in shell order. Auto-scoped roles follow an
// explicit token in the same field or the caller's preferred scope. Executable
// spellings are deliberately ignored: target and host may select the same
// artifact, and only source-carried provenance is authoritative.
func (s *KbuildProbeScopes) firstConfiguredToolScopes(command, preferredScope string) ([]string, error) {
	for _, field := range strings.Fields(command) {
		field = strings.Trim(field, `"'();`)
		field = strings.TrimSuffix(field, `\`)
		if field == "" {
			continue
		}
		refs, err := KbuildActionRoleRefs(field)
		if err != nil {
			return nil, fmt.Errorf("invalid configured action-role provenance: %w", err)
		}
		if len(refs) == 0 {
			continue
		}
		explicitScopes := make([]string, 0, len(refs))
		for _, ref := range refs {
			if ref.Scope == KbuildActionRoleAutoScope {
				continue
			}
			if s.evaluators[ref.Scope] == nil {
				return nil, fmt.Errorf("configured action-role token references unavailable %s scope", ref.Scope)
			}
			if !slices.Contains(explicitScopes, ref.Scope) {
				explicitScopes = append(explicitScopes, ref.Scope)
			}
		}
		if len(explicitScopes) > 1 {
			return nil, fmt.Errorf("one shell field mixes configured target and host action-role provenance")
		}
		selectedScope := preferredScope
		if len(explicitScopes) == 1 {
			selectedScope = explicitScopes[0]
		}
		evaluator := s.evaluators[selectedScope]
		if evaluator == nil {
			return nil, fmt.Errorf("configured action-role token references unavailable %s scope", selectedScope)
		}
		for _, sourceRef := range refs {
			ref, matches := kbuildActionRoleRefForScope(sourceRef, selectedScope)
			if !matches {
				return nil, fmt.Errorf("one shell field mixes configured target and host action-role provenance")
			}
			if evaluator.tools[ref.Role] == "" {
				return nil, fmt.Errorf("configured action-role token references unavailable %s role %q", selectedScope, ref.Role)
			}
		}
		return []string{selectedScope}, nil
	}
	return nil, nil
}

func oppositeKbuildProbeScope(scope string) string {
	if scope == "target" {
		return "host"
	}
	return "target"
}

func (s *KbuildProbeScopes) References() []ProbeReference {
	if s == nil {
		return nil
	}
	seen := map[string]bool{}
	var references []ProbeReference
	for _, scope := range []string{"target", "host"} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		for _, reference := range evaluator.References() {
			if !seen[reference.NodeID] {
				seen[reference.NodeID] = true
				references = append(references, reference)
			}
		}
	}
	return references
}

// canonicalCompilerPredefineArguments clones arguments and canonicalizes only
// the replacement body of a direct, object-like -D whose name is one literal
// ASCII C identifier. The config-dependency consumer observes defined macro
// names, not replacement tokens, so GCC/Clang-compatible preprocessors give the
// same observable state for every such body. Keeping the -D/-U name, position,
// and joined/separated shape preserves option ordering and candidate ownership.
//
// An opaque compiler wrapper which assigns meaning to the raw replacement bytes
// beyond ordinary GCC/Clang -D semantics is intentionally outside this bounded
// projection. Function-like, malformed, and dynamically named definitions stay
// byte-exact so the probe continues to fail closed for unmodeled grammar.
func canonicalCompilerPredefineArguments(role string, arguments []string) []string {
	canonical := slices.Clone(arguments)
	separatedPayloads := compactKbuildCompilerSeparatedOptionPayloads(role, canonical)
	for index := 0; index < len(canonical); index++ {
		if separatedPayloads[index] {
			continue
		}
		argument := canonical[index]
		if argument == "--" {
			break
		}
		if argument == "-D" {
			if index+1 >= len(canonical) || !separatedPayloads[index+1] {
				continue
			}
			if operand, ok := canonicalCompilerPredefineObjectMacro(canonical[index+1]); ok {
				canonical[index+1] = operand
			}
			index++
			continue
		}
		// Preserve any -D-looking operand owned by the candidate grammar even if
		// the Kbuild command parser does not model that option (for example
		// -mllvm). Such invocations normally fail closed before reaching this API.
		if argument == "-mllvm" || argument == "-Xpreprocessor" ||
			probeCandidateOptionRequiresScalar(argument, ProbeCandidatePolicyCC) {
			index++
			continue
		}
		operand, joined := strings.CutPrefix(argument, "-D")
		if !joined || operand == "" {
			continue
		}
		if operand, ok := canonicalCompilerPredefineObjectMacro(operand); ok {
			canonical[index] = "-D" + operand
		}
	}
	return canonical
}

func canonicalCompilerPredefineObjectMacro(operand string) (string, bool) {
	name, _, _ := strings.Cut(operand, "=")
	identifier, ok := configDependencyMacroIdentifier(name)
	if !ok || identifier != name {
		return "", false
	}
	return name + "=1", true
}

// CompilerPredefines registers (during discovery) or replays (during final
// planning) one compiler predefine dump for the config-dependency defined-name
// projection. arguments contains only the source-selected portion of the
// configured compiler invocation: proberun's identity-bound tool proxy adds the
// selected role's configured prefix, suffix, and environment around these
// arguments at execution time. translationUnits names exact argv words which
// can remain inside a dependency-backed fragment and must be removed before
// the managed /dev/null query. Direct object-like -D replacement bodies are
// canonicalized on a private copy before request identity. Arguments hidden in
// symbolic Make-text fragments receive the same projection in proberun after
// they render and before candidate validation; executable ActionRecipe
// arguments remain untouched.
//
// The bool result is false only during discovery, when the returned probe text
// is intentionally still symbolic. Callers must fail closed for that pass and
// repeat the same request during replay before interpreting the text.
func (s *KbuildProbeScopes) CompilerPredefines(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) (string, bool, error) {
	return s.compilerProjectedText(scope, role, language, arguments, translationUnits, environment,
		"compiler-predefines", []string{"-dM", "-E", "-x", language, "/dev/null"}, "")
}

// CompilerDefinedness measures every requested name, including explicit false
// results. Names are validated, sorted and deduplicated on a private copy before
// request identity; their source must remain stable across discovery and replay.
// Like CompilerPredefines, this queries the configured initial compiler state,
// before source-selected includes. It does not mutate a predefine dump or infer
// absence from a compiler family. A nil error with ready=false means discovery;
// unsupported projections and unavailable/malformed results remain errors.
func (s *KbuildProbeScopes) CompilerDefinedness(
	scope, role, language string,
	arguments, translationUnits, names []string,
	environment map[string]string,
) (map[string]bool, bool, error) {
	ordered, stdin, err := compilerDefinednessSource(names)
	if err != nil {
		return nil, false, unsupportedCompilerPredefineProjection(err)
	}
	contents, ready, err := s.compilerProjectedText(scope, role, language, arguments, translationUnits, environment,
		"compiler-definedness", []string{"-E", "-P", "-x", language, "-"}, stdin)
	if err != nil || !ready {
		return nil, false, err
	}
	result, err := parseCompilerDefinednessResult(ordered, contents)
	return result, err == nil, err
}

func parseCompilerDefinednessResult(ordered []string, contents string) (map[string]bool, error) {
	if len(contents) > MaxProbeInterpolatedBytes {
		return nil, fmt.Errorf("compiler definedness result exceeds %d bytes", MaxProbeInterpolatedBytes)
	}
	// Only preprocessing whitespace may separate the exact 0/1 tokens. Extra
	// diagnostics, missing values, and non-Boolean values cannot prove absence.
	values := strings.FieldsFunc(contents, func(character rune) bool {
		return character == ' ' || character == '\t' || character == '\n' ||
			character == '\r' || character == '\v' || character == '\f'
	})
	if len(values) != len(ordered) {
		return nil, fmt.Errorf("compiler definedness returned %d values, want %d", len(values), len(ordered))
	}
	for _, value := range values {
		if value != "0" && value != "1" {
			return nil, fmt.Errorf("compiler definedness returned a non-Boolean value")
		}
	}
	result := make(map[string]bool, len(ordered))
	for index, name := range ordered {
		result[name] = values[index] == "1"
	}
	return result, nil
}

const compilerDollarPunctuationSource = "#undef linux_bzl_dollar_punctuation_v1\n" +
	"#define linux_bzl_dollar_punctuation_v1(a) #a$\n" +
	"linux_bzl_dollar_punctuation_v1(731)\n"

const compilerDollarPunctuationPattern = `^[ \t\r\n\v\f]*"731"[ \t\r\n\v\f]*\$[ \t\r\n\v\f]*$`

var compilerDollarPunctuationOutput = regexp.MustCompile(compilerDollarPunctuationPattern)

// CompilerDollarPunctuation measures a positive lexical capability in the
// configured invocation, without changing its dollar options. Stringification
// of a formal followed by a separate '$' avoids depending on the absence of a
// predefined $NAME macro. Some assembler preprocessors accept #a$ unchanged,
// so success alone is insufficient: stdout must also match the exact proof.
// A negative measurement is ready, but grants no identifier-mode authority.
func (s *KbuildProbeScopes) CompilerDollarPunctuation(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
) (bool, bool, error) {
	const stepName = "compiler-dollar-punctuation"
	probe, err := s.compilerProjectedRequest(scope, role, language, arguments, translationUnits, environment,
		stepName, []string{"-E", "-P", "-x", language, "-"}, compilerDollarPunctuationSource,
		ProbeOutcome{Kind: "boolean", Predicate: &ProbePredicate{Operator: "all", Operands: []ProbePredicate{
			{Operator: "exit-zero", Step: stepName},
			{Operator: "stream-matches", Step: stepName, Stream: "stdout", Value: compilerDollarPunctuationPattern},
		}}})
	if err != nil {
		return false, false, err
	}
	truth, err := probe.evaluator.requestTruth(probe.request, probe.dependencies...)
	if err != nil {
		return false, false, err
	}
	token, err := probe.evaluator.renderTruth(truth, "1", "0")
	if err != nil {
		return false, false, err
	}
	resolved, err := probe.evaluator.ResolveSymbolic(token)
	if err != nil {
		return false, false, fmt.Errorf("resolve %s %s compiler dollar punctuation: %w", scope, role, err)
	}
	if resolved == token {
		return false, false, nil
	}
	if resolved != "0" && resolved != "1" {
		return false, false, fmt.Errorf("compiler dollar punctuation returned a non-Boolean result")
	}
	// Validate the exact reduction as well as readBoolean's request, toolset and
	// step-shape checks. A malformed oracle summary must not grant permission.
	result, err := probe.evaluator.readProbeResult(truth.reference, truth.request, truth.dependencies...)
	if err != nil {
		return false, false, err
	}
	step, present := probeResultStep(result.Steps, stepName)
	if !present || step.Status == "skipped" {
		return false, false, fmt.Errorf("compiler dollar punctuation result has no executed proof step")
	}
	positive := step.Status == "success" && step.ExitCode == 0 && compilerDollarPunctuationOutput.MatchString(step.Stdout)
	if positive != (resolved == "1") {
		return false, false, fmt.Errorf("compiler dollar punctuation result disagrees with its exact reduction")
	}
	return positive, true, nil
}

func compilerDefinednessSource(names []string) ([]string, string, error) {
	// Bound caller-owned input before copying/sorting it, as well as the final
	// canonical program. Duplicates cannot create unbounded preprocessing work.
	if len(names) > MaxProbeDynamicArgumentWords {
		return nil, "", fmt.Errorf("compiler definedness has too many names")
	}
	nameBytes := 0
	for _, name := range names {
		if len(name) > MaxProbeInterpolatedBytes-nameBytes {
			return nil, "", fmt.Errorf("compiler definedness names exceed %d bytes", MaxProbeInterpolatedBytes)
		}
		nameBytes += len(name)
		if identifier, valid := configDependencyMacroIdentifier(name); !valid || identifier != name {
			return nil, "", fmt.Errorf("compiler definedness has an invalid macro name")
		}
	}
	ordered := slices.Clone(names)
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	const prefix, suffix = "#if defined(", ")\n1\n#else\n0\n#endif\n"
	var source strings.Builder
	for _, name := range ordered {
		if len(name) > MaxProbeInterpolatedBytes-source.Len()-len(prefix)-len(suffix) {
			return nil, "", fmt.Errorf("compiler definedness stdin exceeds %d bytes", MaxProbeInterpolatedBytes)
		}
		source.WriteString(prefix)
		source.WriteString(name)
		source.WriteString(suffix)
	}
	return ordered, source.String(), nil
}

// compilerProjectedText shares the source-argument projection, symbolic
// lowering, environment security and identity-bound tool proxy contract of
// both initial-state queries. Keep the legacy predefine request bytes unchanged.
func (s *KbuildProbeScopes) compilerProjectedText(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
	stepName string,
	managedArguments []string,
	stdin string,
) (string, bool, error) {
	probe, err := s.compilerProjectedRequest(scope, role, language, arguments, translationUnits, environment,
		stepName, managedArguments, stdin,
		ProbeOutcome{Kind: "text", Step: stepName, Stream: "stdout", RequireSuccess: true})
	if err != nil {
		return "", false, err
	}
	token, err := probe.evaluator.requestText(probe.request, probe.dependencies...)
	if err != nil {
		return "", false, err
	}
	resolved, err := probe.evaluator.ResolveSymbolic(token)
	if err != nil {
		return "", false, fmt.Errorf("resolve %s %s compiler predefines: %w", scope, role, err)
	}
	if resolved == token {
		return "", false, nil
	}
	return resolved, true, nil
}

type compilerProjectedProbe struct {
	evaluator    *LinuxProbeEvaluator
	request      ProbeRequest
	dependencies []ProbeReference
}

// Construction is shared by text and Boolean initial-state probes. Outcome
// selection does not change candidate ownership, projection or tool authority.
func (s *KbuildProbeScopes) compilerProjectedRequest(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
	stepName string,
	managedArguments []string,
	stdin string,
	outcome ProbeOutcome,
) (*compilerProjectedProbe, error) {
	return s.compilerProjectedRequestWithProjection(scope, role, language, arguments, translationUnits, environment,
		stepName, managedArguments, stdin, outcome, ProbeCandidateProjectionCompilerPredefines)
}

// Existing initial-state queries keep their canonicalizing projection and exact
// request bytes. Value-sensitive intrinsic queries select a distinct projection,
// including after dependency-backed argv fragments have rendered in proberun.
func (s *KbuildProbeScopes) compilerProjectedRequestWithProjection(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
	stepName string,
	managedArguments []string,
	stdin string,
	outcome ProbeOutcome,
	projection string,
) (*compilerProjectedProbe, error) {
	context, err := s.compilerProjectedContext(scope, role, language, arguments, translationUnits, environment, projection)
	if err != nil {
		return nil, err
	}
	// This path owns a fresh context. Transfer it without the extra deep copy
	// needed when multiple queries borrow a retained context.
	return context.consume(stepName, managedArguments, stdin, outcome)
}

// compilerQueryContext owns the source-selected, authenticated argument and
// environment templates before any particular query is attached. It is local
// to its owning evaluator: this is not yet a serialized discovery handoff or
// a source-closure proof. Neither query construction nor returned requests may
// mutate the context. Predefine and value-sensitive contexts remain distinct.
type compilerQueryContext struct {
	evaluator    *LinuxProbeEvaluator
	scope        string
	step         ProbeStep
	dependencies []ProbeReference
}

func (s *KbuildProbeScopes) compilerProjectedContext(
	scope, role, language string,
	arguments, translationUnits []string,
	environment map[string]string,
	projection string,
) (*compilerQueryContext, error) {
	if s == nil || s.evaluators[scope] == nil {
		return nil, fmt.Errorf("Kbuild probe workload has no %s scope", scope)
	}
	if role != "cc" && role != "cxx" {
		return nil, fmt.Errorf("Kbuild compiler predefines require cc or cxx role, got %q", role)
	}
	switch language {
	case "c", "c++", "assembler-with-cpp":
	default:
		return nil, fmt.Errorf("Kbuild compiler predefines have unsupported language %q", language)
	}
	evaluator := s.evaluators[scope]
	if evaluator.tools[role] == "" {
		return nil, fmt.Errorf("Kbuild probe workload %s scope has no %s role", scope, role)
	}
	switch projection {
	case ProbeCandidateProjectionCompilerPredefines:
		arguments = canonicalCompilerPredefineArguments(role, arguments)
	case ProbeCandidateProjectionCompilerIntrinsic:
		arguments = slices.Clone(arguments)
	default:
		return nil, fmt.Errorf("unsupported compiler query projection %q", projection)
	}
	lowerer := newProbeSymbolicValueLowerer(evaluator)
	base, conditional, fragments, candidate, err := lowerProbeCandidateArguments(
		lowerer, arguments, probeCandidateArgumentMask(len(arguments)), ProbeCandidatePolicyCC,
	)
	if err != nil {
		return nil, unsupportedCompilerPredefineProjection(fmt.Errorf(
			"lower %s %s compiler predefine arguments: %w", scope, role, err,
		))
	}
	if candidate != nil {
		candidate.Projection = projection
		canonicalTranslationUnits := slices.Clone(translationUnits)
		slices.Sort(canonicalTranslationUnits)
		candidate.TranslationUnits = slices.Compact(canonicalTranslationUnits)
	} else if len(translationUnits) != 0 {
		return nil, unsupportedCompilerPredefineProjection(fmt.Errorf(
			"Kbuild compiler predefines have translation units but no source-owned arguments",
		))
	}
	literalEnvironment := maps.Clone(environment)
	environmentFragments := []ProbeEnvironmentFragments{}
	environmentNames := slices.Sorted(maps.Keys(literalEnvironment))
	for _, name := range environmentNames {
		valueFragments, symbolic, err := lowerer.value(literalEnvironment[name])
		if err != nil {
			return nil, unsupportedCompilerPredefineProjection(fmt.Errorf(
				"lower %s %s compiler predefine environment %s: %w", scope, role, name, err,
			))
		}
		if !symbolic {
			continue
		}
		environmentFragments = append(environmentFragments, ProbeEnvironmentFragments{
			Name: name, Fragments: valueFragments,
		})
		delete(literalEnvironment, name)
	}
	return &compilerQueryContext{
		evaluator: evaluator,
		scope:     scope,
		step: ProbeStep{
			Tool:                 role,
			Arguments:            base,
			ConditionalArguments: conditional,
			ArgumentFragments:    fragments,
			Candidate:            candidate,
			Environment:          literalEnvironment,
			EnvironmentFragments: environmentFragments,
		},
		dependencies: slices.Clone(lowerer.dependencies),
	}, nil
}

func (context *compilerQueryContext) query(
	stepName string,
	managedArguments []string,
	stdin string,
	outcome ProbeOutcome,
) (*compilerProjectedProbe, error) {
	if context == nil || context.evaluator == nil {
		return nil, fmt.Errorf("compiler query context is nil or consumed")
	}
	// Deep-copy also the candidate ownership masks and predicate trees, which
	// canonicalSourceRequest deliberately does not clone. A returned request
	// must not be able to change later queries instantiated from this context.
	request := cloneLinuxProbeRequest(ProbeRequest{
		Steps:   []ProbeStep{context.step},
		Outcome: outcome,
	})
	owned := *context
	owned.step = request.Steps[0]
	owned.dependencies = slices.Clone(context.dependencies)
	return owned.consume(stepName, managedArguments, stdin, request.Outcome)
}

// consume transfers a fresh, unshared context into one request. A retained
// context must use query instead. Clearing the source prevents accidental reuse.
func (context *compilerQueryContext) consume(
	stepName string,
	managedArguments []string,
	stdin string,
	outcome ProbeOutcome,
) (*compilerProjectedProbe, error) {
	if context == nil || context.evaluator == nil {
		return nil, fmt.Errorf("compiler query context is nil or consumed")
	}
	evaluator, scope, role := context.evaluator, context.scope, context.step.Tool
	dependencies := context.dependencies
	request := ProbeRequest{
		Schema:     LinuxProbeRequestSchema,
		InputCount: len(dependencies),
		Steps:      []ProbeStep{context.step},
		Outcome:    outcome,
	}
	*context = compilerQueryContext{}
	step := request.Steps[0]
	step.Name, step.Stdin = stepName, stdin
	step.Arguments = append(step.Arguments, managedArguments...)
	auxiliary := []string{}
	for referenced := range probeStepTemplateToolRoles(step) {
		if referenced == role {
			continue
		}
		if evaluator.tools[referenced] == "" {
			return nil, unsupportedCompilerPredefineProjection(fmt.Errorf(
				"Kbuild compiler predefine environment references unavailable %s tool role %q",
				scope, referenced,
			))
		}
		auxiliary = append(auxiliary, referenced)
	}
	sort.Strings(auxiliary)
	step.AuxiliaryTools = auxiliary
	request.Steps[0] = step
	// Symbol lowering can leave literal ActionRecipe capabilities (for example
	// ${work:root} in an exported Make value) which this standalone compiler
	// query cannot bind. Validate exactly the source-normalized request that
	// requestText/requestTruth will register, before it enters the discovery DAG.
	// Keep the executable environment intact and classify this optional projection as
	// unsupported; registration and oracle failures below remain real errors.
	request = evaluator.canonicalSourceRequest(request)
	if err := request.Validate(); err != nil {
		return nil, unsupportedCompilerPredefineProjection(fmt.Errorf(
			"validate %s %s compiler predefine projection: %w", scope, role, err,
		))
	}
	return &compilerProjectedProbe{evaluator: evaluator, request: request, dependencies: dependencies}, nil
}

// ImportToolsetPathCapabilities performs the typed phase handoff from an
// upstream Kconfig probe workload into this Kbuild workload. The upstream
// callback first authenticates and removes its transient tags. Kbuild then
// hides the deterministic provenance behind one stable symbolic atom so
// source-owned Make never observes either workload's authenticator. Replay
// seals the value with this workload's key immediately before symbolic use.
func (s *KbuildProbeScopes) ImportToolsetPathCapabilities(
	value string,
	normalizeUpstream func(string) (string, error),
) (string, error) {
	if s == nil {
		return "", fmt.Errorf("Kbuild probe workload is nil")
	}
	if normalizeUpstream == nil {
		return "", fmt.Errorf("Kconfig toolset-path normalizer is nil")
	}
	normalized, err := normalizeUpstream(value)
	if err != nil {
		return "", fmt.Errorf("authenticate Kconfig toolset path: %w", err)
	}
	if linuxProbeSymbolPattern.MatchString(normalized) {
		return "", fmt.Errorf("Kconfig toolset-path handoff contains an unresolved upstream probe symbol")
	}
	scopes := map[string]bool{}
	found := false
	if _, err := toolaction.RewriteExecutionRootProvenanceValue(normalized, func(scope, _ string) (string, error) {
		found = true
		scopes[scope] = true
		return "authenticated-toolset-path", nil
	}); err != nil {
		return "", fmt.Errorf("validate normalized Kconfig toolset path: %w", err)
	}
	if !found {
		return normalized, nil
	}

	var registry *linuxProbeSymbolRegistry
	for _, scope := range []string{"target", "host"} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		if evaluator.symbolRegistry == nil {
			return "", fmt.Errorf("Kbuild probe workload %s evaluator has no shared symbol registry", scope)
		}
		if registry != nil && registry != evaluator.symbolRegistry {
			return "", fmt.Errorf("Kbuild probe workload evaluators do not share toolset-path authority")
		}
		registry = evaluator.symbolRegistry
	}
	if registry == nil {
		return "", fmt.Errorf("Kbuild probe workload has no evaluator symbol registry")
	}
	digest := sha256.Sum256([]byte("linux-bzl-kconfig-toolset-path-handoff-v1\x00" + normalized))
	token := fmt.Sprintf("%s%x", linuxProbeSymbolPrefix, digest)
	symbol := linuxProbeSymbol{
		kind:               "toolset-path-literal",
		toolsetPathLiteral: normalized,
		toolsetPathScopes:  slices.Sorted(maps.Keys(scopes)),
	}
	if err := registry.publish(token, symbol); err != nil {
		return "", err
	}
	return token, nil
}

// BindActionPlanToolsetPathCapabilities attaches this workload's authenticated
// compiler-path normalizer to metadata produced inside the same callback.
// Source-owned Make transformations remain visible until action lowering, but
// no ephemeral capability tag or unauthenticated runtime token may cross the
// recipe content-addressing boundary.
func (s *KbuildProbeScopes) BindActionPlanToolsetPathCapabilities(metadata *CompactMetadata) error {
	if s == nil {
		return fmt.Errorf("Kbuild probe workload is nil")
	}
	if metadata == nil {
		return fmt.Errorf("Kbuild probe workload cannot bind nil metadata")
	}
	var registry *linuxProbeSymbolRegistry
	for _, scope := range []string{"target", "host"} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		if evaluator.symbolRegistry == nil {
			return fmt.Errorf("Kbuild probe workload %s evaluator has no shared symbol registry", scope)
		}
		if registry != nil && registry != evaluator.symbolRegistry {
			return fmt.Errorf("Kbuild probe workload evaluators do not share toolset-path authority")
		}
		registry = evaluator.symbolRegistry
	}
	if registry == nil {
		return fmt.Errorf("Kbuild probe workload has no evaluator symbol registry")
	}
	codec, err := registry.executionRootProvenanceCapabilityCodec()
	if err != nil {
		return err
	}
	metadata.toolsetPathCapabilityNormalizer = codec.NormalizeValue
	metadata.compilerProbeSourceShellWords = func(value string) (string, error) {
		evaluator, err := s.compatibleSymbolicEvaluator(value)
		if err != nil {
			return "", err
		}
		return evaluator.renderSourceShellWords(value)
	}
	metadata.compilerPredefines = s.CompilerPredefines
	metadata.compilerDefinedness = s.CompilerDefinedness
	metadata.compilerDollarPunctuation = s.CompilerDollarPunctuation
	if s.sourceGuardInventory == nil {
		s.sourceGuardInventory = &configDependencyGuardInventory{}
	}
	metadata.sourceGuardInventory = s.sourceGuardInventory
	return nil
}

// BindExactScriptEnvironments interns one complete source-owned environment
// for every configured compiler scope and returns a process-local activation
// closure. Activating a binding replaces, rather than overlays, the current
// environment, so a source unexport cannot retain a configured variable by
// accident. Repeated bindings reuse the same closure and adjacent activation
// of the same environment is a no-op.
func (s *KbuildProbeScopes) BindExactScriptEnvironments(
	exact map[string]map[string]string,
) (func() error, error) {
	if s == nil {
		return nil, fmt.Errorf("Kbuild probe workload is nil")
	}
	key, err := s.exactScriptEnvironmentsKey(exact)
	if err != nil {
		return nil, err
	}
	if s.exactScriptEnvironmentBindings == nil {
		s.exactScriptEnvironmentBindings = map[string]map[string]map[string]string{}
	}
	if s.exactScriptEnvironmentActivations == nil {
		s.exactScriptEnvironmentActivations = map[string]func() error{}
	}
	if previous, ok := s.exactScriptEnvironmentBindings[key]; ok {
		if !s.equalExactScriptEnvironments(previous, exact) {
			return nil, fmt.Errorf("Kbuild probe exact environment digest collision")
		}
		return s.exactScriptEnvironmentActivations[key], nil
	}
	// Caller maps remain mutable. Copy them only after the validated canonical
	// key proves this is a new binding, then retain the normalized copies as an
	// immutable workload-local snapshot.
	normalized := make(map[string]map[string]string, len(s.evaluators))
	for _, scope := range []string{"target", "host"} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		environment := maps.Clone(exact[scope])
		if environment == nil {
			environment = map[string]string{}
		}
		environment["ARCH"] = evaluator.architecture
		environment["SRCARCH"] = evaluator.sourceArchitecture
		normalized[scope] = environment
	}
	s.exactScriptEnvironmentBindings[key] = normalized
	activate := func() error { return s.activateExactScriptEnvironments(key) }
	s.exactScriptEnvironmentActivations[key] = activate
	return activate, nil
}

func (s *KbuildProbeScopes) activateExactScriptEnvironments(key string) error {
	if s.activeExactScriptEnvironment == key {
		s.activeScriptEnvironmentIdentity = key
		return nil
	}
	exact := s.exactScriptEnvironmentBindings[key]
	if exact == nil {
		return fmt.Errorf("Kbuild probe exact environment binding %q is unavailable", key)
	}
	unchanged := true
	for _, scope := range []string{"target", "host"} {
		if current := s.evaluators[scope]; current != nil && !maps.Equal(current.scriptEnvironment, exact[scope]) {
			unchanged = false
			break
		}
	}
	if unchanged {
		s.activeExactScriptEnvironment = key
		s.activeScriptEnvironmentIdentity = key
		return nil
	}
	refreshed := make(map[string]*LinuxProbeEvaluator, len(exact))
	for _, scope := range []string{"target", "host"} {
		current := s.evaluators[scope]
		if current == nil {
			continue
		}
		evaluator, err := current.WithScriptEnvironment(exact[scope])
		if err != nil {
			return fmt.Errorf("activate %s exact Kbuild probe environment: %w", scope, err)
		}
		refreshed[scope] = evaluator
	}
	for scope, evaluator := range refreshed {
		s.evaluators[scope] = evaluator
	}
	s.activeExactScriptEnvironment = key
	s.activeScriptEnvironmentIdentity = key
	s.resolved.clear()
	s.resolvedStructure.clear()
	return nil
}

func (s *KbuildProbeScopes) exactScriptEnvironmentsKey(environments map[string]map[string]string) (string, error) {
	for scope := range environments {
		if scope != "target" && scope != "host" {
			return "", fmt.Errorf("Kbuild probe workload has invalid %s scope", scope)
		}
		if s.evaluators[scope] == nil {
			return "", fmt.Errorf("Kbuild probe workload has no %s scope", scope)
		}
	}
	digest := sha256.New()
	write := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write([]byte(value))
	}
	for _, scope := range []string{"target", "host"} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		environment, ok := environments[scope]
		if !ok {
			return "", fmt.Errorf("Kbuild probe workload exact environment omits %s scope", scope)
		}
		for name, value := range environment {
			if !validKbuildCommandEnvironmentName(name) || strings.ContainsRune(value, 0) {
				return "", fmt.Errorf("Kbuild probe workload %s exact environment has invalid variable %q", scope, name)
			}
		}
		for _, invariant := range [...]struct{ name, value string }{
			{name: "ARCH", value: evaluator.architecture},
			{name: "SRCARCH", value: evaluator.sourceArchitecture},
		} {
			name, value := invariant.name, invariant.value
			if configured, exists := environment[name]; exists && configured != value {
				return "", fmt.Errorf(
					"Kbuild probe workload %s exact environment has %s=%q, want %q",
					scope, name, configured, value,
				)
			}
		}
		write(scope)
		names := make([]string, 0, len(environment)+2)
		for name := range environment {
			names = append(names, name)
		}
		if _, exists := environment["ARCH"]; !exists {
			names = append(names, "ARCH")
		}
		if _, exists := environment["SRCARCH"]; !exists {
			names = append(names, "SRCARCH")
		}
		slices.Sort(names)
		for _, name := range names {
			write(name)
			value, exists := environment[name]
			if !exists {
				switch name {
				case "ARCH":
					value = evaluator.architecture
				case "SRCARCH":
					value = evaluator.sourceArchitecture
				}
			}
			write(value)
		}
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func (s *KbuildProbeScopes) equalExactScriptEnvironments(
	canonical, candidate map[string]map[string]string,
) bool {
	if len(canonical) != len(s.evaluators) || len(candidate) != len(s.evaluators) {
		return false
	}
	for _, scope := range []string{"target", "host"} {
		evaluator := s.evaluators[scope]
		if evaluator == nil {
			continue
		}
		stored, storedOK := canonical[scope]
		values, valuesOK := candidate[scope]
		if !storedOK || !valuesOK {
			return false
		}
		wantLength := len(values)
		if _, exists := values["ARCH"]; !exists {
			wantLength++
		}
		if _, exists := values["SRCARCH"]; !exists {
			wantLength++
		}
		if len(stored) != wantLength {
			return false
		}
		for name, value := range values {
			if storedValue, exists := stored[name]; !exists || storedValue != value {
				return false
			}
		}
		if stored["ARCH"] != evaluator.architecture || stored["SRCARCH"] != evaluator.sourceArchitecture {
			return false
		}
	}
	return true
}

// RefreshScriptEnvironments atomically overlays one source-ordered export
// snapshot on each scope's original configured environment and replaces its
// evaluator. Starting from the immutable base prevents a variable exported by
// one later invocation from leaking into an earlier or sibling invocation
// which does not export it. Each replacement retains all capability symbols
// and references already discovered in the workload, but does not reuse
// environment-sensitive shell or resolution memos.
func (s *KbuildProbeScopes) RefreshScriptEnvironments(exported map[string]map[string]string) error {
	if s == nil {
		return fmt.Errorf("Kbuild probe workload is nil")
	}
	for scope := range exported {
		if scope != "target" && scope != "host" {
			return fmt.Errorf("Kbuild probe workload has invalid %s scope", scope)
		}
		if s.evaluators[scope] == nil {
			return fmt.Errorf("Kbuild probe workload has no %s scope", scope)
		}
	}
	refreshed := make(map[string]*LinuxProbeEvaluator, len(exported))
	changed := false
	for _, scope := range []string{"target", "host"} {
		values, selected := exported[scope]
		if !selected {
			continue
		}
		current := s.evaluators[scope]
		environment := maps.Clone(s.baseScriptEnvironments[scope])
		if environment == nil {
			environment = map[string]string{}
		}
		for name, value := range values {
			environment[name] = value
		}
		if maps.Equal(current.scriptEnvironment, environment) {
			continue
		}
		evaluator, err := current.WithScriptEnvironment(environment)
		if err != nil {
			return fmt.Errorf("refresh %s Kbuild probe environment: %w", scope, err)
		}
		refreshed[scope] = evaluator
		changed = true
	}
	if !changed {
		return nil
	}
	nextEnvironments := s.currentScriptEnvironments()
	for scope, evaluator := range refreshed {
		nextEnvironments[scope] = evaluator.scriptEnvironment
	}
	identity, err := s.exactScriptEnvironmentsKey(nextEnvironments)
	if err != nil {
		return fmt.Errorf("identify refreshed Kbuild probe environments: %w", err)
	}
	for scope, evaluator := range refreshed {
		s.evaluators[scope] = evaluator
	}
	s.activeExactScriptEnvironment = ""
	s.activeScriptEnvironmentIdentity = identity
	s.resolved.clear()
	s.resolvedStructure.clear()
	return nil
}

func (s *KbuildProbeScopes) currentScriptEnvironments() map[string]map[string]string {
	current := make(map[string]map[string]string, len(s.evaluators))
	for scope, evaluator := range s.evaluators {
		if evaluator != nil {
			current[scope] = evaluator.scriptEnvironment
		}
	}
	return current
}

// KbuildProbeEvaluation is the result of either discovery or replay. During
// discovery Plan is the exact action DAG to execute. During replay the same
// plan has first been validated against Oracle and Value contains the fully
// concrete graph/profile bundle returned by the caller's workload.
type KbuildProbeEvaluation[T any] struct {
	Value T
	Plan  *ProbePlan
}

// EvaluateKbuildProbeWorkload evaluates an arbitrary multi-profile Kbuild
// workload with one shared target/host ProbePlanBuilder. A nil oracle performs
// discovery and preserves probe atoms. A non-nil oracle performs replay,
// reconstructs the exact DAG, validates every result, and returns only the
// concretely resolved workload value.
func EvaluateKbuildProbeWorkload[T any](
	opts KbuildProbeWorkloadOptions,
	oracle *ProbeResultOracle,
	workload func(*KbuildProbeScopes) (T, error),
) (*KbuildProbeEvaluation[T], error) {
	if workload == nil {
		return nil, fmt.Errorf("Kbuild probe workload callback is nil")
	}
	if opts.Target.Facts == nil {
		return nil, fmt.Errorf("Kbuild probe workload target compiler facts are required")
	}
	targetIdentity := opts.Target.Facts.ToolsetIdentity()
	hostIdentity := ""
	if opts.Host != nil {
		if opts.Host.Facts == nil {
			return nil, fmt.Errorf("Kbuild probe workload host compiler facts are required")
		}
		hostIdentity = opts.Host.Facts.ToolsetIdentity()
	}
	builder, err := NewProbePlanBuilder(targetIdentity, hostIdentity)
	if err != nil {
		return nil, err
	}
	scopes := &KbuildProbeScopes{
		evaluators:                        map[string]*LinuxProbeEvaluator{},
		baseScriptEnvironments:            map[string]map[string]string{},
		exactScriptEnvironmentBindings:    map[string]map[string]map[string]string{},
		exactScriptEnvironmentActivations: map[string]func() error{},
	}
	symbolRegistry := newLinuxProbeSymbolRegistry()
	var lookup ProbeResultLookup
	if oracle != nil {
		lookup = oracle
	}
	add := func(scope string, options KbuildProbeScopeOptions) error {
		scopes.baseScriptEnvironments[scope] = maps.Clone(options.ScriptEnvironment)
		evaluator, err := NewLinuxProbeEvaluator(LinuxProbeEvaluatorOptions{
			Scope: scope, Architecture: options.Architecture, Facts: options.Facts,
			SourceRoot: options.SourceRoot, SourceArchitecture: options.SourceArchitecture,
			ScriptEnvironment: maps.Clone(options.ScriptEnvironment),
			Tools:             maps.Clone(options.Tools),
			Discovery:         builder, Oracle: lookup, RustSourceRoot: options.RustSourceRoot,
		})
		if err != nil {
			return err
		}
		evaluator.symbolRegistry = symbolRegistry
		scopes.evaluators[scope] = evaluator
		return nil
	}
	if err := add("target", opts.Target); err != nil {
		return nil, fmt.Errorf("create target Kbuild probe evaluator: %w", err)
	}
	if opts.Host != nil {
		if err := add("host", *opts.Host); err != nil {
			return nil, fmt.Errorf("create host Kbuild probe evaluator: %w", err)
		}
	}
	identity, err := scopes.exactScriptEnvironmentsKey(scopes.currentScriptEnvironments())
	if err != nil {
		return nil, fmt.Errorf("identify initial Kbuild probe environments: %w", err)
	}
	scopes.activeScriptEnvironmentIdentity = identity
	value, err := workload(scopes)
	if err != nil {
		return nil, err
	}
	plan, err := builder.Plan(scopes.References()...)
	if err != nil {
		return nil, err
	}
	if oracle != nil {
		if err := oracle.ValidatePlan(plan); err != nil {
			return nil, fmt.Errorf("validate replayed Kbuild probe plan: %w", err)
		}
	}
	return &KbuildProbeEvaluation[T]{Value: value, Plan: plan}, nil
}

// KbuildShell recognizes Linux's try-run wrapper before delegating the pure
// Kconfig command grammar. No shell, compiler, filesystem stat, or ambient
// executable is used here; the wrapper body becomes a ProbeRequest.
func (e *LinuxProbeEvaluator) KbuildShell(ctx context.Context, command string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("Linux probe evaluator is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if value, ok := e.kbuildShellResults.load(command); ok {
		return value, nil
	}
	looksLikeTryRun := strings.HasPrefix(command, "set -e;") &&
		strings.Contains(command, "TMP=") && strings.Contains(command, "if (")
	match := kbuildTryRunPattern.FindStringSubmatch(command)
	if match == nil {
		if looksLikeTryRun {
			return "", fmt.Errorf("unsupported Linux Kbuild try-run wrapper %q", command)
		}
		value, err := e.Shell(ctx, command)
		if err != nil {
			return "", err
		}
		e.kbuildShellResults.store(command, value)
		return value, nil
	}
	tempDir, trapDir, mkdirDir := match[1], match[2], match[3]
	// A source-defined TMPOUT=.tmp_$$$$ reaches the shell grammar as .tmp_$$:
	// Make has removed one escaping layer, while the real shell would replace
	// the remaining pair with its PID. The symbolic evaluator never executes
	// the wrapper, so retain and validate that exact token.
	if !validKbuildTryRunTempDir(tempDir) {
		return "", fmt.Errorf("Linux Kbuild try-run has invalid temporary directory %q", tempDir)
	}
	if trapDir != tempDir || mkdirDir != tempDir {
		return "", fmt.Errorf("Linux Kbuild try-run has inconsistent temporary directory")
	}
	probeCommand := strings.TrimSpace(match[4])
	if truth, recognized, probeErr := e.compoundTryRunProbe(probeCommand); recognized || probeErr != nil {
		if probeErr != nil {
			return "", probeErr
		}
		value, renderErr := e.renderTruth(truth, match[5], match[6])
		if renderErr != nil {
			return "", renderErr
		}
		e.kbuildShellResults.store(command, value)
		return value, nil
	}
	truth, err := e.commandSucceeds(probeCommand)
	if err != nil {
		return "", err
	}
	value, err := e.renderTruth(truth, match[5], match[6])
	if err != nil {
		return "", err
	}
	e.kbuildShellResults.store(command, value)
	return value, nil
}

func validKbuildTryRunTempDir(value string) bool {
	validBase := func(base string) bool {
		return kbuildTryRunTemp.MatchString(base) || base == ".tmp_$$"
	}
	if validBase(value) {
		return true
	}
	const sourcePrefix = "__LINUX_BZL_SOURCE_TREE__/"
	if !strings.HasPrefix(value, sourcePrefix) {
		return false
	}
	relative := strings.TrimPrefix(value, sourcePrefix)
	separator := strings.LastIndexByte(relative, '/')
	if separator <= 0 || !validBase(relative[separator+1:]) {
		return false
	}
	directory := relative[:separator]
	return !strings.ContainsAny(directory, "$%") &&
		validatePlanRelativePath("Kbuild try-run temporary directory", directory) == nil
}
