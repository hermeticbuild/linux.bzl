package kconfig

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// CompactKbuildObjectTreeObservation is the exact object-root visibility of
// immutable source scripts selected by one evaluated Kbuild command. A scoped
// observation lists canonical object-root-relative References. ObservesAll is
// reserved for a genuinely bare or dynamic root; it never follows merely from
// the presence of an immutable script.
type CompactKbuildObjectTreeObservation struct {
	ObservesObjectTree bool
	ObservesAll        bool
	References         []string
}

// ObserveCompactKbuildObjectTree classifies object-tree access in evaluated
// action text. Exact rooted paths retain a bounded reference set; a bare root
// or any dynamic path below it observes the complete source-visible frontier.
// Keeping this primitive independent from a particular command kind ensures
// ordinary argv, immutable scripts, and deferred Make-shell queries select the
// same dependency semantics.
func ObserveCompactKbuildObjectTree(values ...string) CompactKbuildObjectTreeObservation {
	builder := compactKbuildObjectTreeObservationBuilder{references: map[string]bool{}}
	for _, value := range values {
		builder.addValue(value)
	}
	return builder.result()
}

// EvaluateCompactKbuildSourceScriptObjectTreeObservation reports object-tree
// paths observable by immutable shell programs in one exact selected command.
// Only exported values actually read by the script (or a statically sourced
// wrapper), inline environment values it reads, and explicit script arguments
// confer access to the object root.
func EvaluateCompactKbuildSourceScriptObjectTreeObservation(
	profile CompactKbuildProfile,
	target, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) (CompactKbuildObjectTreeObservation, error) {
	return evaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget(
		profile, target, target, target, stem, normal, orderOnly, injected, command, true,
	)
}

// EvaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget retains
// the lexical rule-search and automatic-variable identities of a selected
// target while deriving its immutable script's exact object-tree reads.
func EvaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) (CompactKbuildObjectTreeObservation, error) {
	return evaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, command, true,
	)
}

// EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolic preserves
// compiler-probe atoms while deriving the same source-script observation. It
// is the stage-solver counterpart of final source-script lowering.
func EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolic(
	profile CompactKbuildProfile,
	target, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) (CompactKbuildObjectTreeObservation, error) {
	return evaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget(
		profile, target, target, target, stem, normal, orderOnly, injected, command, false,
	)
}

// EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolicForMakeTarget
// is the source-selection counterpart of the concrete lexical observation.
func EvaluateCompactKbuildSourceScriptObjectTreeObservationSymbolicForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
) (CompactKbuildObjectTreeObservation, error) {
	return evaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		injected, command, false,
	)
}

