package kconfig

// This file lowers opaque values created by selected Kbuild control effects.
// The query is an ordinary argv-only action whose stdout is a declared File;
// its consumer obtains that File through an exact node edge and requests the
// bounded GNU-Make-shell safe-word transform from mapdirectoryrecipe.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"regexp"
	"sort"
	"strings"
)

func normalizedKbuildDeferredContentQuery(query KbuildDeferredContentQuery) (KbuildDeferredContentQuery, error) {
	if query.Origin.Profile == "" {
		query.Origin.Profile = query.Profile.Name
	}
	if query.Origin.Target == "" {
		query.Origin.Target = query.Target
	}
	if _, err := canonicalKbuildDeferredContentEnvironment(query.Environment); err != nil {
		return KbuildDeferredContentQuery{}, fmt.Errorf("deferred Kbuild content query %q environment: %w", query.Token, err)
	}
	query.Environment = maps.Clone(query.Environment)
	refs, err := KbuildActionRoleRefs(query.Command)
	if err != nil {
		return KbuildDeferredContentQuery{}, fmt.Errorf("deferred Kbuild content query %q action-role provenance: %w", query.Token, err)
	}
	observedValues := []string{query.Command}
	if query.Environment != nil {
		usage, usageErr := compactKbuildDeferredContentQueryEnvironmentUsage(query)
		if usageErr != nil {
			return KbuildDeferredContentQuery{}, fmt.Errorf("deferred Kbuild content query %q environment usage: %w", query.Token, usageErr)
		}
		environment, environmentRefs, environmentErr := compactKbuildProjectedCapturedEnvironment(
			query.Profile, query.Environment, nil, usage,
		)
		if environmentErr != nil {
			return KbuildDeferredContentQuery{}, fmt.Errorf("deferred Kbuild content query %q environment projection: %w", query.Token, environmentErr)
		}
		refs = append(refs, environmentRefs...)
		for _, name := range sortedStringMapKeys(environment) {
			// Keep the complete projected environment on the action, but expose
			// only values the command or an immutable helper actually reads to
			// object-tree dependency discovery. Treating every exported value as
			// observed turns an unused objtree export into a dependency on every
			// visible generated artifact.
			if usage.uses(name) {
				observedValues = append(observedValues, environment[name])
			}
		}
	}
	query.ActionRoles = canonicalKbuildActionRoleRefs(refs)
	query.ObjectTree = observeKbuildDeferredContentQueryObjectTree(
		query.Profile, query.Target, query.Command, observedValues[1:]...,
	)
	return query, nil
}

// observeKbuildDeferredContentQueryObjectTree augments the ordinary typed-root
// observation with exact invocation-relative operands. Deferred queries are
// selected before final action lowering, so their source-authored ../ paths
// must be projected through the same typed Make cwd as the eventual recipe.
// Only complete, clearly path-like argv fields are accepted: bare words and
// compiler search-directory configuration remain outside artifact selection.
func observeKbuildDeferredContentQueryObjectTree(
	profile CompactKbuildProfile,
	target, command string,
	environmentValues ...string,
) CompactKbuildObjectTreeObservation {
	builder := compactKbuildObjectTreeObservationBuilder{references: map[string]bool{}}
	builder.addValue(command)
	for _, value := range environmentValues {
		builder.addValue(value)
	}

	location, ok := CompactKbuildProfileInvocationLocation(profile)
	if !ok || location.Tree != CompactKbuildInvocationObjectTree {
		return builder.result()
	}
	addOperand := func(value string, allowOptionValue bool) {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, " \t\r\n$`\x00") {
			return
		}
		candidate := value
		if index := strings.IndexByte(value, '='); index >= 0 {
			// Shell assignments and linker-style section=noload operands are
			// not filesystem reads. Only an explicit option value is eligible,
			// and it must itself contain a path separator.
			if !allowOptionValue || !strings.HasPrefix(value, "-") {
				return
			}
			candidate = value[index+1:]
			if !strings.Contains(candidate, "/") {
				return
			}
		} else if strings.HasPrefix(value, "-") {
			return
		}
		if candidate == "" || candidate == "." || candidate == ".." ||
			strings.ContainsAny(candidate, "=,:()[]{}") ||
			(!strings.Contains(candidate, "/") && !strings.HasPrefix(candidate, ".")) {
			return
		}
		// A symbolic probe result is not a static filesystem operand. In
		// particular, do not scope an embedded token to an external invocation
		// directory and accidentally turn it into a generated-file target.
		if linuxProbeSymbolPattern.MatchString(candidate) {
			return
		}
		resolved, source, pathLike := compactKbuildProfileCommandPath(profile, candidate)
		if !pathLike || source || resolved == "" || resolved == "." {
			return
		}
		builder.observes = true
		builder.references[resolved] = true
	}

	observable := compactKbuildObjectTreeObservableText(
		compactKbuildProfileCanonicalRecipeText(profile, command),
	)
	commands, err := parseCompactKbuildRecipe(
		observable, compactKbuildAutomaticContext{target: target},
	)
	if err != nil {
		commands, _ = compactKbuildCompoundProgramCommands(observable)
	}
	for _, parsed := range commands {
		addOperand(parsed.program, false)
		for _, argument := range parsed.arguments {
			addOperand(argument, true)
		}
		addOperand(parsed.stdin, false)
		for _, value := range parsed.environment {
			addOperand(value, false)
		}
	}
	for _, value := range environmentValues {
		addOperand(value, false)
	}
	return builder.result()
}

