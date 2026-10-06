package kconfig

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Generated prerequisites: turning parsed Kbuild rules into source resolution.
//
// A handful of leaf objects have no source in the tree; theirs is produced by
// a per-directory rule, for example
//
//	$(obj)/%.S: $(src)/%.pl FORCE
//		$(call if_changed,perlasm)
//
// Deriving that from the parsed rules replaces hardcoded object-path tables,
// which encode one kernel version's layout and silently misdescribe another.
// This lives beside the parser because only the parser knows rootDir, which
// turning "$(src)"-expanded absolute prerequisites back into source-relative
// paths needs.
//
// Two deliberate divergences from GNU make, both bounded by what Kbuild
// writes: no directory-split implicit matching (every rule here is
// "$(obj)/"-prefixed, so its pattern contains a slash and a whole-path match
// is equivalent), and one level of rule chaining rather than a recursive
// implicit search.

// kbuildPhonyPrerequisites are the prerequisite names that carry no file.
// Kbuild's ".PHONY" declarations are not tracked by the parser, so the list is
// deliberately closed: an unrecognised bare word is reported, not guessed at.
var kbuildPhonyPrerequisites = map[string]bool{
	"FORCE": true,
}

// KbuildGeneratedSource describes the rule that produces a leaf object's
// source file, resolved for one specific object.
type KbuildGeneratedSource struct {
	// Target is the source-relative path of the generated file, for example
	// "lib/raid6/int1.c".
	Target string
	// Stem is GNU make's $* for the matched pattern rule, empty for an
	// explicit rule.
	Stem string
	// Primary is GNU make's $<: the first prerequisite, bound before phony
	// prerequisites are filtered out, exactly as make binds it.
	Primary string
	// Prerequisites are the non-order-only prerequisites in rule order with
	// phonies removed.
	Prerequisites []string
	// CommandTemplate is the resolved "cmd_<name>" macro body with the stem
	// left as "$*", used for generator classification and diagnostics. The
	// template is the same text in every kernel version and for every stem,
	// which is what makes it usable as a registry key. Automatic variables
	// survive expansion, so it still shows which argument is the input and
	// which is the output.
	CommandTemplate string
	// CommandName is the "<name>" of that macro, for diagnostics only.
	// Generators are classified by command text, never by this name: 6.12
	// spells the same generator "perlasm" on x86 and "perl" on arm.
	CommandName string
	// Directory is the Makefile directory the rule came from.
	Directory string
	// Position is the rule's position, for diagnostics.
	Position Position
}

// KbuildGeneratedSourceResolver answers generated-source queries for a parsed
// Kbuild tree. Building the per-directory rule buckets once amortises the cost
// across the many objects a configuration resolves.
type KbuildGeneratedSourceResolver struct {
	rootDir  string
	rules    map[string][]*KbuildRule
	commands map[string]map[string]*KbuildCommand
	// exists memoises prerequisite existence probes. Candidate sources are
	// retried once per compiled extension and sibling objects share a pattern
	// rule, so the same handful of paths are otherwise stat'ed repeatedly.
	exists map[string]bool
}

// prerequisiteExists reports whether an absolute prerequisite path is present,
// caching the answer for the resolver's lifetime.
func (r *KbuildGeneratedSourceResolver) prerequisiteExists(absolute string) bool {
	if present, ok := r.exists[absolute]; ok {
		return present
	}
	present := fileExists(absolute)
	r.exists[absolute] = present
	return present
}