func evaluateCompactKbuildSourceScriptObjectTreeObservationForMakeTarget(
	profile CompactKbuildProfile,
	target, lookupTarget, automaticTarget, stem string,
	normal, orderOnly []string,
	injected map[string]string,
	command string,
	resolveSymbolic bool,
) (CompactKbuildObjectTreeObservation, error) {
	effectiveInjections, err := compactKbuildSourceScriptInjectionsForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
	)
	if err != nil {
		return CompactKbuildObjectTreeObservation{}, err
	}
	for name, value := range injected {
		effectiveInjections[name] = value
	}
	// Most selected recipes do not execute an immutable source script. Keep
	// those recipes outside the stricter source-script action lowerer: compiler
	// commands may legitimately contain constructs (including deferred Make
	// dollars) that are irrelevant to script environment observation.
	scripts, err := readCompactKbuildCommandSourceScriptsForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		effectiveInjections, command, resolveSymbolic,
	)
	if err != nil {
		return CompactKbuildObjectTreeObservation{}, err
	}
	if len(scripts) == 0 {
		return CompactKbuildObjectTreeObservation{}, nil
	}

	values, err := evaluateCompactKbuildTargetForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		effectiveInjections, resolveSymbolic, "CONFIG_SHELL",
	)
	if err != nil {
		return CompactKbuildObjectTreeObservation{}, err
	}
	environment, err := evaluateCompactKbuildTargetEnvironmentForMakeTarget(
		profile, target, lookupTarget, automaticTarget, stem, normal, orderOnly,
		effectiveInjections, resolveSymbolic,
	)
	if err != nil {
		return CompactKbuildObjectTreeObservation{}, err
	}

	canonicalCommand := compactKbuildProfileCanonicalRecipeText(profile, command)
	// command is already one target-evaluated Make value. Active automatic
	// variables were expanded quote-independently by the captured evaluator,
	// while escaped Make dollars have become shell-owned dollars. Do not pass it
	// through the final argv lowerer's automatic-variable phase again: doing so
	// could reinterpret a shell $@ from source $$@ as this Make target.
	// Project typed tree placeholders onto inert observation sentinels before
	// command-head classification. They retain source/object provenance without
	// looking like shell parameters to the compound scanner.
	commands, err := compactKbuildCompoundProgramCommands(
		compactKbuildCommandObservationText(profile, canonicalCommand),
	)
	if err != nil {
		return CompactKbuildObjectTreeObservation{}, fmt.Errorf("inspect source-script object-tree command for target %q: %w", target, err)
	}
	observation := compactKbuildObjectTreeObservationBuilder{references: map[string]bool{}}
	for _, parsed := range commands {
		configured, scope, err := compactKbuildSourceScriptCommandRoleContext(parsed)
		if err != nil {
			return CompactKbuildObjectTreeObservation{}, err
		}
		invocation, sourceScript, err := compactKbuildSourceScriptCommand(
			profile, parsed, values, scope, configured,
		)
		if err != nil {
			return CompactKbuildObjectTreeObservation{}, err
		}
		if !sourceScript {
			continue
		}
		argumentUsage := invocation.argumentUsage
		observationArguments := invocation.observationArguments
		if observationArguments == nil {
			observationArguments = invocation.scriptArguments
		}
		if argumentUsage.argumentVectorProgramUses > 0 &&
			argumentUsage.argumentVectorUses == argumentUsage.argumentVectorProgramUses &&
			!argumentUsage.positionalArgumentDynamic {
			// The immutable script executes its complete argv as one command. Keep
			// that command structure intact so a configured compiler role masks
			// passive -I/search-root operands exactly as it does in an ordinary
			// Kbuild recipe. Inspecting each word independently would turn `-I
			// ${tree:prep}` into a false whole-object-tree read.
			observation.addValue(compactKbuildProfileCanonicalRecipeText(
				profile, strings.Join(observationArguments, " "),
			))
			// A positional read outside the forwarded command still observes that
			// exact argument. Shell functions have their own positional namespace;
			// conservatively projecting a function-local $N onto the outer argv can
			// add a false positive, but cannot erase an object-tree input. This lets
			// wrappers such as checks.syscalls use function-local $1 without losing
			// the compiler structure of their separate top-level $* command.
			for index := range argumentUsage.positionalArgumentIndexes {
				if index > len(observationArguments) {
					continue
				}
				observation.addValue(compactKbuildProfileCanonicalRecipeText(
					profile, observationArguments[index-1],
				))
			}
		} else {
			for _, argument := range observationArguments {
				observation.addValue(compactKbuildProfileCanonicalRecipeText(profile, argument))
			}
		}

		effectiveEnvironment := make(map[string]string, len(environment)+len(invocation.environment))
		for name, value := range environment {
			effectiveEnvironment[name] = compactKbuildProfileCanonicalRecipeText(profile, value)
		}
		for name, value := range invocation.environment {
			effectiveEnvironment[name] = compactKbuildProfileCanonicalRecipeText(profile, value)
		}
		usage := invocation.environmentUsage
		if usage.ObservesAll {
			for _, value := range effectiveEnvironment {
				observation.addValue(value)
			}
		} else {
			for name := range usage.wholeValues {
				if value, ok := effectiveEnvironment[name]; ok {
					observation.addValue(value)
				}
			}
			for name, paths := range usage.valuePaths {
				value, ok := effectiveEnvironment[name]
				if !ok {
					continue
				}
				for pathname := range paths {
					observation.addValue(strings.TrimSuffix(value, "/") + "/" + pathname)
				}
			}
		}
	}
	return observation.result(), nil
}

