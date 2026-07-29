package kconfig

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const CompactMetadataProtocolV7 = "compact-v7-lazy-action-graph"

const (
	compactV7ProbeDomain             = "linux-compact-v7-kbuild-probe-v1"
	compactV7FlagTerminalDomain      = "linux-compact-v7-kbuild-flag-terminal-v1"
	compactV7FlagNodeDomain          = "linux-compact-v7-kbuild-flag-node-v1"
	compactV7FlagProgramDomain       = "linux-compact-v7-kbuild-flag-program-v1"
	compactV7SourceSetDomain         = "linux-compact-v7-source-set-v1"
	compactV7ActionSourceGroupDomain = "linux-compact-v7-action-source-group-v1"
	compactV7ReachabilityDomain      = "linux-compact-v7-reachability-v1"
	compactV7ActionRecipeDomain      = "linux-compact-v7-action-recipe-v1"
	compactV7ActionRecipeGroupDomain = "linux-compact-v7-action-recipe-group-v1"
	compactV7ObjectDomain            = "linux-compact-v7-object-v1"
)

const (
	compactV7EffectArgv   = "argv"
	compactV7EffectInput  = "input"
	compactV7EffectOutput = "output"
	compactV7EffectGraph  = "graph"
)

var compactV7EffectOrder = map[string]int{
	compactV7EffectArgv:   0,
	compactV7EffectInput:  1,
	compactV7EffectOutput: 2,
	compactV7EffectGraph:  3,
}

// CompactMetadataV7Options selects the isolated compact-v7 metadata protocol.
// The embedded v6 options preserve the existing repository-generation inputs.
type CompactMetadataV7Options struct {
	CompactMetadataOptions
	ToolchainProfileID string
}

type CompactMetadataV7 struct {
	Protocol                string                           `json:"protocol"`
	ToolchainProfile        string                           `json:"toolchain_profile"`
	CompileEnvironmentABI   string                           `json:"compile_environment_abi"`
	Configs                 []CompactConfigV7                `json:"configs"`
	ConfigPayloads          []CompactConfigPayload           `json:"config_payloads"`
	CompileEnvironments     []CompactCompileEnvironment      `json:"compile_environments"`
	GeneratedHeaderFamilies []CompactGeneratedHeaderFamilyV7 `json:"generated_header_families"`
	SourceFiles             []CompactSourceInput             `json:"source_files"`
	SourceSets              []CompactSourceSet               `json:"source_sets"`
	ActionSourceGroups      []CompactActionSourceGroup       `json:"action_source_groups"`
	KbuildProbes            []CompactKbuildProbe             `json:"kbuild_probes"`
	FlagTerminals           []CompactKbuildFlagTerminal      `json:"flag_terminals"`
	FlagNodes               []CompactKbuildFlagNode          `json:"flag_nodes"`
	FlagPrograms            []CompactKbuildFlagProgram       `json:"flag_programs"`
	ReachabilitySignatures  []CompactReachabilitySignature   `json:"reachability_signatures"`
	ActionRecipes           []CompactActionRecipe            `json:"action_recipes"`
	ActionRecipeGroups      []CompactActionRecipeGroup       `json:"action_recipe_groups"`
	ObjectVariants          []CompactObjectVariantV7         `json:"object_variants"`
}

type CompactConfigV7 struct {
	Name                string   `json:"name"`
	ConfigPayload       string   `json:"config_payload"`
	SupportSourceSet    string   `json:"support_source_set"`
	ObjectTargets       []string `json:"object_targets"`
	ModuleObjectTargets []string `json:"module_object_targets,omitempty"`
	imageTarget         string
}

type CompactGeneratedHeaderFamilyV7 struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ConfigPayload string   `json:"config_payload"`
	Labels        []string `json:"labels"`
	Srcarch       string   `json:"srcarch"`
	Dependencies  []string `json:"dependencies,omitempty"`
	SourceSet     string   `json:"source_set,omitempty"`
}

// CompactSourceSet is an interned exact set. Children permit future scanners to
// preserve shared closure structure; the v6 bridge emits flat nodes.
type CompactSourceSet struct {
	ID       string   `json:"id"`
	Files    []int    `json:"files,omitempty"`
	Children []string `json:"children,omitempty"`
}