// GeneratedSourceResolver builds a resolver over the parsed tree's rules. The
// result is cached on the parsed file: building it walks every rule and
// command, and a compact run resolves many configurations against one tree.
func (kb *KbuildFile) GeneratedSourceResolver() *KbuildGeneratedSourceResolver {
	if kb.generatedResolver != nil {
		return kb.generatedResolver
	}
	r := &KbuildGeneratedSourceResolver{
		rootDir:  kb.rootDir,
		rules:    map[string][]*KbuildRule{},
		commands: map[string]map[string]*KbuildCommand{},
		exists:   map[string]bool{},
	}
	for i := range kb.Rules {
		rule := &kb.Rules[i]
		// A rule with no recipe declares a dependency, it does not produce
		// anything. drivers/tty/vt/Makefile's
		//
		//	$(obj)/defkeymap.o: $(obj)/defkeymap.c
		//
		// is the case that matters: without this guard a resolver hijacks
		// defkeymap.o away from its checked-in "_shipped" source.
		if len(rule.Recipe) == 0 {
			continue
		}
		for _, target := range rule.Targets {
			dir := path.Dir(filepath.ToSlash(target))
			r.rules[dir] = append(r.rules[dir], rule)
		}
	}
	for i := range kb.Commands {
		command := &kb.Commands[i]
		byName := r.commands[command.Directory]
		if byName == nil {
			byName = map[string]*KbuildCommand{}
			r.commands[command.Directory] = byName
		}
		byName[command.Name] = command
	}
	kb.generatedResolver = r
	return r
}

// ForObject resolves the rule that generates a leaf object's source.
//
// Resolution is object-driven rather than enumerative: a pattern rule
// describes an unbounded set of targets, so there is nothing to enumerate
// over. The caller supplies the object, this derives the finite candidate
// source list and matches the rules against that.
func (r *KbuildGeneratedSourceResolver) ForObject(object string) (KbuildGeneratedSource, bool, error) {
	stem, ok := strings.CutSuffix(filepath.ToSlash(object), ".o")
	if !ok {
		return KbuildGeneratedSource{}, false, nil
	}
	dir := path.Dir(stem)
	candidates := r.rules[dir]
	if len(candidates) == 0 {
		return KbuildGeneratedSource{}, false, nil
	}
	// Compiled sources only, sharing sourceForObject's candidate order so an
	// object that could plausibly come from either never resolves differently
	// here than it would on disk.
	for _, ext := range compiledSourceExtensions {
		target := stem + ext
		match, found, err := r.selectRule(target, candidates)
		if err != nil {
			return KbuildGeneratedSource{}, false, err
		}
		if !found {
			continue
		}
		generated, err := r.buildGeneratedSource(object, target, match)
		if err != nil {
			return KbuildGeneratedSource{}, false, err
		}
		return generated, true, nil
	}
	return KbuildGeneratedSource{}, false, nil
}

// ruleMatch is a candidate rule paired with how it matched a target.
type ruleMatch struct {
	rule *KbuildRule
	stem string
}

// selectRule picks the rule that GNU make would use to build target.
func (r *KbuildGeneratedSourceResolver) selectRule(target string, candidates []*KbuildRule) (ruleMatch, bool, error) {
	// An explicit rule always beats a pattern rule, and among explicit rules
	// the last definition wins. This is what makes 6.12's arm64 override
	//
	//	$(obj)/%-core.S:      $(src)/%-armv8.pl
	//	$(obj)/sha256-core.S: $(src)/sha512-armv8.pl
	//
	// resolve to sha512-armv8.pl, which the hardcoded table got wrong.
	var explicit ruleMatch
	found := false
	for _, rule := range candidates {
		if rule.TargetPattern != "" {
			continue
		}
		for _, candidate := range rule.Targets {
			if filepath.ToSlash(candidate) == target {
				explicit = ruleMatch{rule: rule}
				found = true
				break
			}
		}
	}
	if found {
		return explicit, true, nil
	}

	var matches []ruleMatch
	for _, rule := range candidates {
		stem, ok := r.matchPatternRule(rule, target)
		if !ok {
			continue
		}
		// GNU make only considers a pattern rule whose prerequisites can all
		// be made. Nothing here can make a missing file, so requiring the
		// prerequisites to exist is the equivalent test, and it is what stops
		// a directory-wide "%.S: %.pl" rule from claiming every object in that
		// directory that happens to lack a source.
		if !r.patternPrerequisitesResolve(rule, stem) {
			continue
		}
		matches = append(matches, ruleMatch{rule: rule, stem: stem})
	}
	if len(matches) == 0 {
		return ruleMatch{}, false, nil
	}

	// GNU make prefers the pattern rule with the shortest stem, then the last
	// one defined.
	best := matches[0]
	for _, match := range matches[1:] {
		if len(match.stem) <= len(best.stem) {
			best = match
		}
	}
	// Two rules at the same position are one rule listing several targets, so
	// only distinct positions constitute a real tie.
	var tied []ruleMatch
	seen := map[Position]bool{}
	for _, match := range matches {
		if len(match.stem) != len(best.stem) || seen[match.rule.Position] {
			continue
		}
		seen[match.rule.Position] = true
		tied = append(tied, match)
	}
	if len(tied) > 1 {
		return ruleMatch{}, false, fmt.Errorf(
			"Kbuild generated source for %q is ambiguous: %s match with an equally short stem %q; "+
				"disambiguate the rules or teach the resolver which one wins",
			target, describeRulePositions(tied), best.stem)
	}
	return best, true, nil
}