// compactKbuildDeferredContentPreconfiguredInputs closes exact immutable SDK
// leaves referenced by a deferred query. Generated object-tree files are
// selected earlier with producer provenance, but a preconfigured kernel tree
// exposes its prepared files as source namespaces rather than Kbuild actions.
// Keep those two ownership models distinct while staging both at their common
// logical paths below the query's private working root.
func (b *compactKbuildRulePlanBuilder) compactKbuildDeferredContentPreconfiguredInputs(
	profile CompactKbuildProfile,
	observation CompactKbuildObjectTreeObservation,
	inputs []compactKbuildRuleInput,
) ([]compactKbuildRuleInput, error) {
	if b == nil || b.metadata == nil || !b.metadata.preconfiguredObjectTree ||
		!observation.ObservesObjectTree {
		return inputs, nil
	}
	if err := b.metadata.ensureActionPlanSourceNamespaceIndex(); err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		existing[canonicalKbuildRulePath(input.path)] = true
	}
	matches := func(candidate string) bool {
		if observation.ObservesAll {
			return true
		}
		for _, reference := range observation.References {
			reference = canonicalKbuildRulePath(reference)
			if reference != "" && (candidate == reference || strings.HasPrefix(candidate, strings.TrimSuffix(reference, "/")+"/")) {
				return true
			}
		}
		return false
	}
	for _, candidate := range sortedStringMapKeys(b.metadata.exactSourcePaths) {
		candidate = canonicalKbuildRulePath(candidate)
		if candidate == "" || existing[candidate] || !matches(candidate) {
			continue
		}
		exists, err := b.metadata.preconfiguredObjectTreeSourcePathExists(profile, candidate)
		if err != nil {
			return nil, fmt.Errorf("deferred Kbuild content object-tree input %q: %w", candidate, err)
		}
		if !exists {
			continue
		}
		sourceID, err := b.metadata.ensureActionPlanSource(b.plan, candidate)
		if err != nil {
			return nil, fmt.Errorf("deferred Kbuild content object-tree input %q: %w", candidate, err)
		}
		inputs = append(inputs, compactKbuildRuleInput{
			path: candidate, sourceID: sourceID, objectTree: true, workingOnly: true,
		})
		existing[candidate] = true
	}
	return inputs, nil
}

// compactKbuildDeferredContentQueryEnvironmentUsage derives one usage view for
// both graph selection and lowering. Besides shell parameters in Command it
// reads immutable helper scripts named in argv, which is required when an
// unexported CONFIG_SHELL hides the helper relationship from command
// classification.
func compactKbuildDeferredContentQueryEnvironmentUsage(
	query KbuildDeferredContentQuery,
) (compactKbuildSourceScriptEnvironmentUsage, error) {
	command := compactKbuildProfileCanonicalRecipeText(query.Profile, query.Command)
	commands, err := parseCompactKbuildRecipe(
		command, compactKbuildAutomaticContext{target: query.Target},
	)
	if err != nil {
		commands, _ = compactKbuildCompoundProgramCommands(command)
	}
	return compactKbuildHermeticScriptEnvironmentUsage(query.Profile, command, commands)
}

func compactKbuildDeferredContentQueryClassificationValues(
	query KbuildDeferredContentQuery,
) (map[string]string, error) {
	if query.Environment == nil && query.CommandShell == "" {
		return nil, nil
	}
	values, err := compactKbuildResolvedCapturedEnvironment(query.Profile, query.Environment)
	if err != nil {
		return nil, err
	}
	if query.CommandShell != "" {
		evaluator, evaluatorErr := compactKbuildProfileTargetEvaluator(query.Profile, "")
		if evaluatorErr != nil {
			return nil, evaluatorErr
		}
		commandShell, resolveErr := evaluator.template.resolveKbuildSymbolic(query.CommandShell)
		if resolveErr != nil {
			return nil, fmt.Errorf("captured Kbuild CONFIG_SHELL symbolic result: %w", resolveErr)
		}
		values["CONFIG_SHELL"] = compactKbuildProfileCanonicalRecipeText(query.Profile, commandShell)
	}
	return values, nil
}

func sameKbuildDeferredContentQuerySource(left, right KbuildDeferredContentQuery) bool {
	return left.Token == right.Token && left.Command == right.Command &&
		left.Target == right.Target && left.Profile.Name == right.Profile.Name &&
		left.Transform == right.Transform && left.Generation == right.Generation &&
		left.CommandShell == right.CommandShell &&
		(left.Environment == nil) == (right.Environment == nil) &&
		maps.Equal(left.Environment, right.Environment) && left.Origin == right.Origin
}