type CompactActionSourceGroup struct {
	ID            string `json:"id"`
	SourceSet     string `json:"source_set"`
	PrimarySource int    `json:"primary_source"`
}

type CompactKbuildProbe struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	CandidateArgv  []string `json:"candidate_argv"`
	ContextProgram string   `json:"context_program,omitempty"`
	Language       string   `json:"language,omitempty"`
	Srcarch        string   `json:"srcarch,omitempty"`
}

type CompactKbuildFlagTerminal struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
}

type CompactKbuildFlagNode struct {
	ID        string `json:"id"`
	Probe     string `json:"probe"`
	WhenTrue  string `json:"when_true"`
	WhenFalse string `json:"when_false"`
}

type CompactKbuildFlagProgram struct {
	ID      string   `json:"id"`
	Root    string   `json:"root"`
	Effects []string `json:"effects"`
}

type CompactReachabilitySignature struct {
	ID      string   `json:"id"`
	Configs []string `json:"configs"`
}

// CompactActionRecipe describes the reusable action behavior. Object-specific
// sources, outputs, dependencies, and members remain on the object variant.
type CompactActionRecipe struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind"`
	Language          string   `json:"language,omitempty"`
	Mode              string   `json:"mode"`
	FlagProgram       string   `json:"flag_program"`
	RemoveFlagProgram string   `json:"remove_flag_program"`
	ModuleRoot        bool     `json:"module_root,omitempty"`
	ModName           string   `json:"modname,omitempty"`
	ObjtoolArgs       []string `json:"objtool_args,omitempty"`
	ObjtoolDisabled   bool     `json:"objtool_disabled,omitempty"`
	ObjtoolForce      bool     `json:"objtool_force,omitempty"`
}

type CompactActionRecipeGroup struct {
	ID           string   `json:"id"`
	Recipe       string   `json:"recipe"`
	Reachability string   `json:"reachability"`
	Objects      []string `json:"objects"`
}

type CompactObjectVariantV7 struct {
	Target             string   `json:"target"`
	ContentID          string   `json:"content_id"`
	Object             string   `json:"object"`
	Recipe             string   `json:"recipe"`
	RecipeGroup        string   `json:"recipe_group"`
	Reachability       string   `json:"reachability"`
	CompileEnvironment string   `json:"compile_environment,omitempty"`
	ActionSourceGroup  string   `json:"action_source_group,omitempty"`
	Deps               []string `json:"deps,omitempty"`
	Members            []string `json:"members,omitempty"`
}

func compactV7ProbeContentID(probe CompactKbuildProbe) string {
	hasher := newCompactContentHasher(compactV7ProbeDomain)
	hasher.writeValue("kind=", probe.Kind)
	hasher.writeValue("context_program=", probe.ContextProgram)
	hasher.writeValue("language=", probe.Language)
	hasher.writeValue("srcarch=", probe.Srcarch)
	for _, arg := range probe.CandidateArgv {
		hasher.writeValue("candidate_arg=", arg)
	}
	return hasher.id()
}

func compactV7FlagTerminalContentID(argv []string) string {
	hasher := newCompactContentHasher(compactV7FlagTerminalDomain)
	hasher.writeValue("argc=", strconv.Itoa(len(argv)))
	for _, arg := range argv {
		hasher.writeValue("arg=", arg)
	}
	return hasher.id()
}

func compactV7FlagNodeContentID(probe, whenTrue, whenFalse string) string {
	return compactContentID(
		compactV7FlagNodeDomain,
		"probe="+probe,
		"when_true="+whenTrue,
		"when_false="+whenFalse,
	)
}

func compactV7FlagProgramContentID(root string, effects []string) string {
	hasher := newCompactContentHasher(compactV7FlagProgramDomain)
	hasher.writeValue("root=", root)
	for _, effect := range effects {
		hasher.writeValue("effect=", effect)
	}
	return hasher.id()
}

func compactV7SourceSetContentID(files []CompactSourceInput, children []string) string {
	hasher := newCompactContentHasher(compactV7SourceSetDomain)
	for _, input := range files {
		hasher.writeValue("file=", input.Path, "\x00", input.Digest)
	}
	for _, child := range children {
		hasher.writeValue("child=", child)
	}
	return hasher.id()
}

