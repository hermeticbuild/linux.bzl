package kconfig

import (
	"fmt"
	"maps"
	"path"
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

// ObserveCompactKbuildSelectedRecipeObjectTree applies the selected linker's
// output proof to one recipe command before inspecting its object-tree reads.
// A plain `ld` can be authenticated only from the same selected script and
// configured roles that establish its physical output; an arbitrary command
// with -o must continue to observe its rooted operands conservatively.
func ObserveCompactKbuildSelectedRecipeObjectTree(
	profile CompactKbuildProfile, target, script, command string,
) CompactKbuildObjectTreeObservation {
	commands, err := compactKbuildCompoundProgramCommands(script)
	if err != nil {
		return ObserveCompactKbuildObjectTree(command)
	}
	for _, candidate := range commands {
		if candidate.program != "ld" || candidate.sourceStart < 0 ||
			candidate.sourceEnd > len(script) || candidate.sourceEnd <= candidate.sourceStart ||
			!CompactKbuildProfileConfiguredToolWritesRootedObjectTargetInScript(
				profile, script, script[candidate.sourceStart:candidate.sourceEnd], target,
			) {
			continue
		}
		// Mask only the exact -o word in this one authenticated command.
		// Other occurrences of the output path, including a later cat, remain
		// observable reads. command may be the complete script or an isolated
		// segment returned by shell shape analysis.
		segment := script[candidate.sourceStart:candidate.sourceEnd]
		tokens, lexErr := lexCompactKbuildRecipe(segment)
		if lexErr != nil {
			return ObserveCompactKbuildObjectTree(command)
		}
		output := canonicalKbuildRulePath(target)
		for index := 0; index+1 < len(tokens); index++ {
			operand := compactKbuildMaterializeActionTreeMarkers(tokens[index+1].value)
			if tokens[index].operator || tokens[index].value != "-o" || tokens[index+1].operator ||
				operand != "__LINUX_BZL_OBJECT_TREE__/"+output && operand != "${tree:prep}/"+output {
				continue
			}
			start, end := candidate.sourceStart+tokens[index+1].start, candidate.sourceStart+tokens[index+1].end
			if start < 0 || end > len(script) {
				break
			}
			// Match the command's exact lexical occurrence; textual global
			// replacement could hide a later genuine read of this output.
			if command == script {
				masked := []byte(command)
				for offset := start; offset < end; offset++ {
					masked[offset] = ' '
				}
				return ObserveCompactKbuildObjectTree(string(masked))
			}
			if command == segment {
				masked := []byte(command)
				for offset := tokens[index+1].start; offset < tokens[index+1].end; offset++ {
					masked[offset] = ' '
				}
				return ObserveCompactKbuildObjectTree(string(masked))
			}
			break
		}
	}
	return ObserveCompactKbuildObjectTree(command)
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
		// mkcompile_h reads .version relative to Make's object-tree cwd when
		// KBUILD_BUILD_VERSION is unset or empty. Neither the script argv nor
		// exported path variables name that file; inspect the actual selected
		// source bytes before deciding whether to add its producer edge.
		if invocation.scriptPath == "scripts/mkcompile_h" {
			content, readErr := readCompactKbuildProfileSource(profile, invocation.scriptPath)
			if readErr != nil {
				return CompactKbuildObjectTreeObservation{}, fmt.Errorf("read selected %q: %w", invocation.scriptPath, readErr)
			}
			versionPath, readsVersion, readErr := compactKbuildMkcompileHVersionRead(
				profile, string(content), effectiveEnvironment["KBUILD_BUILD_VERSION"],
			)
			if readErr != nil {
				return CompactKbuildObjectTreeObservation{}, readErr
			}
			if readsVersion {
				observation.addValue("${tree:prep}/" + versionPath)
			}
		}
		usage := invocation.environmentUsage
		for _, word := range slices.Sorted(maps.Keys(usage.literalProgramHeads)) {
			if compactKbuildSourceScriptProgramUsesSourceRoot(word) {
				continue
			}
			programPath, immutableSource, resolved := compactKbuildProfileCommandPath(profile, word)
			if !resolved {
				return CompactKbuildObjectTreeObservation{}, fmt.Errorf("selected source program %q has no exact object-tree path", word)
			}
			if immutableSource {
				continue
			}
			observation.addValue("${tree:prep}/" + programPath)
		}
		if usage.ObservesAll || usage.observesProcessEnvironment {
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

// compactKbuildMkcompileHVersionRead authenticates the conditional object
// read from the immutable script selected by the current Make command. The
// five lines match the Linux 5.10 and 5.15 mkcompile_h version expression;
// other active .version syntax must be understood before it can be lowered.
func compactKbuildMkcompileHVersionRead(
	profile CompactKbuildProfile, content, buildVersion string,
) (string, bool, error) {
	expansions, _, err := prepareCompactKbuildSourceScript(content)
	if err != nil {
		return "", false, fmt.Errorf("inspect selected scripts/mkcompile_h: %w", err)
	}
	if !strings.Contains(expansions, ".version") {
		return "", false, nil
	}
	lines := strings.Split(expansions, "\n")
	block := []string{
		`if [ -z "$KBUILD_BUILD_VERSION" ]; then`,
		`VERSION=$(cat .version 2>/dev/null || echo 1)`,
		`else`,
		`VERSION=$KBUILD_BUILD_VERSION`,
		`fi`,
	}
	found := false
	for index := 0; index+len(block) <= len(lines); index++ {
		matches := true
		for offset, expected := range block {
			if strings.TrimSpace(lines[index+offset]) != expected {
				matches = false
				break
			}
		}
		if matches {
			found = true
			break
		}
	}
	if !found || strings.Count(expansions, ".version") != 1 {
		return "", false, fmt.Errorf("selected scripts/mkcompile_h has an unrecognized active .version read")
	}
	// Shell expansions in an exported version may produce an empty value at
	// execution time. Only a known nonempty literal proves this branch cannot
	// read the object file.
	if buildVersion != "" {
		tokens, lexErr := lexCompactKbuildRecipe(buildVersion)
		if lexErr == nil && len(tokens) == 1 && !tokens[0].shellExpansion && !tokens[0].pathnameExpansion {
			return "", false, nil
		}
	}
	location, located := CompactKbuildProfileInvocationLocation(profile)
	if located && location.Tree != CompactKbuildInvocationObjectTree {
		return "", false, fmt.Errorf("selected scripts/mkcompile_h reads .version outside an object-tree Make invocation: %q", location.Tree)
	}
	versionPath := ".version"
	if located && location.Directory != "" {
		versionPath = path.Join(location.Directory, versionPath)
	}
	return versionPath, true, nil
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
				// An active pathname glob can select other generated files below
				// the same prefix. Recording only the literal prefix would omit
				// source-visible producer edges from the working-tree closure.
				if strings.ContainsRune("$`*?[]", rune(character)) {
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
	// Kbuild also passes quoted compiler argv to bookkeeping tools. Inspect
	// those argv recursively only after passive display operands have been
	// masked: printf of a compiler command writes text, while a surviving
	// operand passed to a selected tool can still name actual file inputs.
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
	declaredRootedOutput := func(value string) bool {
		value = compactKbuildMaterializeActionTreeMarkers(value)
		rooted := strings.HasPrefix(value, "__LINUX_BZL_OBJECT_TREE__/")
		for _, prefix := range []string{"${tree:kernel}/", "${tree:prep}/", "${tree:host}/", "${tree:bootstrap}/", "${tree:prehost}/", "${work:root}/"} {
			rooted = rooted || strings.HasPrefix(value, prefix)
		}
		_, declared := compactKbuildRecipePath(value)
		return rooted && declared
	}
	passiveShellWord := func(token compactKbuildRecipeToken) bool {
		// A display argument can execute command substitution or glob before
		// echo/printf prints it. The shell lexer records only globs exposed
		// outside quotes and escapes; a quoted '*' is passive literal text.
		value := token.value
		for _, placeholder := range []string{"${tree:kernel}", "${tree:prep}", "${tree:host}", "${tree:bootstrap}", "${tree:prehost}", "${work:root}"} {
			value = strings.ReplaceAll(value, placeholder, "")
		}
		return !token.pathnameExpansion && !strings.ContainsAny(value, "$`")
	}
	maskDisplayOperands := func(command []compactKbuildRecipeToken) {
		if len(command) == 0 {
			return
		}
		program := command[0].value
		if applet, runtime := compactKbuildAbsoluteRuntimeApplet(program); runtime {
			program = applet
		}
		if program != "echo" && program != "printf" {
			return
		}
		for _, operand := range command[1:] {
			if !passiveShellWord(operand) {
				return
			}
		}
		for _, operand := range command[1:] {
			maskToken(operand)
		}
	}
	maskConfiguredToolNonReadOperands := func(command []compactKbuildRecipeToken) {
		if len(command) == 0 {
			return
		}
		program, configured := parseKbuildActionRoleToken(command[0].value)
		if !configured || program.Role != "cc" && program.Role != "cxx" && program.Role != "ld" {
			return
		}
		fields := make([]string, 0, len(command))
		for _, token := range command {
			fields = append(fields, token.value)
		}
		if program.Role == "cc" || program.Role == "cxx" {
			for _, operand := range KbuildCompilerIncludeOperands(fields) {
				switch operand.Flag {
				case "-I", "-iquote", "-isystem", "-idirafter":
					maskToken(command[operand.ArgumentIndex])
				}
			}
		}
		// A configured compiler or linker command's -o operand is a write.
		// Keep malformed, unrooted, and undeclared output words visible so
		// uncertain commands retain conservative object-tree observation.
		maskOutput := func(index int, value string) {
			if declaredRootedOutput(value) {
				maskToken(command[index])
			}
		}
		for index, argument := range fields {
			if argument == "--" {
				break
			}
			if program.Role == "cc" || program.Role == "cxx" {
				if index+1 < len(fields) && (argument == "-MT" || argument == "-MQ" || argument == "-MF") {
					maskOutput(index+1, fields[index+1])
					index++
					continue
				}
				for _, prefix := range []string{"-Wp,-MT,", "-Wp,-MQ,", "-Wp,-MD,", "-Wp,-MMD,", "-MT", "-MQ", "-MF"} {
					if payload, recognized := strings.CutPrefix(argument, prefix); recognized && payload != "" {
						maskOutput(index, payload)
						break
					}
				}
			}
			if argument == "-o" {
				if index+1 < len(fields) {
					maskOutput(index+1, fields[index+1])
					index++
				}
				continue
			}
			if strings.HasPrefix(argument, "-o") && len(argument) > len("-o") {
				maskOutput(index, strings.TrimPrefix(argument, "-o"))
			}
		}
	}
	command := []compactKbuildRecipeToken{}
	redirect := ""
	finishCommand := func(piped bool) {
		// A printed path can become an actual input when the next command
		// interprets stdin as filenames (for example, printf | xargs ar).
		// Keep that path visible unless the display ends this shell command.
		if !piped {
			maskDisplayOperands(command)
		}
		maskConfiguredToolNonReadOperands(command)
		command = command[:0]
	}
	for _, token := range tokens {
		if token.operator {
			switch token.value {
			case ">":
				redirect = ">"
			case ">>", "<":
				redirect = token.value
			case "|":
				finishCommand(true)
				redirect = ""
			case ";", "&&", "||", "&":
				finishCommand(false)
				redirect = ""
			default:
				redirect = ""
			}
			continue
		}
		if redirect != "" {
			// POSIX > opens its operand for writing. A typed, literal tree
			// destination is an output; >> and < can read existing bytes.
			if redirect == ">" && declaredRootedOutput(token.value) && passiveShellWord(token) {
				maskToken(token)
			}
			redirect = ""
			continue
		}
		command = append(command, token)
	}
	finishCommand(false)
	if depth < compactKbuildSerializedCommandDepthLimit {
		for _, token := range tokens {
			if token.operator || strings.TrimSpace(string(masked[token.start:token.end])) == "" ||
				!strings.ContainsAny(token.value, " \t\r\n") {
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