// ApplyKbuildDeferredContentSelections attaches the independently solved query
// actions to every profile registry that can expose their tokens. Registries
// are intentionally process-local and shared by profile copies, so the
// selected action graph remains out of serialized Make metadata.
func ApplyKbuildDeferredContentSelections(
	profiles []CompactKbuildProfile,
	selections map[string]KbuildDeferredContentSelection,
) error {
	found := map[string]bool{}
	for profileIndex := range profiles {
		registry := profiles[profileIndex].deferredContentQueries
		for token, query := range registry {
			selection, ok := selections[token]
			if !ok {
				continue
			}
			normalized, err := normalizedKbuildDeferredContentQuery(query)
			if err != nil {
				return err
			}
			if selection.Token == "" {
				selection.Token = token
			}
			if selection.Profile == "" {
				selection.Profile = normalized.Origin.Profile
			}
			if selection.Target == "" {
				selection.Target = normalized.Origin.Target
			}
			if selection.Token != token || selection.Profile != normalized.Origin.Profile || selection.Target != normalized.Origin.Target {
				return fmt.Errorf(
					"deferred Kbuild content query %q selection origin %s:%s differs from source origin %s:%s",
					token, selection.Profile, selection.Target, normalized.Origin.Profile, normalized.Origin.Target,
				)
			}
			normalized.Selection = selection
			registry[token] = normalized
			found[token] = true
		}
	}
	for token := range selections {
		if !found[token] {
			return fmt.Errorf("selected deferred Kbuild content query %q has no profile registry", token)
		}
	}
	return nil
}

// KbuildDeferredContentSelections returns the canonical selected query-action
// records attached to profile registries after graph selection.
func KbuildDeferredContentSelections(profiles []CompactKbuildProfile) ([]KbuildDeferredContentSelection, error) {
	byToken := map[string]KbuildDeferredContentSelection{}
	for _, profile := range profiles {
		for token, query := range profile.deferredContentQueries {
			if query.Selection.Token == "" {
				continue
			}
			if query.Selection.Token != token {
				return nil, fmt.Errorf("deferred Kbuild content query registry key %q has selection token %q", token, query.Selection.Token)
			}
			if previous, ok := byToken[token]; ok && previous != query.Selection {
				return nil, fmt.Errorf("deferred Kbuild content query %q has conflicting selections", token)
			}
			byToken[token] = query.Selection
		}
	}
	tokens := make([]string, 0, len(byToken))
	for token := range byToken {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	result := make([]KbuildDeferredContentSelection, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, byToken[token])
	}
	return result, nil
}

func compactKbuildDeferredContentSelectionArtifacts(
	selection KbuildDeferredContentSelection,
) ([]CompactKbuildVisibleArtifact, []CompactKbuildVisibleArtifact, error) {
	view := CompactKbuildSelection{
		UsesInitialObjectTree:        selection.UsesInitialObjectTree,
		InitialObjectTreeArtifacts:   selection.InitialObjectTreeArtifacts,
		GeneratedObjectTreeArtifacts: selection.GeneratedObjectTreeArtifacts,
	}
	initial, err := compactKbuildSelectionInitialObjectTreeArtifacts(view)
	if err != nil {
		return nil, nil, err
	}
	generated, err := compactKbuildSelectionGeneratedObjectTreeArtifacts(view)
	if err != nil {
		return nil, nil, err
	}
	return initial, generated, nil
}

func kbuildDeferredContentSelectionDigest(
	query KbuildDeferredContentQuery,
	selection KbuildDeferredContentSelection,
) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		query.Token,
		selection.Lifecycle,
		selection.Scope,
		selection.Stage,
		selection.InitialObjectTreeArtifacts,
		selection.GeneratedObjectTreeArtifacts,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

var kbuildDeferredContentTokenPattern = regexp.MustCompile(`LINUX_BZL_KBUILD_CONTENT_[0-9a-f]{64}`)

const kbuildDeferredLiteralPrefix = "__LINUX_BZL_DEFERRED_LITERAL_"

var kbuildDeferredLiteralTokens = map[byte]string{
	'$':  kbuildDeferredLiteralPrefix + "DOLLAR__",
	'`':  kbuildDeferredLiteralPrefix + "BACKTICK__",
	'|':  kbuildDeferredLiteralPrefix + "PIPE__",
	'&':  kbuildDeferredLiteralPrefix + "AMPERSAND__",
	';':  kbuildDeferredLiteralPrefix + "SEMICOLON__",
	'<':  kbuildDeferredLiteralPrefix + "LESS__",
	'>':  kbuildDeferredLiteralPrefix + "GREATER__",
	'\n': kbuildDeferredLiteralPrefix + "NEWLINE__",
	'\r': kbuildDeferredLiteralPrefix + "CARRIAGE_RETURN__",
}

func compactKbuildHasDeferredShellSingleWord(profile CompactKbuildProfile, value string) bool {
	for _, token := range kbuildDeferredContentTokenPattern.FindAllString(value, -1) {
		if query, ok := profile.deferredContentQueries[token]; ok &&
			query.Transform == ActionRecipeContentTransformMakeShellSingleWord {
			return true
		}
	}
	return false
}

func validateKbuildDeferredShellSingleWordPlacements(
	profile CompactKbuildProfile,
	value string,
) error {
	seen := map[string]bool{}
	for _, token := range kbuildDeferredContentTokenPattern.FindAllString(value, -1) {
		if seen[token] {
			continue
		}
		seen[token] = true
		query, ok := profile.deferredContentQueries[token]
		if !ok || query.Transform != ActionRecipeContentTransformMakeShellSingleWord {
			continue
		}
		if _, err := kbuildDeferredShellSingleWordPlacements(value, token); err != nil {
			return err
		}
	}
	return nil
}