func compactV7ActionSourceGroupContentID(
	sourceSet string,
	primary CompactSourceInput,
) string {
	return compactContentID(
		compactV7ActionSourceGroupDomain,
		"source_set="+sourceSet,
		"primary_path="+primary.Path,
		"primary_digest="+primary.Digest,
	)
}

func compactV7ReachabilityContentID(configs []string) string {
	hasher := newCompactContentHasher(compactV7ReachabilityDomain)
	for _, config := range configs {
		hasher.writeValue("config=", config)
	}
	return hasher.id()
}

func compactV7ActionRecipeContentID(
	recipe CompactActionRecipe,
	toolchainProfile string,
	compileEnvironmentABI string,
) string {
	hasher := newCompactContentHasher(compactV7ActionRecipeDomain)
	hasher.writeValue("toolchain_profile=", toolchainProfile)
	hasher.writeValue("compile_environment_abi=", compileEnvironmentABI)
	hasher.writeValue("kind=", recipe.Kind)
	hasher.writeValue("language=", recipe.Language)
	hasher.writeValue("mode=", recipe.Mode)
	hasher.writeValue("flag_program=", recipe.FlagProgram)
	hasher.writeValue("remove_flag_program=", recipe.RemoveFlagProgram)
	hasher.writeValue("modname=", recipe.ModName)
	if recipe.ModuleRoot {
		hasher.writeValue("module_root=true")
	}
	if recipe.ObjtoolDisabled {
		hasher.writeValue("objtool_disabled=true")
	}
	if recipe.ObjtoolForce {
		hasher.writeValue("objtool_force=true")
	}
	for _, arg := range recipe.ObjtoolArgs {
		hasher.writeValue("objtool_arg=", arg)
	}
	return hasher.id()
}

func compactV7ActionRecipeGroupContentID(
	recipe string,
	reachability string,
	objectContentIDs []string,
) string {
	hasher := newCompactContentHasher(compactV7ActionRecipeGroupDomain)
	hasher.writeValue("recipe=", recipe)
	hasher.writeValue("reachability=", reachability)
	for _, contentID := range objectContentIDs {
		hasher.writeValue("object_content_id=", contentID)
	}
	return hasher.id()
}

func compactV7ObjectContentID(
	object string,
	recipe string,
	compileEnvironment string,
	actionSourceGroup string,
	depContentIDs []string,
	memberContentIDs []string,
	compileEnvironmentABI string,
) string {
	hasher := newCompactContentHasher(compactV7ObjectDomain)
	hasher.writeValue("object=", object)
	hasher.writeValue("recipe=", recipe)
	hasher.writeValue("compile_environment=", compileEnvironment)
	hasher.writeValue("action_source_group=", actionSourceGroup)
	hasher.writeValue("compile_environment_abi=", compileEnvironmentABI)
	for _, contentID := range depContentIDs {
		hasher.writeValue("dep_content_id=", contentID)
	}
	for _, contentID := range memberContentIDs {
		hasher.writeValue("member_content_id=", contentID)
	}
	return hasher.id()
}

func canonicalCompactV7Effects(values ...[]string) ([]string, error) {
	seen := map[string]bool{}
	for _, list := range values {
		for _, value := range list {
			if _, ok := compactV7EffectOrder[value]; !ok {
				return nil, fmt.Errorf("unknown compact-v7 effect %q", value)
			}
			seen[value] = true
		}
	}
	if len(seen) == 0 {
		seen[compactV7EffectArgv] = true
	}
	out := make([]string, 0, len(seen))
	for effect := range seen {
		out = append(out, effect)
	}
	sort.Slice(out, func(i, j int) bool {
		return compactV7EffectOrder[out[i]] < compactV7EffectOrder[out[j]]
	})
	return out, nil
}

