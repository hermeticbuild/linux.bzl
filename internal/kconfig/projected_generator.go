package kconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	compactKbuildProjectedFilechkRoot   = ".linux-bzl-projected-filechk"
	compactKbuildProjectionValidateRoot = ".linux-bzl-projection-validation"
)

type projectedGeneratorCandidate struct {
	TargetPath               string
	TargetSlot               int
	ConfigProjectionPrefixes []string
}

type projectedGeneratorOriginalOutputCommitment struct {
	TargetSlot int
	Outputs    []ActionPlanOutput
}

func projectedGeneratorOutputIsInternal(plan *ActionPlan, producerID string, slot int) bool {
	return plan != nil && plan.projectedGeneratorInternalOutputs[actionPlanOutputRef{
		producerID: producerID,
		slot:       slot,
	}]
}

func compactKbuildProjectedGeneratorIdentity(outputs []ActionPlanOutput, targetSlot int) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("linux-bzl-projected-generator-v2\x00" + strconv.Itoa(targetSlot)))
	for slot, output := range outputs {
		for _, field := range []string{
			strconv.Itoa(slot), output.Tree, structuralFamilyOutputPath(output),
			structuralFamilyArtifactPath(output), output.ObservedPath,
		} {
			_, _ = hash.Write([]byte{'\x00'})
			_, _ = hash.Write([]byte(field))
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// compactKbuildConfigSelectorPrefixes extracts deliberately simple CONFIG_*
// selectors from immutable generator text. It accepts literal identifiers and
// one parenthesized alternation, which covers record filters such as
// ^CONFIG_ARCH_(REQUIRED|DISABLED)_FEATURE_. Every result is interpreted as a
// prefix against the resolved config universe; that may over-select but can
// never make the projection smaller.
func compactKbuildConfigSelectorPrefixes(contents []byte) []string {
	prefixes := map[string]bool{}
	for offset := 0; offset < len(contents); {
		relative := bytes.Index(contents[offset:], []byte("CONFIG_"))
		if relative < 0 {
			break
		}
		start := offset + relative
		cursor := start
		literal := func(character byte) bool {
			return character == '_' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
		}
		for cursor < len(contents) && literal(contents[cursor]) {
			cursor++
		}
		base := string(contents[start:cursor])
		candidates := []string{base}
		if cursor < len(contents) && contents[cursor] == '(' {
			close := bytes.IndexByte(contents[cursor+1:], ')')
			if close >= 0 {
				close += cursor + 1
				alternatives := strings.Split(string(contents[cursor+1:close]), "|")
				valid := len(alternatives) > 1
				for _, alternative := range alternatives {
					if alternative == "" {
						valid = false
					}
					for index := 0; index < len(alternative); index++ {
						if !literal(alternative[index]) {
							valid = false
						}
					}
				}
				if valid {
					cursor = close + 1
					suffixStart := cursor
					for cursor < len(contents) && literal(contents[cursor]) {
						cursor++
					}
					suffix := string(contents[suffixStart:cursor])
					candidates = candidates[:0]
					for _, alternative := range alternatives {
						candidates = append(candidates, base+alternative+suffix)
					}
				}
			}
		}
		for _, candidate := range candidates {
			if len(candidate) > len("CONFIG_") {
				prefixes[candidate] = true
			}
		}
		offset = start + len("CONFIG_")
	}
	return sortedBoolKeys(prefixes)
}

func sortedBoolKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

type compactKbuildAwkRule struct {
	pattern string
	body    string
}

// compactKbuildAwkRules recognizes the ordinary top-level pattern/action
// structure without attempting to execute AWK. It is intentionally strict:
// unsupported lexical forms make a generator ineligible, never invalid.
func compactKbuildAwkRules(contents []byte) ([]compactKbuildAwkRule, bool) {
	text := string(contents)
	rules := []compactKbuildAwkRule{}
	position := 0
	skipSpaceAndComments := func() {
		for position < len(text) {
			if strings.ContainsRune(" \t\r\n", rune(text[position])) {
				position++
				continue
			}
			if text[position] == '#' {
				if newline := strings.IndexByte(text[position:], '\n'); newline >= 0 {
					position += newline + 1
					continue
				}
				position = len(text)
			}
			break
		}
	}
	for {
		skipSpaceAndComments()
		if position == len(text) {
			return rules, len(rules) != 0
		}
		patternStart := position
		quoted := byte(0)
		escaped := false
		open := -1
		for position < len(text) {
			character := text[position]
			if escaped {
				escaped = false
				position++
				continue
			}
			if quoted != 0 {
				if character == '\\' {
					escaped = true
				} else if character == quoted {
					quoted = 0
				}
				position++
				continue
			}
			switch character {
			case '"', '/':
				quoted = character
				position++
			case '#':
				if newline := strings.IndexByte(text[position:], '\n'); newline >= 0 {
					position += newline + 1
				} else {
					position = len(text)
				}
			case '{':
				open = position
				position++
			default:
				position++
			}
			if open >= 0 {
				break
			}
		}
		if open < 0 || quoted != 0 {
			return nil, false
		}
		pattern := strings.TrimSpace(text[patternStart:open])
		if pattern == "" {
			return nil, false
		}
		bodyStart := position
		depth := 1
		quoted = 0
		escaped = false
		comment := false
		for position < len(text) && depth != 0 {
			character := text[position]
			if comment {
				if character == '\n' {
					comment = false
				}
				position++
				continue
			}
			if escaped {
				escaped = false
				position++
				continue
			}
			if quoted != 0 {
				if character == '\\' {
					escaped = true
				} else if character == quoted {
					quoted = 0
				}
				position++
				continue
			}
			switch character {
			case '"', '/':
				quoted = character
			case '#':
				comment = true
			case '{':
				depth++
			case '}':
				depth--
			}
			position++
		}
		if depth != 0 || quoted != 0 {
			return nil, false
		}
		rules = append(rules, compactKbuildAwkRule{
			pattern: pattern,
			body:    text[bodyStart : position-1],
		})
	}
}

var compactKbuildAwkFileEquality = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])file[ \t]*==[ \t]*([0-9]+)(?:$|[^A-Za-z0-9_])`)
var compactKbuildAwkConfigSelector = regexp.MustCompile(`^\$[ \t]*1[ \t]*~[ \t]*/\^(CONFIG_[A-Z0-9_]+(?:\([A-Z0-9_|]+\)[A-Z0-9_]*)?)/$`)
var compactKbuildAwkOrdinalConjunct = regexp.MustCompile(`^file[ \t\r\n]*==[ \t\r\n]*([0-9]+)$`)
var compactKbuildAwkFieldRegexConjunct = regexp.MustCompile(`^\$[ \t]*[1-9][0-9]*[ \t\r\n]*~[ \t\r\n]*/(?:\\[^\r\n]|[^/\\\r\n])*/$`)
var compactKbuildAwkFieldLiteralConjunct = regexp.MustCompile(`^\$[ \t]*[1-9][0-9]*[ \t\r\n]*==[ \t\r\n]*(?:"(?:\\[^\r\n]|[^"\\\r\n])*"|[+-]?[0-9]+(?:\.[0-9]+)?)$`)
var compactKbuildAwkFileAssignment = regexp.MustCompile(`(?:(?:\+\+|--)[ \t]*file\b|\bfile[ \t]*(?:\+\+|--|\*\*=|[+*/%^\-]=|=(?:[^=]|$)))`)
var compactKbuildAwkFileIdentifier = regexp.MustCompile(`\bfile\b`)
var compactKbuildAwkFileZeroStatement = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(file[ \t]*=[ \t]*0)[ \t]*(?:;|\r?\n|#[^\n]*(?:\n|$)|$)`)
var compactKbuildAwkFirstFileIncrement = regexp.MustCompile(`^(?:[ \t\r\n]|#[^\n]*(?:\n|$))*(?:\+\+[ \t]*file|file[ \t]*\+\+)[ \t]*(?:;|\r?\n|#[^\n]*(?:\n|$)|$)`)

func compactKbuildAwkInitializesFileZero(body, code string) bool {
	// Match raw statement boundaries so a masked string concatenation, such
	// as file = 0 "1", cannot masquerade as zero. The assignment itself must
	// also survive masking: quoted text and comments are not initializers.
	for _, match := range compactKbuildAwkFileZeroStatement.FindAllStringSubmatchIndex(body, -1) {
		if body[match[2]:match[3]] == code[match[2]:match[3]] {
			return true
		}
	}
	return false
}

func compactKbuildAwkFileUsesAreModeled(contents string, initializer, increment bool) bool {
	// AWK can write a variable without an assignment operator (for-in,
	// sub/gsub destinations, getline). Restrict counter uses to the proven
	// statements and literal ordinal comparisons instead of recognizing every
	// possible lvalue. Preserve '/' here so division cannot hide a mutation;
	// a regexp mentioning file is conservatively outside this bounded proof.
	code := compactKbuildAwkMaskedCode([]byte(contents), false)
	clear := func(start, end int) {
		for index := start; index < end; index++ {
			code[index] = ' '
		}
	}
	if initializer {
		for _, match := range compactKbuildAwkFileZeroStatement.FindAllStringSubmatchIndex(contents, -1) {
			if contents[match[2]:match[3]] == string(code[match[2]:match[3]]) {
				clear(match[2], match[3])
			}
		}
	}
	if increment {
		if match := compactKbuildAwkFirstFileIncrement.FindStringIndex(contents); match != nil {
			clear(match[0], match[1])
		}
	}
	for _, match := range compactKbuildAwkFileEquality.FindAllIndex(code, -1) {
		clear(match[0], match[1])
	}
	return !compactKbuildAwkFileIdentifier.Match(code)
}

func compactKbuildAwkAffirmativeConfigPattern(pattern string, configOrdinal int) (int, []string, bool) {
	// This is a literal conjunction grammar, not an AWK expression parser.
	// Mask strings/regexes only to find conjunction boundaries; validate every
	// complete raw conjunct afterwards. The latter is essential because the
	// bounded masker alone cannot distinguish division from a regexp literal.
	code := compactKbuildAwkCodeOnly([]byte(pattern))
	conjuncts := []string{}
	start := 0
	for {
		next := bytes.Index(code[start:], []byte("&&"))
		if next < 0 {
			conjuncts = append(conjuncts, strings.TrimSpace(pattern[start:]))
			break
		}
		next += start
		conjuncts = append(conjuncts, strings.TrimSpace(pattern[start:next]))
		start = next + len("&&")
	}
	ordinalMatch := compactKbuildAwkOrdinalConjunct.FindStringSubmatch(conjuncts[0])
	if ordinalMatch == nil {
		return 0, nil, false
	}
	ordinal, err := strconv.Atoi(ordinalMatch[1])
	if err != nil || ordinal <= 0 {
		return 0, nil, false
	}
	var prefixes []string
	selectors := 0
	for _, conjunct := range conjuncts[1:] {
		if !compactKbuildAwkFieldRegexConjunct.MatchString(conjunct) &&
			!compactKbuildAwkFieldLiteralConjunct.MatchString(conjunct) {
			return 0, nil, false
		}
		if ordinal == configOrdinal {
			if selector := compactKbuildAwkConfigSelector.FindStringSubmatch(conjunct); selector != nil {
				selectors++
				prefixes = compactKbuildConfigSelectorPrefixes([]byte(selector[1]))
			}
		}
	}
	if ordinal == configOrdinal && (selectors != 1 || len(prefixes) == 0) {
		return 0, nil, false
	}
	return ordinal, prefixes, true
}

var compactKbuildAwkProjectionUnmodeledState = regexp.MustCompile(`\b(?:NR|FNR|FILENAME|ARGC|ARGV|ARGIND|ENVIRON|PROCINFO|RT|RS|FPAT|FIELDWIDTHS)\b`)
var compactKbuildAwkProjectionImplicitRecord = regexp.MustCompile(`\bNF\b|\b(?:sub|gsub)[ \t\r\n]*\(`)
var compactKbuildAwkLengthIdentifier = regexp.MustCompile(`\blength\b`)

func compactKbuildAwkCodeOnly(contents []byte) []byte {
	return compactKbuildAwkMaskedCode(contents, true)
}

func compactKbuildAwkMaskedCode(contents []byte, maskRegex bool) []byte {
	out := slices.Clone(contents)
	quoted := byte(0)
	escaped := false
	comment := false
	for index, character := range contents {
		if comment {
			out[index] = ' '
			if character == '\n' {
				comment = false
				out[index] = '\n'
			}
			continue
		}
		if escaped {
			out[index] = ' '
			escaped = false
			continue
		}
		if quoted != 0 {
			out[index] = ' '
			if character == '\\' {
				escaped = true
			} else if character == quoted {
				quoted = 0
			}
			continue
		}
		switch character {
		case '"':
			quoted = character
			out[index] = ' '
		case '/':
			if maskRegex {
				quoted = character
				out[index] = ' '
			}
		case '#':
			comment = true
			out[index] = ' '
		}
	}
	return out
}

func compactKbuildAwkCallsAreClosed(contents []byte) bool {
	allowed := map[string]bool{
		"for": true, "if": true, "while": true,
		"printf": true, "sub": true, "gsub": true, "split": true,
		"int": true, "length": true, "index": true, "match": true,
		"sprintf": true, "substr": true, "tolower": true, "toupper": true,
	}
	text := string(compactKbuildAwkCodeOnly(contents))
	identifier := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*[ \t\r\n]*\(`)
	for _, match := range identifier.FindAllString(text, -1) {
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(match), "("))
		if !allowed[name] {
			return false
		}
	}
	return true
}

// compactKbuildAwkConfigProjectionProof proves every record-sensitive rule
// which can observe the positional .config input is guarded by a literal,
// anchored CONFIG_* selector. Rules for other positional inputs are irrelevant;
// BEGIN/END cannot read more bytes because getline/system/unknown calls were
// rejected by the surrounding closed-source checks.
func compactKbuildAwkConfigProjectionProof(contents []byte, configOrdinal int) []string {
	if configOrdinal <= 0 || !compactKbuildAwkCallsAreClosed(contents) {
		return nil
	}
	rules, ok := compactKbuildAwkRules(contents)
	if !ok {
		return nil
	}
	prefixes := map[string]bool{}
	beginTracksFiles := false
	firstRecordTracksFiles := false
	for _, rule := range rules {
		pattern := strings.Join(strings.Fields(rule.pattern), " ")
		bodyCode := string(compactKbuildAwkCodeOnly([]byte(rule.body)))
		if !compactKbuildAwkFileUsesAreModeled(rule.pattern, false, false) ||
			!compactKbuildAwkFileUsesAreModeled(rule.body, pattern == "BEGIN", pattern == "FNR == 1") {
			return nil
		}
		// Filtering preserves the first-record boundary, not record numbers,
		// filenames, or the last record. Keep division visible for this check:
		// treating it as a regexp delimiter could hide an NR/FNR reference.
		stateCode := string(compactKbuildAwkMaskedCode([]byte(rule.body), false))
		if compactKbuildAwkProjectionUnmodeledState.MatchString(stateCode) ||
			pattern != "FNR == 1" && compactKbuildAwkProjectionUnmodeledState.MatchString(pattern) {
			return nil
		}
		if pattern == "BEGIN" || pattern == "END" || pattern == "FNR == 1" {
			// A bare regexp also reads $0. Reject unquoted '/' here rather
			// than pretending the bounded lexer distinguishes every division
			// expression from an implicit-record regexp observation.
			if strings.ContainsAny(stateCode, "$/") || compactKbuildAwkProjectionImplicitRecord.MatchString(stateCode) {
				return nil
			}
			for _, location := range compactKbuildAwkLengthIdentifier.FindAllStringIndex(stateCode, -1) {
				tail := strings.TrimSpace(stateCode[location[1]:])
				if !strings.HasPrefix(tail, "(") || strings.HasPrefix(strings.TrimSpace(tail[1:]), ")") {
					return nil
				}
			}
		}
		switch pattern {
		case "BEGIN":
			assignments := compactKbuildAwkFileAssignment.FindAllString(bodyCode, -1)
			// Positional rule guards are valid only when this counter starts
			// at exactly zero. A substring also admits file = 01 or 0 + 1.
			// Other BEGIN statements may precede the unique initializer.
			if len(assignments) != 1 || !compactKbuildAwkInitializesFileZero(rule.body, bodyCode) {
				return nil
			}
			beginTracksFiles = true
			continue
		case "END":
			if len(compactKbuildAwkFileAssignment.FindAllString(bodyCode, -1)) != 0 {
				return nil
			}
			continue
		case "FNR == 1":
			if firstRecordTracksFiles {
				return nil
			}
			assignments := compactKbuildAwkFileAssignment.FindAllString(bodyCode, -1)
			// One mutation is not enough: assignment or a conditional
			// increment can send .config through another input's unguarded
			// rules. Require a standalone, unconditional first increment;
			// subsequent field-separator conditionals remain supported.
			if len(assignments) != 1 || !compactKbuildAwkFirstFileIncrement.MatchString(rule.body) {
				return nil
			}
			firstRecordTracksFiles = true
			continue
		}
		// The tracker must run before any record-dependent rule. Otherwise an
		// omitted first record could execute the preceding file's rules.
		if !firstRecordTracksFiles {
			return nil
		}
		if len(compactKbuildAwkFileAssignment.FindAllString(bodyCode, -1)) != 0 {
			return nil
		}
		ordinal, selectedPrefixes, valid := compactKbuildAwkAffirmativeConfigPattern(rule.pattern, configOrdinal)
		if !valid {
			return nil
		}
		if ordinal != configOrdinal {
			continue
		}
		for _, prefix := range selectedPrefixes {
			prefixes[prefix] = true
		}
	}
	if !beginTracksFiles || !firstRecordTracksFiles || len(prefixes) == 0 {
		return nil
	}
	return sortedBoolKeys(prefixes)
}

func compactKbuildQuotedStringsAfterPrintf(contents []byte) ([]string, bool) {
	stringsFound := []string{}
	for offset := 0; offset < len(contents); {
		relative := bytes.Index(contents[offset:], []byte("printf"))
		if relative < 0 {
			break
		}
		start := offset + relative
		beforeOK := start == 0 || !configDependencyIdentifierByte(contents[start-1])
		after := start + len("printf")
		afterOK := after == len(contents) || !configDependencyIdentifierByte(contents[after])
		if !beforeOK || !afterOK {
			offset = after
			continue
		}
		for after < len(contents) && (contents[after] == ' ' || contents[after] == '\t' || contents[after] == '(') {
			after++
		}
		if after >= len(contents) || contents[after] != '"' {
			return nil, false
		}
		after++
		var decoded strings.Builder
		closed := false
		for after < len(contents) {
			character := contents[after]
			after++
			if character == '"' {
				closed = true
				break
			}
			if character != '\\' {
				decoded.WriteByte(character)
				continue
			}
			if after >= len(contents) {
				return nil, false
			}
			escaped := contents[after]
			after++
			switch escaped {
			case 'n':
				decoded.WriteByte('\n')
			case 't':
				decoded.WriteByte('\t')
			case 'r':
				decoded.WriteByte('\r')
			case '\\', '"':
				decoded.WriteByte(escaped)
			default:
				// Preserve an unknown escape as source text. It cannot create a
				// false closed-header proof because directive checks below still
				// require complete literal spellings.
				decoded.WriteByte('\\')
				decoded.WriteByte(escaped)
			}
		}
		if !closed {
			return nil, false
		}
		stringsFound = append(stringsFound, decoded.String())
		offset = after
	}
	return stringsFound, len(stringsFound) != 0
}

// compactKbuildLiteralClosedMacroEmitterEvidence is an optimization
// eligibility proof, not the execution validator. It requires the generator
// source itself to expose a literal printf protocol for all three pieces of a
// guarded macro header and rejects any literal unsafe preprocessor directive.
// A source which does not expose this protocol simply remains on the ordinary
// opaque filechk path and is never made invalid by the optimization.
func compactKbuildLiteralClosedMacroEmitterEvidence(contents []byte) bool {
	for _, outputPrimitive := range []string{"print", "system", "getline"} {
		for offset := 0; offset < len(contents); {
			relative := bytes.Index(contents[offset:], []byte(outputPrimitive))
			if relative < 0 {
				break
			}
			start := offset + relative
			end := start + len(outputPrimitive)
			if (start == 0 || !configDependencyIdentifierByte(contents[start-1])) &&
				(end == len(contents) || !configDependencyIdentifierByte(contents[end])) {
				return false
			}
			offset = end
		}
	}
	formats, ok := compactKbuildQuotedStringsAfterPrintf(contents)
	if !ok {
		return false
	}
	joined := strings.Join(formats, "\n")
	if !strings.Contains(joined, "#ifndef ") || !strings.Contains(joined, "#define ") ||
		!strings.Contains(joined, "#endif") {
		return false
	}
	for _, forbidden := range []string{
		"#include", "#include_next", "#import", "#embed", "#undef", "#pragma", "#line", "#error", "#warning",
		";", "{", "}",
	} {
		if strings.Contains(joined, forbidden) {
			return false
		}
	}
	return true
}

func (b *compactKbuildRulePlanBuilder) projectedFilechkInputIsNonemptySource(
	pathname string,
	inputs []compactKbuildRuleInput,
) bool {
	var sourceInput *compactKbuildRuleInput
	for index := range inputs {
		input := &inputs[index]
		if canonicalKbuildRulePath(input.path) != pathname {
			continue
		}
		if sourceInput != nil || input.sourceID == "" || input.producer != "" ||
			input.objectTree || input.recipeLocal || input.workingOnly || input.overwriteLineage {
			return false
		}
		sourceInput = input
	}
	if sourceInput == nil || b.plan.ensureSourceLookupIndex() != nil {
		return false
	}
	source, bound := b.plan.sourcesByID[sourceInput.sourceID]
	namespace, err := b.metadata.actionPlanSourceNamespace(pathname)
	if !bound || err != nil || source.Path != pathname || source.Namespace != namespace {
		return false
	}
	physical, resolved := ResolveCompactKbuildProfileSourcePath(*b.profile, pathname)
	if !resolved {
		return false
	}
	info, err := os.Stat(physical)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	file, err := os.Open(physical)
	if err != nil {
		return false
	}
	defer file.Close()
	// Read one byte rather than loading an arbitrary-sized source operand.
	// The captured immutable source binding is the lifetime authority; neither
	// declared generated paths nor a guessed ordinal can stand in for bytes.
	var first [1]byte
	n, err := file.Read(first[:])
	return err == nil && n == len(first)
}

func (b *compactKbuildRulePlanBuilder) projectedFilechkConfigPrefixes(
	commands []compactKbuildRecipeCommand,
	inputs []compactKbuildRuleInput,
) []string {
	if b == nil || b.plan == nil || b.plan.probeDiscoveryOnly || b.metadata == nil ||
		len(commands) != 1 || commands[0].connector != "" || commands[0].stdout != "" ||
		commands[0].stdin != "" || len(commands[0].environment) != 0 ||
		commands[0].shellExpansion || commands[0].programPathnameExpansion ||
		commands[0].stdinPathnameExpansion || commands[0].stdoutPathnameExpansion || b.profile == nil {
		return nil
	}
	command := commands[0]
	if err := b.annotateSourceActionRoles(&command); err != nil || command.programToolRole != "awk" {
		return nil
	}

	// This optimization is deliberately a source-language proof, not a table of
	// known generators.  Accept exactly the ordinary AWK file-program form and
	// bind the proof to the immutable script named by -f.  Other prerequisites
	// (in particular generated headers) must never accidentally opt a recipe in.
	scriptPath := ""
	configOrdinal := 0
	fileOperands := 0
	precedingInputs := []string{}
	for index, argument := range command.arguments {
		if argument == "-f" {
			if scriptPath != "" || index+1 >= len(command.arguments) {
				return nil
			}
			resolved, ok := compactKbuildProfileCommandOperandPath(*b.profile, command.arguments[index+1])
			if !ok || resolved == ".config" {
				return nil
			}
			scriptPath = canonicalKbuildRulePath(resolved)
			continue
		}
		if index > 0 && command.arguments[index-1] == "-f" {
			continue
		}
		if strings.HasPrefix(argument, "-") {
			return nil
		}
		resolved, ok := compactKbuildProfileCommandOperandPath(*b.profile, argument)
		if !ok {
			return nil
		}
		fileOperands++
		if canonicalKbuildRulePath(resolved) == ".config" {
			if configOrdinal != 0 {
				return nil
			}
			configOrdinal = fileOperands
		} else {
			precedingInputs = append(precedingInputs, canonicalKbuildRulePath(resolved))
		}
	}
	if scriptPath == "" || configOrdinal == 0 || configOrdinal != fileOperands {
		return nil
	}
	// FNR==1 counts nonempty files, not argv operands. Prove every preceding
	// input contributes a first record before using the positional ordinal in
	// the source-language proof. Unknown generated data stays ineligible.
	for _, preceding := range precedingInputs {
		if !b.projectedFilechkInputIsNonemptySource(preceding, inputs) {
			return nil
		}
	}

	var script *compactKbuildRuleInput
	for _, input := range inputs {
		if canonicalKbuildRulePath(input.path) != scriptPath {
			continue
		}
		if script != nil || input.sourceID == "" || input.producer != "" || input.objectTree {
			return nil
		}
		candidate := input
		script = &candidate
	}
	if script == nil {
		return nil
	}
	physical, ok := ResolveCompactKbuildProfileSourcePath(*b.profile, script.path)
	if !ok {
		return nil
	}
	info, err := os.Stat(physical)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil
	}
	contents, err := os.ReadFile(physical)
	if err != nil || !compactKbuildLiteralClosedMacroEmitterEvidence(contents) {
		return nil
	}
	prefixes := compactKbuildAwkConfigProjectionProof(contents, configOrdinal)
	if len(prefixes) == 0 {
		return nil
	}
	return prefixes
}

func captureProjectedCompactKbuildFilechkOutput(
	target string,
	commands []compactKbuildRecipeCommand,
	prefixes []string,
) ([]compactKbuildRecipeCommand, error) {
	if len(commands) != 1 || commands[0].stdout != "" || commands[0].connector != "" || len(prefixes) == 0 {
		return nil, fmt.Errorf("projected filechk requires one unredirected simple command")
	}
	// Ordinary ActionPlan execution remains the upstream single full-config
	// action. Snapshot planning recognizes this stable marker and performs the
	// projected/full differential lowering only for a family, where a subset
	// capsule can actually be supplied.
	commands[0].stdout = target
	commands[0].configProjectionPrefixes = slices.Clone(prefixes)
	commands[0].configProjectionTarget = target
	return commands, nil
}

func appendCompactKbuildProjectedGeneratorValidation(
	plan *ActionPlan,
	target string,
	targetSlot int,
	projectedProducer string,
	projectedNode ActionPlanNode,
	projectedRecipe ActionRecipe,
	originalOutputs []ActionPlanOutput,
) (string, string, map[string]bool, error) {
	if plan == nil || len(projectedRecipe.ConfigProjectionPrefixes) == 0 ||
		len(projectedNode.Outputs) == 0 || len(projectedNode.Outputs) != len(originalOutputs) ||
		targetSlot < 0 || targetSlot >= len(projectedNode.Outputs) ||
		originalOutputs[targetSlot].Path != target || originalOutputs[targetSlot].ObservedPath != "" {
		return "", "", nil, fmt.Errorf("projected generator %q has invalid target slot %d", target, targetSlot)
	}
	fullNode := projectedNode
	fullNode.ID = ""
	fullNode.Sources = slices.Clone(projectedNode.Sources)
	fullNode.Inputs = slices.Clone(projectedNode.Inputs)
	fullNode.Trees = slices.Clone(projectedNode.Trees)
	fullNode.AuxiliaryTools = slices.Clone(projectedNode.AuxiliaryTools)
	fullNode.Outputs = slices.Clone(originalOutputs)
	identity := compactKbuildProjectedGeneratorIdentity(originalOutputs, targetSlot)
	fullNode.Outputs[targetSlot].ArtifactPath = path.Join(
		compactKbuildProjectionValidateRoot,
		identity, "full", planOrdinal(targetSlot),
	)
	fullRecipe := cloneActionRecipe(projectedRecipe)
	fullRecipe.ConfigProjectionPrefixes = nil
	fullProducer, err := appendOwnedPreparedActionPlanNode(plan, fullNode, fullRecipe, nil)
	if err != nil {
		return "", "", nil, fmt.Errorf("append full-config generator replay: %w", err)
	}

	canonicalNode := ActionPlanNode{
		Stage: projectedNode.Stage, Kind: "generate", Tool: "actionfile", Product: projectedNode.Product,
		Inputs:  []ActionPlanNodeEdge{{Role: "projection-raw", ProducerID: projectedProducer, Slot: targetSlot}},
		Outputs: []ActionPlanOutput{originalOutputs[targetSlot]},
	}
	canonicalRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "generate", Tool: "actionfile",
		Arguments: []string{
			"-input", "${input:projection-raw:00000000}",
			"-validate_closed_integer_macro_header_v1",
			"-out", "${output:00000000}",
		},
		Inputs: []string{"projection-raw:00000000"}, Outputs: []string{"00000000"},
	}
	canonicalProducer, err := appendActionPlanNode(plan, canonicalNode, canonicalRecipe)
	if err != nil {
		return "", "", nil, fmt.Errorf("append projected-generator canonical validator: %w", err)
	}

	stampPath := path.Join(
		compactKbuildProjectionValidateRoot,
		identity, "validated",
	)
	stampNode := ActionPlanNode{
		Stage: projectedNode.Stage, Kind: "metadata", Tool: "actionfile", Product: projectedNode.Product,
		Inputs: []ActionPlanNodeEdge{
			{Role: "projected", ProducerID: projectedProducer, Slot: targetSlot},
			{Role: "full", ProducerID: fullProducer, Slot: targetSlot},
		},
		Outputs: []ActionPlanOutput{{Tree: originalOutputs[targetSlot].Tree, Path: stampPath}},
	}
	stampRecipe := ActionRecipe{
		Schema: LinuxKernelPlanSchema, Kind: "metadata", Tool: "actionfile",
		Arguments: []string{
			"-compare_input", "${input:projected:00000000}",
			"-compare_input", "${input:full:00000001}",
			"-out", "${output:00000000}",
		},
		Inputs: []string{"projected:00000000", "full:00000001"}, Outputs: []string{"00000000"},
	}
	stampProducer, err := appendActionPlanNode(plan, stampNode, stampRecipe)
	if err != nil {
		return "", "", nil, fmt.Errorf("append projected-generator equivalence validation: %w", err)
	}
	plan.projectedGeneratorValidations = append(plan.projectedGeneratorValidations, stampProducer)
	plan.projectedGeneratorInternalNodes[fullProducer] = true
	plan.projectedGeneratorInternalNodes[stampProducer] = true
	plan.projectedGeneratorInternalOutputs[actionPlanOutputRef{producerID: fullProducer, slot: targetSlot}] = true
	plan.projectedGeneratorInternalOutputs[actionPlanOutputRef{producerID: stampProducer, slot: 0}] = true
	return canonicalProducer, fullProducer, map[string]bool{
		canonicalProducer + "\x00" + planOrdinal(0): true,
		stampProducer + "\x00" + planOrdinal(0):     true,
		stampProducer + "\x00" + planOrdinal(1):     true,
	}, nil
}

// lowerProjectedGeneratorsForFamilySnapshot turns stable source-language
// projection markers into a differential family graph. It runs only on the
// snapshot path; ordinary ActionPlan users keep the original single execution.
func lowerProjectedGeneratorsForFamilySnapshot(plan *ActionPlan) error {
	if plan == nil {
		return nil
	}
	type pendingProjectedGeneratorValidation struct {
		target          string
		targetSlot      int
		projectedNode   ActionPlanNode
		projectedRecipe ActionRecipe
		originalOutputs []ActionPlanOutput
	}
	originalCount := len(plan.Nodes)
	pendingValidations := make([]pendingProjectedGeneratorValidation, 0, len(plan.projectedGeneratorCandidates))
	replacements := map[actionPlanOutputRef]actionPlanOutputRef{}
	protected := map[string]bool{}
	if plan.projectedGeneratorInternalNodes == nil {
		plan.projectedGeneratorInternalNodes = map[string]bool{}
	}
	if plan.projectedGeneratorInternalOutputs == nil {
		plan.projectedGeneratorInternalOutputs = map[actionPlanOutputRef]bool{}
	}
	if plan.projectedGeneratorOriginalOutputs == nil {
		plan.projectedGeneratorOriginalOutputs = map[string]projectedGeneratorOriginalOutputCommitment{}
	}
	for index := 0; index < originalCount; index++ {
		node := plan.Nodes[index]
		candidate, candidateOK := plan.projectedGeneratorCandidates[node.ID]
		if !candidateOK {
			continue
		}
		if len(candidate.ConfigProjectionPrefixes) == 0 {
			return fmt.Errorf("projected generator %s has no config projection prefixes", node.ID)
		}
		recipe, ok := plan.Recipes[node.Recipe]
		if !ok {
			return fmt.Errorf("projected generator %s has no recipe", node.ID)
		}
		if candidate.TargetSlot < 0 || candidate.TargetSlot >= len(node.Outputs) {
			return fmt.Errorf("projected generator %s target slot %d is absent from %d outputs", node.ID, candidate.TargetSlot, len(node.Outputs))
		}
		targetOutput := node.Outputs[candidate.TargetSlot]
		if candidate.TargetPath == "" || targetOutput.Path != candidate.TargetPath {
			return fmt.Errorf("projected generator %s target slot %d path %q disagrees with provenance %q", node.ID, candidate.TargetSlot, targetOutput.Path, candidate.TargetPath)
		}
		if targetOutput.ObservedPath != "" {
			return fmt.Errorf("projected generator %s target slot %d is not an ordinary output", node.ID, candidate.TargetSlot)
		}
		// Every selected writer is recognized before the overwrite graph assigns
		// immutable ArtifactPaths.  A historical writer therefore remains a
		// candidate, but it is not the canonical header consumed after the final
		// overwrite.  Keep that version and its full-config dependency untouched;
		// only the one canonical winner may publish a projected substitute.
		if !actionPlanOutputIsCanonical(targetOutput) {
			continue
		}
		recipe = cloneActionRecipe(recipe)
		recipe.ConfigProjectionPrefixes = slices.Clone(candidate.ConfigProjectionPrefixes)
		recipeID, err := recipe.ID()
		if err != nil {
			return fmt.Errorf("projected generator %s recipe: %w", node.ID, err)
		}
		plan.Recipes[recipeID] = recipe
		node.Recipe = recipeID
		target := targetOutput.Path
		originalOutputs := slices.Clone(node.Outputs)
		plan.projectedGeneratorOriginalOutputs[node.ID] = projectedGeneratorOriginalOutputCommitment{
			TargetSlot: candidate.TargetSlot,
			Outputs:    slices.Clone(originalOutputs),
		}
		identity := compactKbuildProjectedGeneratorIdentity(originalOutputs, candidate.TargetSlot)
		node.Outputs = slices.Clone(originalOutputs)
		for slot := range node.Outputs {
			node.Outputs[slot].ArtifactPath = path.Join(
				compactKbuildProjectedFilechkRoot, identity, "projected", planOrdinal(slot),
			)
			plan.projectedGeneratorInternalOutputs[actionPlanOutputRef{producerID: node.ID, slot: slot}] = true
		}
		plan.Nodes[index] = node
		plan.projectedGeneratorInternalNodes[node.ID] = true
		pendingValidations = append(pendingValidations, pendingProjectedGeneratorValidation{
			target:          target,
			targetSlot:      candidate.TargetSlot,
			projectedNode:   node,
			projectedRecipe: recipe,
			originalOutputs: originalOutputs,
		})
	}

	// Mutating an indexed original node invalidates every lookup. Stage every
	// mutation before appending the validation suffix so the first append pays
	// for one full rebuild and every later append can extend it incrementally.
	// Interleaving these phases rebuilds the growing full-plan index once per
	// projected generator.
	if len(pendingValidations) != 0 {
		plan.invalidateLookupIndexes()
	}
	for _, pending := range pendingValidations {
		canonical, full, internalEdges, err := appendCompactKbuildProjectedGeneratorValidation(
			plan,
			pending.target,
			pending.targetSlot,
			pending.projectedNode.ID,
			pending.projectedNode,
			pending.projectedRecipe,
			pending.originalOutputs,
		)
		if err != nil {
			return err
		}
		for slot := range pending.originalOutputs {
			replacement := actionPlanOutputRef{producerID: full, slot: slot}
			if slot == pending.targetSlot {
				replacement = actionPlanOutputRef{producerID: canonical, slot: 0}
			}
			replacements[actionPlanOutputRef{producerID: pending.projectedNode.ID, slot: slot}] = replacement
		}
		for key := range internalEdges {
			protected[key] = true
		}
	}
	plan.projectedGeneratorCandidates = nil
	if len(replacements) == 0 {
		return nil
	}
	store, err := plan.planningActionPlanInputSetStore()
	if err != nil {
		return fmt.Errorf("projected-generator input-set rewrite: %w", err)
	}
	inputSetMapper, err := store.NewTargetStableMapper(func(entry ActionPlanInputSetEntry) (ActionPlanInputSetEntry, error) {
		if replacement, ok := replacements[actionPlanOutputRef{
			producerID: entry.ProducerID,
			slot:       entry.Slot,
		}]; ok {
			entry.ProducerID = replacement.producerID
			entry.Slot = replacement.slot
		}
		return entry, nil
	})
	if err != nil {
		return fmt.Errorf("projected-generator input-set rewrite: %w", err)
	}
	for nodeIndex := range plan.Nodes {
		node := &plan.Nodes[nodeIndex]
		for edgeIndex := range node.Inputs {
			key := node.ID + "\x00" + planOrdinal(edgeIndex)
			if protected[key] {
				continue
			}
			input := node.Inputs[edgeIndex]
			if replacement, ok := replacements[actionPlanOutputRef{producerID: input.ProducerID, slot: input.Slot}]; ok {
				node.Inputs[edgeIndex].ProducerID = replacement.producerID
				node.Inputs[edgeIndex].Slot = replacement.slot
			}
		}
		if node.InputSet != "" {
			root, err := inputSetMapper.Map(node.InputSet)
			if err != nil {
				return fmt.Errorf("projected-generator input-set rewrite for node %s: %w", node.ID, err)
			}
			node.InputSet = root
		}
	}
	plan.invalidateLookupIndexes()
	return nil
}

func projectedGeneratorConfigDependencies(
	plan *ActionPlan,
	node ActionPlanNode,
	recipe ActionRecipe,
) (ConfigDependencySet, string) {
	if node.Kind != "generate" || recipe.Kind != "generate" || len(recipe.ConfigProjectionPrefixes) == 0 {
		return ConfigDependencySet{}, "invalid projected-generator recipe contract"
	}
	configPath := ""
	if marker, used, err := configDependencyRecipeConfigSourceUse(plan, node, recipe, true); err != nil {
		return ConfigDependencySet{}, "projected generator persistent config source cannot be resolved: " + err.Error()
	} else if used && strings.HasPrefix(marker, "${source:") && strings.HasSuffix(marker, "}") {
		binding := strings.TrimSuffix(strings.TrimPrefix(marker, "${source:"), "}")
		source, ok := actionPlanConfigDependencySourceBinding(plan, node, binding)
		if !ok {
			return ConfigDependencySet{}, "projected generator has an unresolved config source binding"
		}
		projection, ok := configDependencyResolvedProjectionSource(source)
		if !ok {
			return ConfigDependencySet{}, "projected generator source is not a resolved config projection"
		}
		configPath = projection
	}
	if marker, used, err := configDependencyRecipeConfigInputUse(plan, node, recipe, true); err != nil {
		return ConfigDependencySet{}, "projected generator persistent config input cannot be resolved: " + err.Error()
	} else if used && strings.HasPrefix(marker, "${input:") && strings.HasSuffix(marker, "}") {
		binding := strings.TrimSuffix(strings.TrimPrefix(marker, "${input:"), "}")
		input, ok := actionPlanConfigDependencyInputBinding(plan, node, binding)
		if !ok || !input.fallback {
			return ConfigDependencySet{}, "projected generator has an unresolved config input binding"
		}
		if configPath != "" && configPath != input.projection {
			return ConfigDependencySet{}, "projected generator consumes multiple resolved config projections"
		}
		configPath = input.projection
	}
	// ConfigProjectionPrefixes is a source-language proof that this generator
	// consumes .config. A compacted closure therefore needs no recipe-local
	// placeholder, but it still must bind that exact working pathname to the
	// resolved .config source/copy rather than trusting the destination alone.
	entry, found, err := lookupActionPlanConfigDependencyWorkInput(plan, node, ".config")
	if err != nil {
		return ConfigDependencySet{}, "projected generator persistent .config input cannot be resolved: " + err.Error()
	}
	if found {
		provenance, err := actionPlanConfigDependencyInputSetProvenance(plan, entry)
		if err != nil {
			return ConfigDependencySet{}, "projected generator persistent .config provenance cannot be resolved: " + err.Error()
		}
		if !provenance.config || provenance.projection != ".config" {
			return ConfigDependencySet{}, "projected generator persistent .config target is not the resolved .config projection"
		}
		if configPath != "" && configPath != provenance.projection {
			return ConfigDependencySet{}, "projected generator consumes multiple resolved config projections"
		}
		configPath = provenance.projection
	}
	if configPath != ".config" {
		return ConfigDependencySet{}, "projected generator does not consume exactly the resolved .config projection"
	}
	if len(recipe.WorkingTrees) != 0 || len(recipe.CommandReplays) != 0 ||
		strings.HasPrefix(recipe.Tool, "input:") || recipe.Tool == compactKbuildScriptRunnerRole {
		return ConfigDependencySet{}, "projected generator has an unbounded execution envelope"
	}
	if plan.metadata == nil {
		return ConfigDependencySet{}, "projected generator has no resolved config symbol set"
	}
	universe := plan.metadata.configSymbolUniverse
	if len(universe) == 0 {
		// Synthetic/unit-test metadata predating the stable universe retains the
		// conservative written-symbol fallback. Production metadata always carries
		// the resolver-derived complete universe.
		for symbol := range plan.metadata.configFragment {
			universe = append(universe, symbol)
		}
		sort.Strings(universe)
	} else if !slices.IsSorted(universe) {
		// Compact metadata owns a canonical sorted universe. Keep hand-built
		// callers sound without mutating their backing storage.
		universe = slices.Clone(universe)
		slices.Sort(universe)
	}
	selected := map[string]bool{}
	for _, prefix := range recipe.ConfigProjectionPrefixes {
		// The resolver's symbol universe is sorted. Jump directly to the prefix
		// range instead of rescanning every Kconfig symbol for every projected
		// generator and every prefix.
		start := sort.SearchStrings(universe, prefix)
		for index := start; index < len(universe) && strings.HasPrefix(universe[index], prefix); index++ {
			selected[universe[index]] = true
		}
	}
	return ConfigDependencySet{Symbols: sortedBoolKeys(selected)}, ""
}