func compactKbuildSourceScriptCommandRoleContext(
	command compactKbuildRecipeCommand,
) ([]KbuildActionRoleRef, string, error) {
	fields := append([]string{command.program}, command.arguments...)
	fields = append(fields, sortedStringMapValues(command.environment)...)
	refs := []KbuildActionRoleRef{}
	for _, field := range fields {
		fieldRefs, err := KbuildActionRoleRefs(field)
		if err != nil {
			return nil, "", err
		}
		refs = append(refs, fieldRefs...)
	}
	refs = canonicalKbuildActionRoleRefs(refs)
	scope := "target"
	host, target := false, false
	for _, ref := range refs {
		host = host || ref.Scope == "host"
		target = target || ref.Scope == "target"
	}
	if host && target {
		return nil, "", fmt.Errorf("source-script command carries both host and target action-role provenance")
	}
	if host {
		scope = "host"
	}
	return refs, scope, nil
}

type compactKbuildObjectTreeObservationBuilder struct {
	observes   bool
	all        bool
	references map[string]bool
}

func (b *compactKbuildObjectTreeObservationBuilder) addValue(value string) {
	value = compactKbuildObjectTreeObservableText(value)
	for _, marker := range []string{
		"__LINUX_BZL_OBJECT_TREE__",
		compactKbuildActionObjectTreeMarker,
		compactKbuildActionAbsoluteObjectTreeMarker,
		"${tree:prep}",
		"${work:root}",
	} {
		remaining := value
		for {
			index := strings.Index(remaining, marker)
			if index < 0 {
				break
			}
			remaining = remaining[index+len(marker):]
			if len(remaining) == 0 || remaining[0] != '/' {
				if len(remaining) == 0 || compactKbuildObjectTreeMarkerBoundary(remaining[0]) {
					b.observes = true
					b.all = true
				}
				continue
			}
			candidate := remaining[1:]
			end := 0
			dynamic := false
			for end < len(candidate) {
				character := candidate[end]
				if character == '$' || character == '`' {
					dynamic = true
					break
				}
				if (character >= 'a' && character <= 'z') ||
					(character >= 'A' && character <= 'Z') ||
					(character >= '0' && character <= '9') ||
					strings.ContainsRune("/._+-@", rune(character)) {
					end++
					continue
				}
				break
			}
			pathname := strings.TrimSuffix(candidate[:end], "/")
			canonical := canonicalKbuildRulePath(pathname)
			b.observes = true
			if dynamic || canonical == "" || canonical == "." || canonical != pathname || strings.ContainsAny(pathname, "$%") {
				b.all = true
				continue
			}
			b.references[canonical] = true
		}
	}
}

// compactKbuildObjectTreeObservableText removes argument words which merely
// configure compiler path handling. Prefix-map options transform path
// spellings; neither side of the mapping is opened or traversed. Compiler
// search-directory flags are resolved separately from immutable source
// #include directives, producing exact generated-header edges rather than an
// edge to every artifact below a search root. Forced include operands remain
// observable file reads here. Treating any of these directory/root spellings
// as a full filesystem observation would connect unrelated host artifacts to
// every compile action carrying ordinary Kbuild flags.
func compactKbuildObjectTreeObservableText(value string) string {
	return compactKbuildObjectTreeObservableTextDepth(value, 0)
}

const compactKbuildSerializedCommandDepthLimit = 16