type kbuildDeferredShellSingleWordPlacement struct {
	start     int
	end       int
	transform string
}

// validateActionRecipeContentTemplate proves that every generated-content
// insertion in one serialized shell template is compatible with both its
// byte-to-text transform and its exact source-shell placement. The argument
// transform intentionally leaves tree bindings for scriptrun, so this proof is
// part of the recipe boundary rather than planner-only metadata.
func validateActionRecipeContentTemplate(
	value string,
	substitutions map[string]ActionRecipeContentSubstitution,
) error {
	matches := actionRecipePlaceholder.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return nil
	}

	// Mask typed placeholders with same-length static bytes before asking the
	// shell parser for word and program extents. This preserves every source
	// offset and quote boundary without letting a typed binding look like a
	// dynamic shell expansion to the command parser.
	masked := []byte(value)
	for _, match := range matches {
		for index := match[0]; index < match[1]; index++ {
			masked[index] = 'x'
		}
	}
	tokens, err := lexCompactKbuildRecipe(string(masked))
	if err != nil {
		return fmt.Errorf("lex content-template shell argument: %w", err)
	}
	programs, err := compactKbuildCompoundProgramCommands(string(masked))
	if err != nil {
		return fmt.Errorf("discover content-template shell programs: %w", err)
	}

	for _, match := range matches {
		kind := value[match[2]:match[3]]
		if kind != "content" {
			continue
		}
		name := value[match[4]:match[5]]
		substitution, ok := substitutions[name]
		if !ok {
			// The ordinary placeholder validator reports the undeclared binding
			// with its established error. Do not manufacture a second contract.
			continue
		}

		lexicalIndex := -1
		for index, lexical := range tokens {
			if lexical.start <= match[0] && match[1] <= lexical.end {
				lexicalIndex = index
				break
			}
		}
		if lexicalIndex < 0 || tokens[lexicalIndex].operator {
			return fmt.Errorf("content-template substitution %q has no non-operator shell word", name)
		}
		lexical := tokens[lexicalIndex]
		quote, unescaped := kbuildDeferredContentTokenQuote(value, lexical, match[:2])
		if !unescaped {
			return fmt.Errorf("content-template substitution %q is escaped in its shell word", name)
		}
		if err := validateKbuildDeferredContentWordAffixes(value[lexical.start:lexical.end]); err != nil {
			return fmt.Errorf("content-template substitution %q shell word: %w", name, err)
		}
		if compactKbuildTokenIsRedirectionOperand(tokens, lexicalIndex) {
			return fmt.Errorf("content-template substitution %q is a redirection operand", name)
		}
		for _, program := range programs {
			if program.programStart < match[1] && match[0] < program.programEnd {
				return fmt.Errorf("content-template substitution %q selects a command program", name)
			}
		}

		switch substitution.Transform {
		case ActionRecipeContentTransformMakeShellWord:
			// The runtime value is restricted to the portable shell-safe byte
			// alphabet, so it remains data in any statically proven word quote.
		case ActionRecipeContentTransformMakeShellSingleWord:
			if quote != 0 {
				return fmt.Errorf("content-template substitution %q single-word literal is not unquoted", name)
			}
		case ActionRecipeContentTransformMakeShellSingleQuotedSegment:
			if quote != '\'' {
				return fmt.Errorf("content-template substitution %q single-quoted segment is not inside a source single quote", name)
			}
		case ActionRecipeContentTransformMakeShellValue:
			return fmt.Errorf("content-template substitution %q uses environment-only transform %q", name, substitution.Transform)
		default:
			// The content-substitution validator reports unsupported transforms.
			continue
		}
	}
	return nil
}

func compactKbuildTokenIsRedirectionOperand(tokens []compactKbuildRecipeToken, lexicalIndex int) bool {
	if lexicalIndex <= 0 || lexicalIndex >= len(tokens) {
		return false
	}
	previous := tokens[lexicalIndex-1]
	if previous.operator && (previous.value == "<" || strings.HasPrefix(previous.value, ">")) {
		return true
	}
	// The lexer represents descriptor duplication as adjacent '<'/'>' and '&'
	// operators. Recognize its operand without treating a command connector '&'
	// followed by a new word as a redirection.
	if lexicalIndex >= 2 && previous.operator && previous.value == "&" {
		redirection := tokens[lexicalIndex-2]
		return redirection.operator && (redirection.value == "<" || redirection.value == ">") &&
			redirection.end == previous.start && previous.end == tokens[lexicalIndex].start
	}
	return false
}