func classifyCompactV7FlagEffects(argv []string) []string {
	effects := map[string]bool{compactV7EffectArgv: true}
	add := func(values ...string) {
		for _, value := range values {
			effects[value] = true
		}
	}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "":
			add(compactV7EffectOutput)
		case strings.HasPrefix(arg, "@"):
			add(compactV7EffectInput, compactV7EffectOutput)
		case arg == "-flto" || arg == "-fno-lto" ||
			strings.HasPrefix(arg, "-flto="):
			add(compactV7EffectGraph)
		case compactV7FlagConsumesInputOperand(arg):
			add(compactV7EffectInput)
			if i+1 < len(argv) {
				i++
			}
		case compactV7JoinedInputFlag(arg):
			add(compactV7EffectInput)
		case compactV7FlagConsumesOutputOperand(arg):
			add(compactV7EffectOutput)
			if i+1 < len(argv) {
				i++
			}
		case compactV7JoinedOutputFlag(arg):
			add(compactV7EffectOutput)
		case strings.HasPrefix(arg, "-D") || strings.HasPrefix(arg, "-U") ||
			strings.HasPrefix(arg, "-m") || strings.HasPrefix(arg, "-std=") ||
			strings.HasPrefix(arg, "--target=") || strings.HasPrefix(arg, "-target=") ||
			arg == "-nostdinc" || arg == "-nostdinc++" ||
			arg == "-ffreestanding" || arg == "-fhosted" ||
			strings.HasPrefix(arg, "-fno-builtin"):
			add(compactV7EffectInput)
		case strings.HasPrefix(arg, "-save-temps") ||
			strings.HasPrefix(arg, "-ftime-trace") ||
			strings.HasPrefix(arg, "-fdump-") ||
			strings.HasPrefix(arg, "-fprofile-") ||
			arg == "--coverage" || arg == "-gsplit-dwarf" ||
			strings.HasPrefix(arg, "-Wl,") ||
			strings.HasPrefix(arg, "-Wp,"):
			add(compactV7EffectOutput)
		case compactV7KnownArgvOnlyFlag(arg):
			// The explicit allowlist is deliberately narrower than compiler syntax.
		default:
			add(compactV7EffectOutput)
		}
	}
	out, _ := canonicalCompactV7Effects(mapsKeys(effects))
	return out
}

func compactV7FlagConsumesInputOperand(arg string) bool {
	switch arg {
	case "-D", "-U", "-I", "-isystem", "-iquote", "-idirafter", "-include", "-imacros",
		"--sysroot", "-isysroot", "-x", "-target", "--target", "-march",
		"-mcpu", "-mabi", "-std":
		return true
	default:
		return false
	}
}