// matchPatternRule reports whether rule is a pattern rule matching target, and
// returns the stem. Static pattern rules ("targets: pattern: prereqs") are
// bounded by their listed targets, so both the listing and the pattern have to
// agree.
func (r *KbuildGeneratedSourceResolver) matchPatternRule(rule *KbuildRule, target string) (string, bool) {
	if rule.TargetPattern != "" {
		listed := false
		for _, candidate := range rule.Targets {
			if filepath.ToSlash(candidate) == target {
				listed = true
				break
			}
		}
		if !listed {
			return "", false
		}
		return makePatternStem(filepath.ToSlash(rule.TargetPattern), target)
	}
	for _, candidate := range rule.Targets {
		candidate = filepath.ToSlash(candidate)
		if !strings.ContainsRune(candidate, '%') {
			continue
		}
		if stem, ok := makePatternStem(candidate, target); ok {
			return stem, true
		}
	}
	return "", false
}

// patternPrerequisitesResolve reports whether every non-phony, non-order-only
// prerequisite of a stem-substituted pattern rule exists in the source tree.
func (r *KbuildGeneratedSourceResolver) patternPrerequisitesResolve(rule *KbuildRule, stem string) bool {
	if r.rootDir == "" {
		return false
	}
	for _, prerequisite := range rule.Prerequisites {
		prerequisite = substituteMakeStem(prerequisite, stem)
		resolved, err := r.sourceRelativePath(prerequisite)
		if err != nil {
			return false
		}
		if kbuildPhonyPrerequisites[resolved] {
			continue
		}
		if !r.prerequisiteExists(filepath.Join(r.rootDir, filepath.FromSlash(resolved))) {
			return false
		}
	}
	return true
}