// kbuildDeferredShellSingleWordPlacements proves every occurrence of one
// deferred $(shell ...) result occupies data in a statically selected command
// argument. An unquoted occurrence receives a complete single-word literal.
// The same source query may also be repeated inside Kbuild's single-quoted
// saved-command payload; that occurrence receives only a quote-internal
// segment and is deliberately restricted to values for which make-cmd's
// dollar, pound, and single-quote rewrites are the identity.
func kbuildDeferredShellSingleWordPlacements(
	value, selectedToken string,
) ([]kbuildDeferredShellSingleWordPlacement, error) {
	tokenIndexes := kbuildDeferredContentTokenPattern.FindAllStringIndex(value, -1)
	if len(tokenIndexes) == 0 {
		return nil, nil
	}
	tokens, err := lexCompactKbuildRecipe(value)
	if err != nil {
		return nil, fmt.Errorf("lex deferred-content script: %w", err)
	}
	programs, err := compactKbuildCompoundProgramCommands(value)
	if err != nil {
		return nil, fmt.Errorf("discover deferred-content script programs: %w", err)
	}
	placements := []kbuildDeferredShellSingleWordPlacement{}
	for _, tokenIndex := range tokenIndexes {
		token := value[tokenIndex[0]:tokenIndex[1]]
		if token != selectedToken {
			continue
		}
		lexicalIndex := -1
		for index, lexical := range tokens {
			if lexical.start <= tokenIndex[0] && tokenIndex[1] <= lexical.end {
				lexicalIndex = index
				break
			}
		}
		if lexicalIndex < 0 {
			return nil, fmt.Errorf("deferred Kbuild content token %q has no shell word", token)
		}
		lexical := tokens[lexicalIndex]
		if lexical.operator {
			return nil, fmt.Errorf("deferred Kbuild content token %q is a shell operator", token)
		}
		quote, unescaped := kbuildDeferredContentTokenQuote(value, lexical, tokenIndex)
		transform := ActionRecipeContentTransformMakeShellSingleWord
		switch {
		case !unescaped:
			return nil, fmt.Errorf("deferred Kbuild content token %q is escaped in its argument word", token)
		case quote == 0:
		case quote == '\'':
			transform = ActionRecipeContentTransformMakeShellSingleQuotedSegment
		default:
			return nil, fmt.Errorf("deferred Kbuild content token %q is inside a double-quoted argument word", token)
		}
		if err := validateKbuildDeferredContentWordAffixes(value[lexical.start:lexical.end]); err != nil {
			return nil, fmt.Errorf("deferred Kbuild content token %q argument word: %w", token, err)
		}
		if compactKbuildTokenIsRedirectionOperand(tokens, lexicalIndex) {
			return nil, fmt.Errorf("deferred Kbuild content token %q is a redirection operand", token)
		}
		for _, program := range programs {
			if program.programStart < tokenIndex[1] && tokenIndex[0] < program.programEnd {
				return nil, fmt.Errorf("deferred Kbuild content token %q selects a command program", token)
			}
		}
		placements = append(placements, kbuildDeferredShellSingleWordPlacement{
			start: tokenIndex[0], end: tokenIndex[1], transform: transform,
		})
	}
	return placements, nil
}