func compactV7JoinedInputFlag(arg string) bool {
	for _, prefix := range []string{
		"-I", "-isystem", "-iquote", "-idirafter", "-include", "-imacros",
		"--sysroot=", "-isysroot", "-march=", "-mcpu=", "-mabi=",
	} {
		if arg != prefix && strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

func compactV7FlagConsumesOutputOperand(arg string) bool {
	switch arg {
	case "-o", "-MF", "-MT", "-MQ", "-MJ", "--serialize-diagnostics":
		return true
	default:
		return false
	}
}

func compactV7JoinedOutputFlag(arg string) bool {
	for _, prefix := range []string{"-o", "-MF", "-MT", "-MQ", "-MJ"} {
		if arg != prefix && strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return strings.HasPrefix(arg, "--serialize-diagnostics=")
}

func compactV7KnownArgvOnlyFlag(arg string) bool {
	if strings.HasPrefix(arg, "-W") ||
		strings.HasPrefix(arg, "-O") ||
		strings.HasPrefix(arg, "-g") {
		return true
	}
	switch arg {
	case "-pipe", "-c", "-S", "-E",
		"-fno-stack-protector",
		"-fno-strict-aliasing",
		"-fno-common",
		"-fshort-wchar",
		"-funsigned-char",
		"-fno-delete-null-pointer-checks",
		"-fno-PIE",
		"-fno-pie",
		"-fno-asynchronous-unwind-tables",
		"-fno-unwind-tables",
		"-fno-omit-frame-pointer",
		"-fomit-frame-pointer",
		"-fno-optimize-sibling-calls":
		return true
	default:
		return false
	}
}

func mapsKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out
}

type compactV7ProgramInterner struct {
	terminals map[string]CompactKbuildFlagTerminal
	programs  map[string]CompactKbuildFlagProgram
}

func newCompactV7ProgramInterner() *compactV7ProgramInterner {
	return &compactV7ProgramInterner{
		terminals: map[string]CompactKbuildFlagTerminal{},
		programs:  map[string]CompactKbuildFlagProgram{},
	}
}

func (interner *compactV7ProgramInterner) internLiteral(argv []string) string {
	argv = append([]string{}, argv...)
	terminal := CompactKbuildFlagTerminal{
		ID:   compactV7FlagTerminalContentID(argv),
		Argv: argv,
	}
	interner.terminals[terminal.ID] = terminal
	effects := classifyCompactV7FlagEffects(argv)
	program := CompactKbuildFlagProgram{
		Root:    terminal.ID,
		Effects: effects,
	}
	program.ID = compactV7FlagProgramContentID(program.Root, program.Effects)
	interner.programs[program.ID] = program
	return program.ID
}

func (interner *compactV7ProgramInterner) apply(metadata *CompactMetadataV7) {
	for _, id := range sortedCompactIDs(interner.terminals) {
		metadata.FlagTerminals = append(metadata.FlagTerminals, interner.terminals[id])
	}
	for _, id := range sortedCompactIDs(interner.programs) {
		metadata.FlagPrograms = append(metadata.FlagPrograms, interner.programs[id])
	}
}

type compactV7SourceInterner struct {
	files              []CompactSourceInput
	fileIndices        map[string]int
	sourceSets         map[string]CompactSourceSet
	actionSourceGroups map[string]CompactActionSourceGroup
}

func newCompactV7SourceInterner(files []CompactSourceInput) *compactV7SourceInterner {
	fileIndices := make(map[string]int, len(files))
	for i, input := range files {
		fileIndices[input.Path] = i + 1
	}
	return &compactV7SourceInterner{
		files:              append([]CompactSourceInput(nil), files...),
		fileIndices:        fileIndices,
		sourceSets:         map[string]CompactSourceSet{},
		actionSourceGroups: map[string]CompactActionSourceGroup{},
	}
}

func (interner *compactV7SourceInterner) internFlat(
	inputs []CompactSourceInput,
	context string,
) (string, error) {
	if len(inputs) == 0 {
		return "", nil
	}
	canonical, err := canonicalCompactSourceInputs(inputs, context)
	if err != nil {
		return "", err
	}
	files := make([]int, 0, len(canonical))
	for _, input := range canonical {
		index, ok := interner.fileIndices[input.Path]
		if !ok {
			return "", fmt.Errorf("%s source %q is absent from compact-v7 source files", context, input.Path)
		}
		if interner.files[index-1].Digest != input.Digest {
			return "", fmt.Errorf("%s source %q has conflicting compact-v7 digest", context, input.Path)
		}
		files = append(files, index)
	}
	sourceSet := CompactSourceSet{Files: files}
	sourceSet.ID = compactV7SourceSetContentID(canonical, nil)
	if existing, ok := interner.sourceSets[sourceSet.ID]; ok && !reflect.DeepEqual(existing, sourceSet) {
		return "", fmt.Errorf("compact-v7 source sets collide at %s", sourceSet.ID)
	}
	interner.sourceSets[sourceSet.ID] = sourceSet
	return sourceSet.ID, nil
}

func (interner *compactV7SourceInterner) internActionGroup(
	sourceSet string,
	primaryPath string,
) (string, error) {
	if sourceSet == "" {
		return "", fmt.Errorf("compact-v7 action source group has no source set")
	}
	primaryIndex, ok := interner.fileIndices[primaryPath]
	if !ok {
		return "", fmt.Errorf("compact-v7 primary source %q is absent from source files", primaryPath)
	}
	group := CompactActionSourceGroup{
		SourceSet:     sourceSet,
		PrimarySource: primaryIndex,
	}
	group.ID = compactV7ActionSourceGroupContentID(sourceSet, interner.files[primaryIndex-1])
	if existing, ok := interner.actionSourceGroups[group.ID]; ok && !reflect.DeepEqual(existing, group) {
		return "", fmt.Errorf("compact-v7 action source groups collide at %s", group.ID)
	}
	interner.actionSourceGroups[group.ID] = group
	return group.ID, nil
}

func (interner *compactV7SourceInterner) apply(metadata *CompactMetadataV7) {
	metadata.SourceFiles = append([]CompactSourceInput(nil), interner.files...)
	for _, id := range sortedCompactIDs(interner.sourceSets) {
		metadata.SourceSets = append(metadata.SourceSets, interner.sourceSets[id])
	}
	for _, id := range sortedCompactIDs(interner.actionSourceGroups) {
		metadata.ActionSourceGroups = append(
			metadata.ActionSourceGroups,
			interner.actionSourceGroups[id],
		)
	}
}

func compactV7ActionKind(variant CompactObjectVariant) string {
	if len(variant.Members) == 0 {
		return "compile"
	}
	if isArm64NvheObject(variant.Object) {
		return "arm64_nvhe"
	}
	return "composite"
}

func compactV7SourceLanguage(source string) string {
	switch strings.ToLower(filepathExt(source)) {
	case ".s":
		return "asm"
	default:
		if source == "" {
			return ""
		}
		return "c"
	}
}

func filepathExt(path string) string {
	index := strings.LastIndexByte(path, '.')
	if index < 0 {
		return ""
	}
	return path[index:]
}

func compactV7CheckFullID(kind, id string) error {
	if len(id) != 64 {
		return fmt.Errorf("%s ID %q is not a full SHA-256 digest", kind, id)
	}
	if _, err := hex.DecodeString(id); err != nil {
		return fmt.Errorf("%s ID %q is not hexadecimal: %w", kind, id, err)
	}
	return nil
}

func compactV7SortedUnique(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i-1] >= values[i] {
			return false
		}
	}
	return true
}