// buildGeneratedSource turns a matched rule into the resolved description.
func (r *KbuildGeneratedSourceResolver) buildGeneratedSource(object, target string, match ruleMatch) (KbuildGeneratedSource, error) {
	rule := match.rule
	generated := KbuildGeneratedSource{
		Target:    target,
		Stem:      match.stem,
		Directory: rule.Directory,
		Position:  rule.Position,
	}

	resolve := func(list []string, what string) ([]string, error) {
		out := make([]string, 0, len(list))
		for _, entry := range list {
			resolved, err := r.sourceRelativePath(substituteMakeStem(entry, match.stem))
			if err != nil {
				return nil, fmt.Errorf(
					"Kbuild rule at %s generating %q for object %q has an unusable %s %q: %w",
					rule.Position.String(), target, object, what, entry, err)
			}
			out = append(out, resolved)
		}
		return out, nil
	}

	prerequisites, err := resolve(rule.Prerequisites, "prerequisite")
	if err != nil {
		return KbuildGeneratedSource{}, err
	}
	// GNU make binds $< to the first prerequisite including phony ones, so it
	// is read before FORCE is filtered out. A rule whose $< is phony would
	// feed the generator a non-file, so refuse rather than guess.
	if len(prerequisites) != 0 {
		generated.Primary = prerequisites[0]
		if kbuildPhonyPrerequisites[generated.Primary] {
			return KbuildGeneratedSource{}, fmt.Errorf(
				"Kbuild rule at %s generating %q for object %q binds $< to the phony prerequisite %q",
				rule.Position.String(), target, object, generated.Primary)
		}
	}
	for _, prerequisite := range prerequisites {
		if kbuildPhonyPrerequisites[prerequisite] {
			continue
		}
		generated.Prerequisites = append(generated.Prerequisites, prerequisite)
	}

	name, ok := kbuildRecipeCommandName(rule.Recipe)
	if !ok {
		return KbuildGeneratedSource{}, fmt.Errorf(
			"Kbuild rule at %s generating %q for object %q has an unrecognised recipe %q; "+
				"only a single $(call if_changed,<name>), $(call cmd,<name>) or "+
				"$(call if_changed_dep,<name>) line is understood",
			rule.Position.String(), target, object, strings.Join(rule.Recipe, "; "))
	}
	generated.CommandName = name

	command, ok := r.commands[rule.Directory][name]
	if !ok {
		return KbuildGeneratedSource{}, fmt.Errorf(
			"Kbuild rule at %s generating %q for object %q invokes cmd_%s, "+
				"which is not defined in that directory",
			rule.Position.String(), target, object, name)
	}
	// The stem is substituted here so downstream classification sees a
	// concrete command. cmd_unroll's "-v N=$*" is the case that needs it.
	//
	// Script paths inside a command come from "$(src)/..." and are therefore
	// absolute, so they are rewritten back to source-relative form. Leaving
	// them absolute would make the command text depend on where the tree was
	// unpacked, which would leak a build-machine path into every content ID
	// derived from it.
	generated.CommandTemplate = r.sourceRelativeCommand(command.Value)

	// Detecting a reference that expanded to nothing needs the raw text: a
	// recognised conditional sibling makes an unknown "$(foo-y)" expand to ""
	// with ok=true, so it is gone before anything downstream can see it. A
	// silently dropped argument would turn a three-argument invocation into a
	// two-argument one, which is a wrong answer rather than a failure.
	if err := checkKbuildCommandErasure(command, rule, target, object); err != nil {
		return KbuildGeneratedSource{}, err
	}
	return generated, nil
}

// checkKbuildCommandErasure fails when expanding a cmd_* macro dropped a
// non-automatic reference instead of preserving it.
func checkKbuildCommandErasure(command *KbuildCommand, rule *KbuildRule, target, object string) error {
	for _, reference := range makeVariableRefs(command.Raw) {
		if isMakeAutomaticVariable(reference) {
			continue
		}
		if strings.Contains(command.Value, "$("+reference+")") ||
			strings.Contains(command.Value, "${"+reference+"}") {
			continue
		}
		// The reference resolved to something. That is only safe when it
		// resolved to a non-empty value: an erased reference leaves no trace
		// in the expanded text for the registry to reject.
		if kbuildReferenceExpandedEmpty(command.Raw, command.Value, reference) {
			return fmt.Errorf(
				"Kbuild rule at %s generating %q for object %q invokes cmd_%s = %q, "+
					"in which $(%s) expanded to nothing; refusing to guess the intended arguments",
				rule.Position.String(), target, object, command.Name, command.Raw, reference)
		}
	}
	return nil
}

// kbuildReferenceExpandedEmpty reports whether dropping "$(reference)" from the
// raw text reproduces the expanded text modulo whitespace, which is the
// signature of a reference that expanded to nothing.
func kbuildReferenceExpandedEmpty(raw, value, reference string) bool {
	erased := strings.ReplaceAll(raw, "$("+reference+")", "")
	erased = strings.ReplaceAll(erased, "${"+reference+"}", "")
	return collapseMakeWhitespace(erased) == collapseMakeWhitespace(value)
}