// kbuildDeferredContentTokenQuote reports the shell quote which is active at
// one token occurrence. GNU Make commonly embeds $(shell ...) output in an
// otherwise static option, for example ARM's
//
//	--defsym _kernel_bss_size=$(KBSS_SZ)
//
// The bool is false when the occurrence itself is escaped, because neither a
// complete quoted word nor a quote-internal segment can be substituted at that
// byte boundary without changing the source-authored escape.
func kbuildDeferredContentTokenQuote(
	value string,
	lexical compactKbuildRecipeToken,
	tokenIndex []int,
) (byte, bool) {
	if len(tokenIndex) != 2 || lexical.start < 0 || lexical.end > len(value) ||
		tokenIndex[0] < lexical.start || tokenIndex[1] > lexical.end || tokenIndex[0] >= tokenIndex[1] {
		return 0, false
	}
	quote := byte(0)
	for index := lexical.start; index < tokenIndex[0]; index++ {
		character := value[index]
		if quote != 0 {
			switch {
			case character == quote:
				quote = 0
			case character == '\\' && quote == '"':
				if index+1 >= tokenIndex[0] {
					return 0, false
				}
				index++
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '\\':
			if index+1 >= tokenIndex[0] {
				return 0, false
			}
			index++
		}
	}
	return quote, true
}

// validateKbuildDeferredContentWordAffixes keeps compound placement bounded to
// source-static shell text. Content bytes remain one quoted literal segment;
// the surrounding source may concatenate ordinary literals and typed action
// bindings, but cannot turn the inserted quote into part of a shell expansion,
// glob, placeholder, or other runtime-selected word.
func validateKbuildDeferredContentWordAffixes(word string) error {
	for _, bindingIndex := range actionRecipePlaceholder.FindAllStringIndex(word, -1) {
		if kbuildDeferredContentTokenPattern.MatchString(word[bindingIndex[0]:bindingIndex[1]]) {
			return fmt.Errorf("compound placement overlaps a typed action binding")
		}
	}
	protected := kbuildDeferredContentTokenPattern.ReplaceAllString(word, "linux_bzl_deferred_content")
	protected = actionRecipePlaceholder.ReplaceAllString(protected, "linux_bzl_action_binding")
	tokens, err := lexCompactKbuildRecipe(protected)
	if err != nil {
		return err
	}
	if len(tokens) != 1 || tokens[0].operator {
		return fmt.Errorf("compound placement does not retain one argument word")
	}
	if err := validateActionRecipeStaticShellWord(protected); err != nil {
		return fmt.Errorf("compound placement has non-static affixes: %w", err)
	}
	return nil
}

// compactKbuildDeferredContentQueries returns the exact source-time query
// snapshots referenced by one evaluated value.  Graph discovery consumes the
// same snapshots as action lowering so object-tree operands hidden behind an
// opaque content token cannot disappear until execution time.
func compactKbuildDeferredContentQueries(
	profile CompactKbuildProfile,
	value string,
) ([]KbuildDeferredContentQuery, error) {
	queries := []KbuildDeferredContentQuery{}
	seen := map[string]bool{}
	for _, token := range kbuildDeferredContentTokenPattern.FindAllString(value, -1) {
		if seen[token] {
			continue
		}
		seen[token] = true
		query, ok := profile.deferredContentQueries[token]
		if !ok {
			return nil, fmt.Errorf("evaluated Kbuild value references unknown deferred-content token %q", token)
		}
		normalized, err := normalizedKbuildDeferredContentQuery(query)
		if err != nil {
			return nil, err
		}
		queries = append(queries, normalized)
	}
	sort.Slice(queries, func(i, j int) bool { return queries[i].Token < queries[j].Token })
	return queries, nil
}

func canonicalKbuildDeferredContentQueries(queries []KbuildDeferredContentQuery) []KbuildDeferredContentQuery {
	byToken := map[string]KbuildDeferredContentQuery{}
	for _, query := range queries {
		normalized, err := normalizedKbuildDeferredContentQuery(query)
		if err != nil {
			continue
		}
		query = normalized
		if previous, ok := byToken[query.Token]; ok {
			if !sameKbuildDeferredContentQuerySource(previous, query) {
				// Token construction is content addressed, so a conflict indicates a
				// broken process-local profile registry. Retain both entries and let
				// the normal metadata validation report the collision with context.
				continue
			}
		}
		byToken[query.Token] = query
	}
	tokens := make([]string, 0, len(byToken))
	for token := range byToken {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	result := make([]KbuildDeferredContentQuery, 0, len(tokens))
	for _, token := range tokens {
		result = append(result, byToken[token])
	}
	return result
}

// KbuildDeferredContentObjectTreeReferences extracts canonical filesystem
// operands rooted in the selected invocation's writable object tree.  The
// query remains opaque executable text; only complete typed root markers carry
// dependency meaning here.
func KbuildDeferredContentObjectTreeReferences(command string) []string {
	return ObserveCompactKbuildObjectTree(command).References
}

func bindActionRecipeDeferredKbuildContent(plan *ActionPlan, node *ActionPlanNode, recipe *ActionRecipe) error {
	if plan == nil || node == nil || recipe == nil {
		return fmt.Errorf("cannot bind deferred Kbuild content to a nil plan, node, or recipe")
	}
	tokens := []string{}
	seen := map[string]bool{}
	collect := func(value string) {
		for _, token := range kbuildDeferredContentTokenPattern.FindAllString(value, -1) {
			if !seen[token] {
				seen[token] = true
				tokens = append(tokens, token)
			}
		}
	}
	for _, argument := range recipe.Arguments {
		collect(argument)
	}
	for _, name := range sortedStringMapKeys(recipe.Environment) {
		collect(recipe.Environment[name])
	}
	if len(tokens) == 0 {
		return nil
	}
	if plan.metadata == nil {
		return fmt.Errorf("recipe contains deferred Kbuild content but its plan has no source-derived metadata")
	}
	if recipe.ContentSubstitutions == nil {
		recipe.ContentSubstitutions = map[string]ActionRecipeContentSubstitution{}
	}
	builder := newCompactKbuildRulePlanBuilder(plan.metadata, plan)
	for _, token := range tokens {
		if plan.deferredContentBuilding[token] {
			for _, argument := range recipe.Arguments {
				if strings.Contains(argument, token) {
					return fmt.Errorf("deferred Kbuild content query %q recursively consumes its own result in argv", token)
				}
			}
			// GNU Make cannot export the textual result of a $(shell ...) while
			// that same expansion is still producing it. Target-environment replay
			// can rediscover the opaque token through a role-free exported value;
			// omit that not-yet-defined entry from the query process instead of
			// manufacturing a recursive action edge.
			for name, value := range recipe.Environment {
				if strings.Contains(value, token) {
					delete(recipe.Environment, name)
				}
			}
			continue
		}
		query, ok, err := plan.metadata.deferredKbuildContentQuery(token)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("recipe references unknown deferred Kbuild content token %q", token)
		}
		selection, ok := plan.metadata.deferredKbuildContentSelection(token)
		if !ok {
			return fmt.Errorf("recipe references deferred Kbuild content token %q without a selected query action", token)
		}
		producer, err := builder.buildDeferredKbuildContentQuery(query, selection)
		if err != nil {
			return fmt.Errorf("lower deferred Kbuild content query for %s: %w", token, err)
		}
		inputKey := fmt.Sprintf("content:%08d", len(node.Inputs))
		node.Inputs = append(node.Inputs, ActionPlanNodeEdge{Role: "content", ProducerID: producer, Slot: 0})
		recipe.Inputs = append(recipe.Inputs, inputKey)
		bind := func(transform string) string {
			contentName := fmt.Sprintf("deferred:%08d", len(recipe.ContentSubstitutions))
			recipe.ContentSubstitutions[contentName] = ActionRecipeContentSubstitution{
				Input: "input:" + inputKey, Transform: transform,
			}
			return "${content:" + contentName + "}"
		}
		argumentTransform := query.Transform
		if argumentTransform == "" {
			argumentTransform = ActionRecipeContentTransformMakeShellWord
		}
		argumentPlaceholders := map[string]string{}
		placeholderForTransform := func(transform string) string {
			if placeholder := argumentPlaceholders[transform]; placeholder != "" {
				return placeholder
			}
			placeholder := bind(transform)
			argumentPlaceholders[transform] = placeholder
			return placeholder
		}
		for index := range recipe.Arguments {
			argument := recipe.Arguments[index]
			if !strings.Contains(argument, token) {
				continue
			}
			if argumentTransform != ActionRecipeContentTransformMakeShellSingleWord {
				recipe.Arguments[index] = strings.ReplaceAll(
					argument, token, placeholderForTransform(argumentTransform),
				)
				continue
			}
			placements, placementErr := kbuildDeferredShellSingleWordPlacements(argument, token)
			if placementErr != nil {
				return placementErr
			}
			if len(placements) == 0 {
				return fmt.Errorf("deferred Kbuild content token %q has no argument placement", token)
			}
			var rewritten strings.Builder
			start := 0
			for _, placement := range placements {
				rewritten.WriteString(argument[start:placement.start])
				rewritten.WriteString(placeholderForTransform(placement.transform))
				start = placement.end
			}
			rewritten.WriteString(argument[start:])
			recipe.Arguments[index] = rewritten.String()
		}
		environmentPlaceholder := ""
		for name, value := range recipe.Environment {
			if !strings.Contains(value, token) {
				continue
			}
			if environmentPlaceholder == "" {
				environmentPlaceholder = bind(ActionRecipeContentTransformMakeShellValue)
			}
			recipe.Environment[name] = strings.ReplaceAll(value, token, environmentPlaceholder)
		}
	}
	return nil
}

func (m *CompactMetadata) deferredKbuildContentQuery(token string) (KbuildDeferredContentQuery, bool, error) {
	if m == nil {
		return KbuildDeferredContentQuery{}, false, nil
	}
	var selected KbuildDeferredContentQuery
	found := false
	for _, profile := range m.Config.KbuildProfiles {
		query, ok := profile.deferredContentQueries[token]
		if !ok {
			continue
		}
		query, err := normalizedKbuildDeferredContentQuery(query)
		if err != nil {
			return KbuildDeferredContentQuery{}, false, err
		}
		if found && (!sameKbuildDeferredContentQuerySource(selected, query) || selected.Selection != query.Selection) {
			return KbuildDeferredContentQuery{}, false, fmt.Errorf("deferred Kbuild content token %q has conflicting source queries", token)
		}
		selected, found = query, true
	}
	return selected, found, nil
}

func (m *CompactMetadata) deferredKbuildContentSelection(token string) (KbuildDeferredContentSelection, bool) {
	if m == nil {
		return KbuildDeferredContentSelection{}, false
	}
	for _, selection := range m.Config.KbuildDeferredContentSelections {
		if selection.Token == token {
			return selection, true
		}
	}
	return KbuildDeferredContentSelection{}, false
}

func (b *compactKbuildRulePlanBuilder) buildDeferredKbuildContentQuery(
	query KbuildDeferredContentQuery,
	selection KbuildDeferredContentSelection,
) (string, error) {
	digest := strings.TrimPrefix(query.Token, kbuildDeferredContentTokenPrefix)
	if len(digest) != 64 || kbuildDeferredContentTokenPattern.FindString(query.Token) != query.Token {
		return "", fmt.Errorf("invalid deferred Kbuild content token %q", query.Token)
	}
	outputTree, ok := actionPlanStageScratchTree(selection.Stage)
	if !ok {
		return "", fmt.Errorf("deferred Kbuild content has unsupported selected stage %q", selection.Stage)
	}
	// Query placement and exact artifact provenance are part of the hidden
	// output identity. Consumers and product façades can therefore share the one
	// source-time effect without first-consumer/product reuse changing its inputs.
	output := path.Join(".linux-bzl-content", outputTree, kbuildDeferredContentSelectionDigest(query, selection))
	if producer, _, ok := planProducerByOutput(b.plan, outputTree, output); ok {
		return producer, nil
	}
	if b.plan.deferredContentBuilding == nil {
		b.plan.deferredContentBuilding = map[string]bool{}
	}
	if b.plan.deferredContentBuilding[query.Token] {
		return "", fmt.Errorf("deferred Kbuild content query %q recursively consumes its own result", query.Token)
	}
	b.plan.deferredContentBuilding[query.Token] = true
	defer delete(b.plan.deferredContentBuilding, query.Token)
	initialArtifacts, generatedArtifacts, err := compactKbuildDeferredContentSelectionArtifacts(selection)
	if err != nil {
		return "", fmt.Errorf("decode deferred Kbuild content selection %q: %w", query.Token, err)
	}
	queryBuilder := b.forProfile(query.Profile).forOutput(selection.Stage, outputTree, "sdk")
	queryBuilder = queryBuilder.
		withInitialObjectTree(selection.UsesInitialObjectTree, initialArtifacts...).
		withGeneratedObjectTreeArtifacts(generatedArtifacts...)
	inputs, err := queryBuilder.compactKbuildWorkingTreeClosureInputs(query.Target, query.Profile, nil)
	if err != nil {
		return "", fmt.Errorf("deferred Kbuild content query %q object-tree closure: %w", query.Token, err)
	}
	inputs, err = queryBuilder.compactKbuildDeferredContentPreconfiguredInputs(
		query.Profile, query.ObjectTree, inputs,
	)
	if err != nil {
		return "", fmt.Errorf("deferred Kbuild content query %q preconfigured object-tree closure: %w", query.Token, err)
	}
	queryUsage, err := compactKbuildDeferredContentQueryEnvironmentUsage(query)
	if err != nil {
		return "", fmt.Errorf("deferred Kbuild content query %q environment usage: %w", query.Token, err)
	}
	queryValues, err := compactKbuildDeferredContentQueryClassificationValues(query)
	if err != nil {
		return "", fmt.Errorf("deferred Kbuild content query %q classification values: %w", query.Token, err)
	}

	// Preserve the argv-only fast path for scalar queries. Besides avoiding a
	// shell, ordinary command lowering discovers exact static file operands (the
	// generated asm-offsets query is one example) and binds their producers.
	var argvErr error
	protected, err := protectDeferredKbuildQueryDollars(query.Command)
	if err == nil {
		var commands []compactKbuildRecipeCommand
		commands, err = parseCompactKbuildRecipe(protected+" > "+output, compactKbuildAutomaticContext{target: query.Target})
		if err == nil {
			for commandIndex := range commands {
				commands[commandIndex].program = restoreDeferredKbuildQueryLiterals(commands[commandIndex].program)
				commands[commandIndex].stdin = restoreDeferredKbuildQueryLiterals(commands[commandIndex].stdin)
				commands[commandIndex].stdout = restoreDeferredKbuildQueryLiterals(commands[commandIndex].stdout)
				for argumentIndex := range commands[commandIndex].arguments {
					commands[commandIndex].arguments[argumentIndex] = restoreDeferredKbuildQueryLiterals(commands[commandIndex].arguments[argumentIndex])
				}
				for name, value := range commands[commandIndex].environment {
					commands[commandIndex].environment[name] = restoreDeferredKbuildQueryLiterals(value)
				}
			}
			producer, appendErr := queryBuilder.appendCompactKbuildRecipe(output, compactKbuildRuleMatch{
				profile:                  query.Profile,
				rule:                     KbuildRule{Targets: []string{output}},
				capturedEnvironment:      query.Environment,
				capturedEnvironmentUsage: queryUsage,
			}, inputs, queryValues, commands)
			if appendErr == nil {
				return producer, nil
			}
			argvErr = appendErr
		} else {
			argvErr = err
		}
	} else {
		argvErr = err
	}

	// Shell control flow and command substitutions remain one exact
	// source-derived query action. Its direct target prerequisites are already
	// materialized while the consumer node is being appended, so ruleInputs
	// resolves them to their precise producer IDs rather than to tree snapshots.
	outputTarget := compactKbuildActionObjectTreeMarker + "/" + output
	rootedCommand := compactKbuildProfileEvaluatedRootedActionRecipeText(query.Profile, query.Command)
	script := "(\n" + rootedCommand + "\n) > " + outputTarget
	commands, err := compactKbuildCompoundProgramCommands(script)
	if err != nil {
		return "", fmt.Errorf("deferred query argv lowering failed (%v); compound program discovery: %w", argvErr, err)
	}
	producer, err := queryBuilder.buildHermeticKbuildCompound(output, compactKbuildRuleMatch{
		profile:                  query.Profile,
		rule:                     KbuildRule{Targets: []string{output}},
		capturedEnvironment:      query.Environment,
		capturedEnvironmentUsage: queryUsage,
	}, inputs, script, commands)
	if err != nil {
		return "", fmt.Errorf("deferred query argv lowering failed (%v); hermetic compound: %w", argvErr, err)
	}
	return producer, nil
}

// protectDeferredKbuildQueryDollars permits literal dollars only inside a
// single-quoted argv word. Those bytes originate from $$ in the eval recipe
// and are data for tools such as awk. An unquoted/double-quoted dollar would
// require shell parameter/command expansion, which the argv-only graph does
// not provide and therefore rejects.
func protectDeferredKbuildQueryDollars(command string) (string, error) {
	if strings.Contains(command, kbuildDeferredLiteralPrefix) {
		return "", fmt.Errorf("deferred Kbuild query contains reserved literal marker")
	}
	var out strings.Builder
	quote := byte(0)
	escaped := false
	for index := 0; index < len(command); index++ {
		character := command[index]
		if escaped {
			out.WriteByte(character)
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			out.WriteByte(character)
			escaped = true
			continue
		}
		if character == '\'' || character == '"' {
			if quote == 0 {
				quote = character
			} else if quote == character {
				quote = 0
			}
			out.WriteByte(character)
			continue
		}
		if token := kbuildDeferredLiteralTokens[character]; token != "" {
			if quote != '\'' {
				if character == '$' || character == '`' {
					return "", fmt.Errorf("deferred Kbuild query requires unsupported shell expansion in %q", command)
				}
				out.WriteByte(character)
				continue
			}
			out.WriteString(token)
			continue
		}
		out.WriteByte(character)
	}
	if escaped || quote != 0 {
		return "", fmt.Errorf("deferred Kbuild query has unterminated quote or escape")
	}
	return out.String(), nil
}

func restoreDeferredKbuildQueryLiterals(value string) string {
	for character, token := range kbuildDeferredLiteralTokens {
		value = strings.ReplaceAll(value, token, string(character))
	}
	return value
}