func compactV7EqualEffects(left, right []string) bool {
	return reflect.DeepEqual(left, right)
}

func (metadata *CompactMetadataV7) JSON() ([]byte, error) {
	if err := metadata.validate(); err != nil {
		return nil, err
	}
	serializable := *metadata
	initializeCompactV7Collections(&serializable)
	data, err := json.MarshalIndent(&serializable, "", "  ")
	if err != nil {
		return nil, err
	}
	return compactShortStringArrays(append(data, '\n'), 80), nil
}

func initializeCompactV7Collections(metadata *CompactMetadataV7) {
	if metadata.Configs == nil {
		metadata.Configs = []CompactConfigV7{}
	}
	if metadata.ConfigPayloads == nil {
		metadata.ConfigPayloads = []CompactConfigPayload{}
	}
	if metadata.CompileEnvironments == nil {
		metadata.CompileEnvironments = []CompactCompileEnvironment{}
	}
	if metadata.GeneratedHeaderFamilies == nil {
		metadata.GeneratedHeaderFamilies = []CompactGeneratedHeaderFamilyV7{}
	}
	if metadata.SourceFiles == nil {
		metadata.SourceFiles = []CompactSourceInput{}
	}
	if metadata.SourceSets == nil {
		metadata.SourceSets = []CompactSourceSet{}
	}
	if metadata.ActionSourceGroups == nil {
		metadata.ActionSourceGroups = []CompactActionSourceGroup{}
	}
	if metadata.KbuildProbes == nil {
		metadata.KbuildProbes = []CompactKbuildProbe{}
	}
	if metadata.FlagTerminals == nil {
		metadata.FlagTerminals = []CompactKbuildFlagTerminal{}
	}
	if metadata.FlagNodes == nil {
		metadata.FlagNodes = []CompactKbuildFlagNode{}
	}
	if metadata.FlagPrograms == nil {
		metadata.FlagPrograms = []CompactKbuildFlagProgram{}
	}
	if metadata.ReachabilitySignatures == nil {
		metadata.ReachabilitySignatures = []CompactReachabilitySignature{}
	}
	if metadata.ActionRecipes == nil {
		metadata.ActionRecipes = []CompactActionRecipe{}
	}
	if metadata.ActionRecipeGroups == nil {
		metadata.ActionRecipeGroups = []CompactActionRecipeGroup{}
	}
	if metadata.ObjectVariants == nil {
		metadata.ObjectVariants = []CompactObjectVariantV7{}
	}
}