func compactKbuildObjectTreeObservableTextDepth(value string, depth int) string {
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return value
	}
	masked := []byte(value)
	nestedCompilerCommands := []string{}
	maskToken := func(token compactKbuildRecipeToken) {
		for index := token.start; index < token.end; index++ {
			masked[index] = ' '
		}
	}
	// Kbuild's cmd_and_fixdep/cmd_and_savecmd family passes an escaped copy of
	// the compiler argv to bookkeeping tools.  The shell lexer correctly keeps
	// that copy as one argument, but inspecting its raw bytes would mistake a
	// passive `-I $(objtree)` spelling for a read of the entire object tree.
	//
	// Treat any quoted/escaped word which carries configured compiler
	// provenance as a nested command value.  This is based on the typed tool
	// token and shell structure, not on a wrapper or variable name.  Mask the
	// serialized outer word and retain the recursively sanitized logical argv,
	// so real positional inputs and forced includes remain observable.  At the
	// depth limit the raw word is left intact and observation stays conservative.
	if depth < compactKbuildSerializedCommandDepthLimit {
		for _, token := range tokens {
			if token.operator || !strings.ContainsAny(token.value, " \t\r\n") {
				continue
			}
			refs, refsErr := KbuildActionRoleRefs(token.value)
			if refsErr != nil || !slices.ContainsFunc(refs, func(ref KbuildActionRoleRef) bool {
				return ref.Role == "cc" || ref.Role == "cxx"
			}) {
				continue
			}
			maskToken(token)
			nestedValue := strings.ReplaceAll(token.value, compactKbuildLiteralDollarToken, "$")
			nestedCompilerCommands = append(
				nestedCompilerCommands,
				compactKbuildObjectTreeObservableTextDepth(nestedValue, depth+1),
			)
		}
	}
	for _, token := range tokens {
		if token.operator {
			continue
		}
		name, _, assignment := strings.Cut(token.value, "=")
		if !assignment || !strings.HasPrefix(name, "-") || !strings.HasSuffix(name, "prefix-map") {
			continue
		}
		maskToken(token)
	}
	maskCompilerSearchDirectories := func(command []compactKbuildRecipeToken) {
		if len(command) == 0 {
			return
		}
		fields := make([]string, 0, len(command))
		compiler := false
		for _, token := range command {
			fields = append(fields, token.value)
			refs, refsErr := KbuildActionRoleRefs(token.value)
			if refsErr != nil {
				return
			}
			compiler = compiler || slices.ContainsFunc(refs, func(ref KbuildActionRoleRef) bool {
				return ref.Role == "cc" || ref.Role == "cxx"
			})
		}
		if !compiler {
			return
		}
		for _, operand := range KbuildCompilerIncludeOperands(fields) {
			switch operand.Flag {
			case "-I", "-iquote", "-isystem", "-idirafter":
				maskToken(command[operand.ArgumentIndex])
			}
		}
	}
	command := []compactKbuildRecipeToken{}
	for _, token := range tokens {
		if token.operator {
			maskCompilerSearchDirectories(command)
			command = command[:0]
			continue
		}
		command = append(command, token)
	}
	maskCompilerSearchDirectories(command)
	if len(nestedCompilerCommands) == 0 {
		return string(masked)
	}
	return string(masked) + "\n" + strings.Join(nestedCompilerCommands, "\n")
}

func compactKbuildObjectTreeMarkerBoundary(character byte) bool {
	return strings.ContainsRune(" \t\r\n'\";,:=)]}", rune(character))
}

func (b compactKbuildObjectTreeObservationBuilder) result() CompactKbuildObjectTreeObservation {
	result := CompactKbuildObjectTreeObservation{
		ObservesObjectTree: b.observes,
		ObservesAll:        b.all,
	}
	if b.all {
		return result
	}
	for reference := range b.references {
		result.References = append(result.References, reference)
	}
	sort.Strings(result.References)
	return result
}