// sourceRelativeCommand rewrites absolute in-tree paths in a command body back
// to source-relative form, leaving every other word untouched.
func (r *KbuildGeneratedSourceResolver) sourceRelativeCommand(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if !path.IsAbs(filepath.ToSlash(field)) {
			continue
		}
		relative, err := r.sourceRelativePath(field)
		if err != nil {
			continue
		}
		fields[i] = relative
	}
	return strings.Join(fields, " ")
}

// sourceRelativePath normalises a rule path into a source-relative slash path.
//
// "$(src)/..." prerequisites arrive absolute under the parse root, while
// "$(obj)/..." ones are already source-relative. Bare words with no separator
// are phony targets or nothing at all.
func (r *KbuildGeneratedSourceResolver) sourceRelativePath(value string) (string, error) {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" {
		return "", fmt.Errorf("empty path")
	}
	if kbuildPhonyPrerequisites[value] {
		return value, nil
	}
	if containsMakeReference(value) {
		return "", fmt.Errorf("unresolved make reference")
	}
	if !strings.ContainsRune(value, '/') {
		return "", fmt.Errorf(
			"bare name %q is neither a path nor a known phony target (known: %s)",
			value, strings.Join(sortedKeys(kbuildPhonyPrerequisites), ", "))
	}
	if path.IsAbs(value) {
		if r.rootDir == "" {
			return "", fmt.Errorf("absolute path %q with no known source root", value)
		}
		relative, err := filepath.Rel(r.rootDir, filepath.FromSlash(value))
		if err != nil {
			return "", fmt.Errorf("path %q is not under the source root %q", value, r.rootDir)
		}
		relative = filepath.ToSlash(relative)
		if relative == ".." || strings.HasPrefix(relative, "../") {
			return "", fmt.Errorf("path %q escapes the source root %q", value, r.rootDir)
		}
		return path.Clean(relative), nil
	}
	cleaned := path.Clean(value)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("relative path %q escapes the source root", value)
	}
	return cleaned, nil
}

// kbuildRecipeCommandName extracts "<name>" from a recipe consisting of a
// single "$(call if_changed,<name>)"-style line.
func kbuildRecipeCommandName(recipe []string) (string, bool) {
	name := ""
	for _, line := range recipe {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if name != "" {
			// More than one command: not a simple single-generator recipe.
			return "", false
		}
		// "+" forces execution under "make -n"; "$(Q)" is Kbuild's quiet
		// prefix. Neither changes what runs.
		line = strings.TrimSpace(strings.TrimPrefix(line, "+"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "$(Q)"))
		inner, ok := strings.CutPrefix(line, "$(call ")
		if !ok {
			return "", false
		}
		inner, ok = strings.CutSuffix(inner, ")")
		if !ok {
			return "", false
		}
		caller, argument, ok := strings.Cut(inner, ",")
		if !ok {
			return "", false
		}
		switch strings.TrimSpace(caller) {
		case "if_changed", "cmd", "if_changed_dep":
		default:
			return "", false
		}
		argument = strings.TrimSpace(argument)
		if argument == "" || strings.ContainsAny(argument, ",$ ") {
			return "", false
		}
		name = argument
	}
	return name, name != ""
}

// substituteMakeStem replaces GNU make's "%" and "$*" with the matched stem.
func substituteMakeStem(value, stem string) string {
	value = strings.ReplaceAll(value, "%", stem)
	value = strings.ReplaceAll(value, "$(*)", stem)
	return strings.ReplaceAll(value, "$*", stem)
}

func describeRulePositions(matches []ruleMatch) string {
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.rule.Position.String())
	}
	return strings.Join(out, " and ")
}

// isMakeAutomaticVariable reports whether a reference name is one of GNU
// make's automatic variables, which have no Kbuild definition and therefore
// survive expansion verbatim.
func isMakeAutomaticVariable(name string) bool {
	if len(name) != 1 {
		return false
	}
	return strings.ContainsAny(name, "<@*^?+|%")
}

func collapseMakeWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
