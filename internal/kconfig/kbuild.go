package kconfig

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type KbuildFile struct {
	Objects          []KbuildObject         `json:"objects"`
	Flags            []KbuildFlag           `json:"flags"`
	RemoveFlags      []KbuildFlag           `json:"remove_flags,omitempty"`
	Directories      []KbuildDir            `json:"directories,omitempty"`
	Generated        []KbuildTarget         `json:"generated,omitempty"`
	Includes         []KbuildInclude        `json:"includes,omitempty"`
	Rules            []KbuildRule           `json:"rules,omitempty"`
	TargetVariables  []KbuildTargetVariable `json:"target_variables,omitempty"`
	objectAssigns    []kbuildObjectAssignment
	compositeMembers []kbuildCompositeMember
	compositeAssigns []kbuildCompositeAssignment
	objectSettings   []kbuildObjectSetting
}

type KbuildObject struct {
	Object    string          `json:"object"`
	Kind      string          `json:"kind,omitempty"`
	Directory string          `json:"directory,omitempty"`
	Condition KbuildCondition `json:"condition"`
	Root      bool            `json:"root,omitempty"`
	Position  Position        `json:"position"`
	order     int
}

type KbuildFlag struct {
	Scope     string          `json:"scope"`
	Object    string          `json:"object,omitempty"`
	Directory string          `json:"directory,omitempty"`
	Recursive bool            `json:"recursive,omitempty"`
	Language  string          `json:"language,omitempty"`
	Flags     []string        `json:"flags"`
	Condition KbuildCondition `json:"condition"`
	Position  Position        `json:"position"`
}

type KbuildDir struct {
	Kind      string          `json:"kind"`
	Directory string          `json:"directory"`
	Root      bool            `json:"root,omitempty"`
	Condition KbuildCondition `json:"condition"`
	Position  Position        `json:"position"`
	order     int
}

type KbuildTarget struct {
	Kind      string          `json:"kind"`
	Target    string          `json:"target"`
	Condition KbuildCondition `json:"condition"`
	Position  Position        `json:"position"`
}

type KbuildInclude struct {
	Path     string   `json:"path"`
	Optional bool     `json:"optional,omitempty"`
	Position Position `json:"position"`
}

type KbuildRule struct {
	Targets       []string `json:"targets"`
	Separator     string   `json:"separator,omitempty"`
	Prerequisites []string `json:"prerequisites,omitempty"`
	OrderOnly     []string `json:"order_only,omitempty"`
	Recipe        []string `json:"recipe,omitempty"`
	Position      Position `json:"position"`
}

type KbuildTargetVariable struct {
	Targets   []string `json:"targets"`
	Variable  string   `json:"variable"`
	Operator  string   `json:"operator"`
	Value     string   `json:"value"`
	Modifiers []string `json:"modifiers,omitempty"`
	Position  Position `json:"position"`
}

type kbuildCompositeMember struct {
	Composite string
	Object    string
	Directory string
	Condition KbuildCondition
	Position  Position
}

type kbuildCompositeAssignment struct {
	Composite string
	Objects   []string
	Directory string
	Operator  string
	Condition KbuildCondition
	Position  Position
}

type kbuildObjectAssignment struct {
	Kind      string
	Objects   []string
	Directory string
	Operator  string
	Condition KbuildCondition
	Root      bool
	Position  Position
	order     int
}

type kbuildObjectSetting struct {
	Name      string
	Object    string
	Directory string
	Value     string
}

type KbuildCondition struct {
	Kind       string            `json:"kind"`
	Symbol     string            `json:"symbol,omitempty"`
	State      string            `json:"state,omitempty"`
	Conditions []KbuildCondition `json:"conditions,omitempty"`
}

type KbuildOptions struct {
	RootDir         string
	RootMakefiles   []string
	SourceRoots     map[string]string
	Variables       map[string]string
	MaxIncludeDepth int
}

func ParseKbuildFile(path string) (*KbuildFile, error) {
	return parseKbuildFile(path, nil)
}

func ParseKbuildFileWithOptions(path string, opts KbuildOptions) (*KbuildFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseKbuild(file, path, opts.Variables, filepath.Dir(path))
}

func parseKbuildFile(path string, vars map[string]string) (*KbuildFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseKbuild(file, path, vars, filepath.Dir(path))
}

func ParseKbuildFileTree(path string, opts KbuildOptions) (*KbuildFile, error) {
	if opts.MaxIncludeDepth == 0 {
		opts.MaxIncludeDepth = 64
	}
	treeParser := &kbuildTreeParser{
		opts:    opts,
		seen:    map[string]bool{},
		parsing: map[string]bool{},
	}
	parser := newKbuildParser(treeParser.variables(), "")
	parser.includeFunc = func(includes []KbuildInclude) error {
		return treeParser.parseIncludes(parser, includes)
	}
	if err := treeParser.parseInto(parser, path, 0); err != nil {
		return nil, err
	}
	if err := parser.finalizeObjectSettings(); err != nil {
		return nil, err
	}
	return parser.kb, nil
}

func ParseKbuildDirectoryTree(path string, opts KbuildOptions) (*KbuildFile, error) {
	rootDir := opts.RootDir
	if rootDir == "" {
		rootDir = filepath.Dir(path)
	}
	parser := &kbuildDirectoryTreeParser{
		opts:    opts,
		rootDir: rootDir,
		cache:   map[string]*KbuildFile{},
		stack:   map[string]bool{},
	}
	return parser.parsePath(path, "", KbuildCondition{Kind: "const", State: "y"}, true)
}

func MergeKbuildFiles(parts ...*KbuildFile) *KbuildFile {
	out := &KbuildFile{}
	for _, part := range parts {
		if part != nil {
			out.merge(part)
		}
	}
	return out
}

func MergeKbuildFileAtDirectory(base *KbuildFile, dir string, extra *KbuildFile) *KbuildFile {
	out := &KbuildFile{}
	out.merge(base)
	if extra == nil {
		return out
	}
	extraAssignments := append([]kbuildObjectAssignment(nil), extra.objectAssigns...)
	out.Objects = append(out.Objects, extra.Objects...)
	out.Flags = append(out.Flags, extra.Flags...)
	out.RemoveFlags = append(out.RemoveFlags, extra.RemoveFlags...)
	out.Directories = append(out.Directories, extra.Directories...)
	out.Generated = append(out.Generated, extra.Generated...)
	out.Includes = append(out.Includes, extra.Includes...)
	out.Rules = append(out.Rules, extra.Rules...)
	out.TargetVariables = append(out.TargetVariables, extra.TargetVariables...)
	out.compositeMembers = append(out.compositeMembers, extra.compositeMembers...)
	out.compositeAssigns = append(out.compositeAssigns, extra.compositeAssigns...)
	out.objectSettings = append(out.objectSettings, extra.objectSettings...)
	if len(extraAssignments) == 0 {
		return out
	}
	insertAt := kbuildDirectoryInsertionIndex(out.objectAssigns, dir)
	assignments := make([]kbuildObjectAssignment, 0, len(out.objectAssigns)+len(extraAssignments))
	assignments = append(assignments, out.objectAssigns[:insertAt]...)
	assignments = append(assignments, extraAssignments...)
	assignments = append(assignments, out.objectAssigns[insertAt:]...)
	out.objectAssigns = assignments
	return out
}

func kbuildDirectoryInsertionIndex(assignments []kbuildObjectAssignment, dir string) int {
	parent := filepath.ToSlash(filepath.Dir(strings.Trim(filepath.ToSlash(dir), "/")))
	if parent == "." {
		parent = ""
	}
	seenParent := false
	insertAt := len(assignments)
	for i, assignment := range assignments {
		if kbuildAssignmentInDirectoryTree(assignment, parent) {
			seenParent = true
			insertAt = i + 1
			continue
		}
		if seenParent {
			return i
		}
	}
	return insertAt
}

func kbuildAssignmentInDirectoryTree(assignment kbuildObjectAssignment, dir string) bool {
	assignmentDir := strings.Trim(filepath.ToSlash(assignment.Directory), "/")
	if dir == "" {
		return assignmentDir == ""
	}
	return assignmentDir == dir || strings.HasPrefix(assignmentDir, dir+"/")
}

func PrefixKbuildFile(kb *KbuildFile, dir string) *KbuildFile {
	return prefixKbuildFile(kb, dir, KbuildCondition{Kind: "const", State: "y"}, true)
}

func ParseKbuild(r io.Reader, filename string) (*KbuildFile, error) {
	return parseKbuild(r, filename, nil, "")
}

func parseKbuild(r io.Reader, filename string, vars map[string]string, baseDir string) (*KbuildFile, error) {
	parser := newKbuildParser(vars, baseDir)
	if err := parser.parseReader(r, filename); err != nil {
		return nil, err
	}
	if err := parser.finalizeObjectSettings(); err != nil {
		return nil, err
	}
	return parser.kb, nil
}

func (p *kbuildParser) finalizeObjectSettings() error {
	names := make([]string, 0, len(p.vars))
	for name := range p.vars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, variable := range names {
		name, object, ok := kbuildObjectSettingName(variable)
		if !ok {
			continue
		}
		value, err := p.expand("$(" + variable + ")")
		if err != nil {
			return err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		p.kb.objectSettings = append(p.kb.objectSettings, kbuildObjectSetting{
			Name:   name,
			Object: object,
			Value:  value,
		})
	}
	for _, variable := range names {
		object, language, ok := perObjectFlagTarget(variable)
		if !ok || language != "c" {
			continue
		}
		value, err := p.expand("$(" + variable + ")")
		if err != nil {
			return err
		}
		for _, sanitizer := range []string{"CFLAGS_KASAN", "CFLAGS_KCSAN"} {
			if !assignmentReferencesVariable(value, sanitizer) {
				continue
			}
			p.kb.objectSettings = append(p.kb.objectSettings, kbuildObjectSetting{
				Name:   sanitizer,
				Object: object,
				Value:  "y",
			})
		}
	}
	return nil
}

func (p *kbuildParser) parseReader(r io.Reader, filename string) error {
	p.appendMakefileList(filename)
	scanner := bufio.NewScanner(r)
	lineNo := 0
	var logical strings.Builder
	logicalStart := 1
	for scanner.Scan() {
		lineNo++
		line := strings.TrimRight(scanner.Text(), " \t")
		if logical.Len() == 0 {
			logicalStart = lineNo
		}
		continued := strings.HasSuffix(line, "\\")
		if continued {
			line = strings.TrimRight(strings.TrimSuffix(line, "\\"), " \t")
		}
		logical.WriteString(line)
		if continued {
			logical.WriteByte(' ')
			continue
		}
		if err := p.parseLine(logical.String(), Position{Filename: filename, Line: logicalStart}); err != nil {
			return err
		}
		logical.Reset()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if logical.Len() != 0 {
		if err := p.parseLine(logical.String(), Position{Filename: filename, Line: logicalStart}); err != nil {
			return err
		}
	}
	if p.defineName != "" {
		return fmt.Errorf("%s: unterminated define %q", p.definePos, p.defineName)
	}
	return nil
}

func (p *kbuildParser) appendMakefileList(filename string) {
	filename = filepath.ToSlash(filename)
	current := ""
	if variable, ok := p.vars["MAKEFILE_LIST"]; ok {
		current = variable.value
	}
	p.vars["MAKEFILE_LIST"] = kbuildVariable{value: appendMakeValue(current, filename)}
}

type kbuildParser struct {
	kb           *KbuildFile
	vars         map[string]kbuildVariable
	locals       []map[string]string
	expanding    map[string]bool
	conds        []kbuildConditionalFrame
	baseDir      string
	currentPos   Position
	defineName   string
	defineOp     string
	definePos    Position
	defineBody   []string
	currentRule  int
	order        int
	includeFunc  func([]KbuildInclude) error
	includeDepth int
}

type kbuildVariable struct {
	value     string
	recursive bool
}

type kbuildConditionalFrame struct {
	parentActive           bool
	parentDefinitelyActive bool
	previousKnown          bool
	previousTaken          bool
	previousCondition      KbuildCondition
	hasPreviousCondition   bool
	active                 bool
	definitelyActive       bool
	condition              KbuildCondition
	hasCondition           bool
	sawElse                bool
}

func newKbuildParser(vars map[string]string, baseDir string) *kbuildParser {
	copied := map[string]kbuildVariable{}
	for key, value := range vars {
		copied[key] = kbuildVariable{value: value}
	}
	return &kbuildParser{
		kb:          &KbuildFile{},
		vars:        copied,
		expanding:   map[string]bool{},
		baseDir:     baseDir,
		currentRule: -1,
	}
}

func (p *kbuildParser) nextOrder() int {
	p.order++
	return p.order
}

func (p *kbuildParser) parseLine(line string, pos Position) error {
	previousPos := p.currentPos
	p.currentPos = pos
	defer func() {
		p.currentPos = previousPos
	}()

	if p.defineName != "" {
		if strings.TrimSpace(stripKbuildComment(line)) == "endef" {
			return p.finishDefine()
		}
		p.defineBody = append(p.defineBody, line)
		return nil
	}

	if strings.HasPrefix(line, "\t") {
		if p.active() && p.currentRule >= 0 {
			p.kb.Rules[p.currentRule].Recipe = append(p.kb.Rules[p.currentRule].Recipe, strings.TrimPrefix(line, "\t"))
			return nil
		}
		line = strings.TrimLeft(line, " \t")
	}
	p.currentRule = -1

	line = stripKbuildComment(line)
	if strings.TrimSpace(line) == "" {
		return nil
	}
	if handled, err := p.parseConditional(line, pos); handled || err != nil {
		return err
	}
	if !p.active() {
		return nil
	}
	if name, op, ok := splitKbuildDefine(line); ok {
		p.defineName = name
		p.defineOp = op
		p.definePos = pos
		p.defineBody = nil
		return nil
	}
	if handled, err := p.parseKbuildInclude(line, pos); handled || err != nil {
		return err
	}
	if handled, err := p.parseVariableDirective(line); handled || err != nil {
		return err
	}
	if lhs, _, _, ok := splitKbuildAssignment(line); ok {
		if strings.Contains(lhs, ":") {
			if handled, err := p.parseRule(line, pos); handled || err != nil {
				return err
			}
		}
		return p.parseAssignment(line, pos)
	}
	if handled, err := p.parseRule(line, pos); handled || err != nil {
		return err
	}
	if containsMakeReference(line) {
		_, err := p.expand(line)
		return err
	}
	return nil
}

func (p *kbuildParser) finishDefine() error {
	body := strings.Join(p.defineBody, "\n")
	expandedBody, err := p.expand(body)
	if err != nil {
		return err
	}
	p.assign(p.defineName, p.defineOp, body, expandedBody)
	p.defineName = ""
	p.defineOp = ""
	p.definePos = Position{}
	p.defineBody = nil
	return nil
}

func (p *kbuildParser) active() bool {
	if len(p.conds) == 0 {
		return true
	}
	return p.conds[len(p.conds)-1].active
}

func (p *kbuildParser) definitelyActive() bool {
	if len(p.conds) == 0 {
		return true
	}
	return p.conds[len(p.conds)-1].definitelyActive
}

func (p *kbuildParser) activeCondition() KbuildCondition {
	conditions := []KbuildCondition{}
	for _, frame := range p.conds {
		if frame.active && frame.hasCondition {
			conditions = append(conditions, frame.condition)
		}
	}
	return combineKbuildConditions(conditions...)
}

func (p *kbuildParser) withActiveCondition(condition KbuildCondition) KbuildCondition {
	conditions := []KbuildCondition{}
	active := p.activeCondition()
	if !active.isEmpty() {
		conditions = append(conditions, active)
	}
	conditions = append(conditions, condition)
	return combineKbuildConditions(conditions...)
}

func (p *kbuildParser) parseConditional(line string, pos Position) (bool, error) {
	line = strings.TrimSpace(line)
	for _, keyword := range []string{"ifeq", "ifneq", "ifdef", "ifndef"} {
		if rest, ok := makeDirectiveRest(line, keyword); ok {
			result := p.evalConditional(keyword, rest)
			p.pushConditional(result)
			return true, nil
		}
	}
	if rest, ok := makeDirectiveRest(line, "else"); ok {
		if len(p.conds) == 0 {
			return true, fmt.Errorf("%s: else without matching if", pos)
		}
		frame := &p.conds[len(p.conds)-1]
		rest = strings.TrimSpace(rest)
		if rest == "" {
			if frame.sawElse {
				return true, fmt.Errorf("%s: duplicate else", pos)
			}
			frame.sawElse = true
			p.activateElse(frame)
			return true, nil
		}
		for _, keyword := range []string{"ifeq", "ifneq", "ifdef", "ifndef"} {
			if nestedRest, ok := makeDirectiveRest(rest, keyword); ok {
				if frame.sawElse {
					return true, fmt.Errorf("%s: else conditional after else", pos)
				}
				p.activateElseIf(frame, keyword, nestedRest)
				return true, nil
			}
		}
		return true, fmt.Errorf("%s: unsupported else directive %q", pos, line)
	}
	if _, ok := makeDirectiveRest(line, "endif"); ok {
		if len(p.conds) == 0 {
			return true, fmt.Errorf("%s: endif without matching if", pos)
		}
		p.conds = p.conds[:len(p.conds)-1]
		return true, nil
	}
	return false, nil
}

func (p *kbuildParser) pushConditional(result kbuildConditionalEval) {
	parentActive := p.active()
	parentDefinitelyActive := p.definitelyActive()
	hasCondition := !result.known && result.hasCondition
	p.conds = append(p.conds, kbuildConditionalFrame{
		parentActive:           parentActive,
		parentDefinitelyActive: parentDefinitelyActive,
		previousKnown:          result.known,
		previousTaken:          result.known && result.value,
		previousCondition:      result.condition,
		hasPreviousCondition:   hasCondition,
		active:                 parentActive && (!result.known || result.value),
		definitelyActive:       parentDefinitelyActive && result.known && result.value,
		condition:              result.condition,
		hasCondition:           hasCondition,
	})
}

func (p *kbuildParser) activateElse(frame *kbuildConditionalFrame) {
	switch {
	case !frame.parentActive:
		frame.active = false
		frame.definitelyActive = false
	case !frame.previousKnown:
		frame.active = true
		frame.definitelyActive = false
		if frame.hasPreviousCondition {
			frame.condition = invertKbuildCondition(frame.previousCondition)
			frame.hasCondition = true
			frame.previousCondition = KbuildCondition{}
			frame.hasPreviousCondition = false
		}
	case frame.previousTaken:
		frame.active = false
		frame.definitelyActive = false
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
	default:
		frame.active = true
		frame.definitelyActive = frame.parentDefinitelyActive
		frame.previousKnown = true
		frame.previousTaken = true
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
	}
}

func (p *kbuildParser) activateElseIf(frame *kbuildConditionalFrame, keyword, rest string) {
	if !frame.parentActive {
		frame.active = false
		frame.definitelyActive = false
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
		return
	}
	if !frame.previousKnown {
		result := p.evalConditional(keyword, rest)
		if result.known && !result.value {
			frame.active = false
			frame.definitelyActive = false
			frame.condition = KbuildCondition{}
			frame.hasCondition = false
			return
		}
		conditions := []KbuildCondition{}
		if frame.hasPreviousCondition {
			conditions = append(conditions, invertKbuildCondition(frame.previousCondition))
		}
		if !result.known && result.hasCondition {
			conditions = append(conditions, result.condition)
		}
		frame.condition = combineKbuildConditions(conditions...)
		frame.hasCondition = len(conditions) != 0
		frame.active = true
		frame.definitelyActive = false
		branchCondition := frame.condition
		if branchCondition.isEmpty() {
			branchCondition = KbuildCondition{Kind: "const", State: "y"}
		}
		frame.previousCondition = combineKbuildAny(frame.previousCondition, branchCondition)
		frame.hasPreviousCondition = true
		return
	}
	if frame.previousTaken {
		frame.active = false
		frame.definitelyActive = false
		frame.condition = KbuildCondition{}
		frame.hasCondition = false
		return
	}
	result := p.evalConditional(keyword, rest)
	frame.previousKnown = result.known
	frame.previousTaken = result.known && result.value
	frame.active = frame.parentActive && (!result.known || result.value)
	frame.definitelyActive = frame.parentDefinitelyActive && result.known && result.value
	frame.condition = result.condition
	frame.hasCondition = !result.known && result.hasCondition
	frame.previousCondition = result.condition
	frame.hasPreviousCondition = !result.known && result.hasCondition
}

func (p *kbuildParser) parseKbuildInclude(line string, pos Position) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false, nil
	}
	optional := false
	switch fields[0] {
	case "include":
	case "-include", "sinclude":
		optional = true
	default:
		return false, nil
	}
	paths, err := p.expandFields(strings.Join(fields[1:], " "))
	if err != nil {
		return true, err
	}
	includes := make([]KbuildInclude, 0, len(paths))
	for _, path := range paths {
		includes = append(includes, KbuildInclude{
			Path:     filepath.ToSlash(path),
			Optional: optional,
			Position: pos,
		})
	}
	p.kb.Includes = append(p.kb.Includes, includes...)
	if p.includeFunc != nil {
		return true, p.includeFunc(includes)
	}
	return true, nil
}

func (p *kbuildParser) parseVariableDirective(line string) (bool, error) {
	if _, _, _, ok := splitKbuildAssignment(line); ok {
		return false, nil
	}

	_, stripped := splitMakeAssignmentModifiers(line)
	if rest, ok := makeDirectiveRest(stripped, "undefine"); ok {
		names, err := p.expandVariableDirectiveNames(rest)
		if err != nil {
			return true, err
		}
		for _, name := range names {
			delete(p.vars, name)
		}
		return true, nil
	}

	if rest, ok := makeDirectiveRest(line, "unexport"); ok {
		_, err := p.expandVariableDirectiveNames(rest)
		return true, err
	}

	if rest, ok := makeDirectiveRest(line, "export"); ok {
		names, err := p.expandVariableDirectiveNames(rest)
		if err != nil {
			return true, err
		}
		for _, name := range names {
			if _, ok := p.vars[name]; !ok {
				p.vars[name] = kbuildVariable{}
			}
		}
		return true, nil
	}

	return false, nil
}

func (p *kbuildParser) expandVariableDirectiveNames(value string) ([]string, error) {
	expanded, err := p.expand(value)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Fields(expanded) {
		if containsMakeReference(name) {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

func (p *kbuildParser) parseAssignment(line string, pos Position) error {
	lhs, op, rhs, ok := splitKbuildAssignment(line)
	if !ok {
		return nil
	}
	rawLHS := strings.TrimSpace(lhs)
	expandedLHS, err := p.expand(lhs)
	if err != nil {
		return err
	}
	if !containsMakeReference(expandedLHS) {
		lhs = strings.TrimSpace(expandedLHS)
	}
	expandedRHS, err := p.expand(rhs)
	if err != nil {
		return err
	}
	p.assign(lhs, op, rhs, expandedRHS)

	values := kbuildFields(expandedRHS)
	if len(values) == 0 {
		return nil
	}

	if language, ok := localKbuildFlagVariable(lhs); ok {
		flagValues := values
		if assignmentReferencesVariable(rhs, lhs) {
			additions, err := p.localKbuildFlagAdditions(lhs, rhs)
			if err != nil {
				return err
			}
			flagValues = additions
		}
		if flags := concreteKbuildFlags(flagValues); len(flags) != 0 {
			p.kb.Flags = append(p.kb.Flags, KbuildFlag{
				Scope:     "global",
				Language:  language,
				Flags:     flags,
				Condition: p.withActiveCondition(KbuildCondition{Kind: "const", State: "y"}),
				Position:  pos,
			})
		}
		return nil
	}

	if kind, cond, ok := collectionCondition(rawLHS); ok {
		return p.parseCollectionAssignment(kind, cond, op, values, pos)
	}
	if kind, cond, ok := collectionCondition(lhs); ok {
		return p.parseCollectionAssignment(kind, cond, op, values, pos)
	}

	if kind, cond, ok := generatedTargetCondition(rawLHS); ok {
		return p.parseGeneratedTargetAssignment(kind, cond, values, pos)
	}
	if kind, cond, ok := generatedTargetCondition(lhs); ok {
		return p.parseGeneratedTargetAssignment(kind, cond, values, pos)
	}

	if recursive, language, cond, ok := globalFlagCondition(rawLHS); ok {
		return p.parseGlobalFlagAssignment(recursive, language, cond, values, pos)
	}
	if recursive, language, cond, ok := globalFlagCondition(lhs); ok {
		return p.parseGlobalFlagAssignment(recursive, language, cond, values, pos)
	}

	if object, language, ok := removeFlagTarget(lhs); ok {
		p.kb.RemoveFlags = append(p.kb.RemoveFlags, KbuildFlag{
			Scope:     "object",
			Object:    object,
			Language:  language,
			Flags:     values,
			Condition: p.withActiveCondition(KbuildCondition{Kind: "const", State: "y"}),
			Position:  pos,
		})
		return nil
	}

	if object, language, ok := perObjectFlagTarget(lhs); ok {
		if language == "c" {
			values = withoutExplicitSanitizerFlagReferences(values)
		}
		if len(values) == 0 {
			return nil
		}
		p.kb.Flags = append(p.kb.Flags, KbuildFlag{
			Scope:     "object",
			Object:    object,
			Language:  language,
			Flags:     values,
			Condition: p.withActiveCondition(KbuildCondition{Kind: "const", State: "y"}),
			Position:  pos,
		})
		return nil
	}

	if composite, cond, ok := compositeMemberCondition(rawLHS); ok {
		return p.parseCompositeMemberAssignment(composite, cond, op, values, pos)
	}
	if composite, cond, ok := compositeMemberCondition(lhs); ok {
		return p.parseCompositeMemberAssignment(composite, cond, op, values, pos)
	}
	return nil
}

func (p *kbuildParser) parseCollectionAssignment(kind string, cond KbuildCondition, op string, values []string, pos Position) error {
	cond = p.withActiveCondition(cond)
	objects := []string{}
	objectOrder := 0
	objectOp := op
	flushObjects := func() {
		if len(objects) == 0 || (kind != "obj" && kind != "lib") {
			return
		}
		p.kb.objectAssigns = append(p.kb.objectAssigns, kbuildObjectAssignment{
			Kind:      kind,
			Objects:   append([]string(nil), objects...),
			Operator:  objectOp,
			Condition: cond,
			Root:      true,
			Position:  pos,
			order:     objectOrder,
		})
		objects = nil
		objectOrder = 0
		if objectOp != "+=" {
			objectOp = "+="
		}
	}
	for _, value := range values {
		order := p.nextOrder()
		object, ok := kbuildObjectToken(value)
		if ok && (kind == "obj" || kind == "lib") {
			if objectOrder == 0 {
				objectOrder = order
			}
			objects = append(objects, object)
			p.kb.Objects = append(p.kb.Objects, KbuildObject{
				Object:    object,
				Kind:      kind,
				Condition: cond,
				Root:      true,
				Position:  pos,
				order:     order,
			})
			continue
		}
		dir, ok := kbuildDirectoryToken(value, kind == "subdir")
		if ok {
			flushObjects()
			p.kb.Directories = append(p.kb.Directories, KbuildDir{
				Kind:      kind,
				Directory: dir,
				Condition: cond,
				Position:  pos,
				order:     order,
			})
			continue
		}
		var root bool
		dir, root, ok = p.kbuildArchiveDirectoryToken(value)
		if ok {
			flushObjects()
			p.kb.Directories = append(p.kb.Directories, KbuildDir{
				Kind:      kind,
				Directory: dir,
				Root:      root,
				Condition: cond,
				Position:  pos,
				order:     order,
			})
		}
	}
	flushObjects()
	return nil
}

func (p *kbuildParser) parseGeneratedTargetAssignment(kind string, cond KbuildCondition, values []string, pos Position) error {
	cond = p.withActiveCondition(cond)
	for _, value := range values {
		target, ok := kbuildGeneratedToken(value)
		if !ok {
			continue
		}
		p.kb.Generated = append(p.kb.Generated, KbuildTarget{
			Kind:      kind,
			Target:    target,
			Condition: cond,
			Position:  pos,
		})
	}
	return nil
}

func (p *kbuildParser) parseGlobalFlagAssignment(recursive bool, language string, cond KbuildCondition, values []string, pos Position) error {
	cond = p.withActiveCondition(cond)
	if flags := concreteKbuildFlags(values); len(flags) != 0 {
		p.kb.Flags = append(p.kb.Flags, KbuildFlag{
			Scope:     "global",
			Recursive: recursive,
			Language:  language,
			Flags:     flags,
			Condition: cond,
			Position:  pos,
		})
	}
	return nil
}

func (p *kbuildParser) parseCompositeMemberAssignment(composite string, cond KbuildCondition, op string, values []string, pos Position) error {
	cond = p.withActiveCondition(cond)
	composite = normalizeCompositeMemberTarget(composite)
	objects := []string{}
	for _, value := range values {
		object, ok := kbuildObjectToken(value)
		if !ok {
			continue
		}
		object = normalizeCompositeMemberObject(composite, object)
		objects = append(objects, object)
		p.kb.compositeMembers = append(p.kb.compositeMembers, kbuildCompositeMember{
			Composite: composite,
			Object:    object,
			Directory: makeDir(object),
			Condition: cond,
			Position:  pos,
		})
	}
	if len(objects) != 0 {
		p.kb.compositeAssigns = append(p.kb.compositeAssigns, kbuildCompositeAssignment{
			Composite: composite,
			Objects:   objects,
			Operator:  op,
			Condition: cond,
			Position:  pos,
		})
	}
	return nil
}

func (p *kbuildParser) localKbuildFlagAdditions(lhs, rhs string) ([]string, error) {
	stripped, err := stripKbuildSelfReferences(rhs, lhs)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(stripped) == "" {
		return nil, nil
	}
	expanded, err := p.expand(stripped)
	if err != nil {
		return nil, err
	}
	return kbuildFields(expanded), nil
}

func stripKbuildSelfReferences(value, variable string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' || i+1 >= len(value) || (value[i+1] != '(' && value[i+1] != '{') {
			out.WriteByte(value[i])
			i++
			continue
		}
		open := i + 1
		end, err := matchingKbuildReference(value, open)
		if err != nil {
			return "", err
		}
		ref := value[i : end+1]
		if makeReferenceMentionsVariable(ref, variable) {
			i = end + 1
			continue
		}
		out.WriteString(ref)
		i = end + 1
	}
	return out.String(), nil
}

func makeReferenceMentionsVariable(ref, variable string) bool {
	return strings.Contains(ref, "$("+variable+")") || strings.Contains(ref, "${"+variable+"}")
}

func (p *kbuildParser) parseRule(line string, pos Position) (bool, error) {
	targetsText, separator, prerequisitesText, inlineRecipe, ok := splitKbuildRule(line)
	if !ok {
		return false, nil
	}
	targets, err := p.expandFields(targetsText)
	if err != nil {
		return true, err
	}
	if len(targets) == 0 {
		return false, nil
	}

	if variable, op, value, modifiers, ok := splitKbuildTargetVariable(prerequisitesText); ok {
		expandedValue, err := p.expand(value)
		if err != nil {
			return true, err
		}
		p.kb.TargetVariables = append(p.kb.TargetVariables, KbuildTargetVariable{
			Targets:   targets,
			Variable:  variable,
			Operator:  op,
			Value:     expandedValue,
			Modifiers: modifiers,
			Position:  pos,
		})
		return true, nil
	}

	prerequisites, orderOnly, err := p.expandPrerequisites(prerequisitesText)
	if err != nil {
		return true, err
	}
	rule := KbuildRule{
		Targets:       targets,
		Separator:     separator,
		Prerequisites: prerequisites,
		OrderOnly:     orderOnly,
		Position:      pos,
	}
	if strings.TrimSpace(inlineRecipe) != "" {
		rule.Recipe = append(rule.Recipe, strings.TrimSpace(inlineRecipe))
	}
	p.kb.Rules = append(p.kb.Rules, rule)
	p.currentRule = len(p.kb.Rules) - 1
	return true, nil
}

func (p *kbuildParser) expandFields(value string) ([]string, error) {
	expanded, err := p.expand(value)
	if err != nil {
		return nil, err
	}
	return strings.Fields(expanded), nil
}

func (p *kbuildParser) expandPrerequisites(value string) ([]string, []string, error) {
	fields, err := p.expandFields(value)
	if err != nil {
		return nil, nil, err
	}
	var prerequisites, orderOnly []string
	current := &prerequisites
	for _, field := range fields {
		if field == "|" {
			current = &orderOnly
			continue
		}
		*current = append(*current, field)
	}
	return prerequisites, orderOnly, nil
}

func (p *kbuildParser) assign(lhs, op, rhs, expandedRHS string) {
	switch op {
	case "+=":
		current, ok := p.vars[lhs]
		switch {
		case !ok:
			p.vars[lhs] = kbuildVariable{value: rhs, recursive: true}
		case current.recursive:
			current.value = appendMakeValue(current.value, rhs)
			p.vars[lhs] = current
		default:
			current.value = appendMakeValue(current.value, expandedRHS)
			p.vars[lhs] = current
		}
	case "?=":
		if _, ok := p.vars[lhs]; !ok {
			p.vars[lhs] = kbuildVariable{value: rhs, recursive: true}
		}
	case "=":
		p.vars[lhs] = kbuildVariable{value: rhs, recursive: true}
	default:
		p.vars[lhs] = kbuildVariable{value: expandedRHS}
	}
}

func appendMakeValue(current, appended string) string {
	if appended == "" {
		return current
	}
	if current == "" {
		return appended
	}
	return current + " " + appended
}

func kbuildFields(value string) []string {
	var fields []string
	var field strings.Builder
	inSingle := false
	inDouble := false
	started := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
			started = true
		case ch == '"' && !inSingle:
			inDouble = !inDouble
			started = true
		case ch == '\\' && i+1 < len(value):
			i++
			field.WriteByte(value[i])
			started = true
		case (ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r') && !inSingle && !inDouble:
			if started {
				fields = append(fields, field.String())
				field.Reset()
				started = false
			}
		default:
			field.WriteByte(ch)
			started = true
		}
	}
	if started {
		fields = append(fields, field.String())
	}
	return fields
}

func (p *kbuildParser) expand(value string) (string, error) {
	return p.expandDepth(value, 0)
}

func (p *kbuildParser) expandDepth(value string, depth int) (string, error) {
	if depth > 100 {
		return "", fmt.Errorf("too deep Kbuild variable expansion")
	}
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' || i+1 >= len(value) {
			out.WriteByte(value[i])
			i++
			continue
		}
		if value[i+1] != '(' && value[i+1] != '{' {
			name := value[i+1 : i+2]
			expanded, ok, err := p.expandVariable(name, "$"+name, depth+1)
			if err != nil {
				return "", err
			}
			if !ok {
				out.WriteByte(value[i])
				out.WriteByte(value[i+1])
			} else {
				out.WriteString(expanded)
			}
			i += 2
			continue
		}
		open := i + 1
		end, err := matchingKbuildReference(value, open)
		if err != nil {
			return "", err
		}
		expanded, err := p.evalReference(value[i:end+1], value[i+2:end], depth+1)
		if err != nil {
			return "", err
		}
		out.WriteString(expanded)
		i = end + 1
	}
	return out.String(), nil
}

func (p *kbuildParser) evalReference(original, clause string, depth int) (string, error) {
	name, args, ok := splitMakeFunction(clause)
	if ok {
		switch name {
		case "call":
			return p.evalCall(args, original, depth)
		case "foreach":
			return p.evalForeach(args, original, depth)
		case "eval":
			return p.evalEval(args, original, depth)
		case "error":
			return p.evalDiagnostic(args, original, depth, true)
		case "if":
			return p.evalIf(args, original, depth)
		case "and":
			return p.evalAnd(args, original, depth)
		case "or":
			return p.evalOr(args, original, depth)
		case "let":
			return p.evalLet(args, original, depth)
		case "origin":
			return p.evalOrigin(args, original, depth)
		case "flavor":
			return p.evalFlavor(args, original, depth)
		case "value":
			return p.evalValue(args, original, depth)
		case "warning", "info":
			return p.evalDiagnostic(args, original, depth, false)
		default:
			for i, arg := range args {
				expanded, err := p.expandDepth(arg, depth)
				if err != nil {
					return "", err
				}
				args[i] = expanded
			}
			return p.evalMakeFunction(name, args, original), nil
		}
	}
	if variable, pattern, replacement, ok := splitMakeSubstitution(clause); ok {
		value, ok, err := p.expandVariable(strings.TrimSpace(variable), original, depth)
		if err != nil {
			return "", err
		}
		if !ok {
			return original, nil
		}
		pattern, patternErr := p.expandDepth(pattern, depth)
		replacement, replacementErr := p.expandDepth(replacement, depth)
		if patternErr != nil {
			return "", patternErr
		}
		if replacementErr != nil {
			return "", replacementErr
		}
		if containsMakeReference(pattern) || containsMakeReference(replacement) {
			return original, nil
		}
		return mapMakeWords(value, func(word string) string {
			return makePatsubst(strings.TrimSpace(pattern), strings.TrimSpace(replacement), word)
		}), nil
	}
	varName := strings.TrimSpace(clause)
	computedName := false
	if containsMakeReference(varName) {
		expandedName, err := p.expandDepth(varName, depth)
		if err != nil {
			return "", err
		}
		if containsMakeReference(expandedName) {
			return original, nil
		}
		computedName = true
		varName = strings.TrimSpace(expandedName)
	}
	value, ok, err := p.expandVariable(varName, original, depth)
	if err != nil {
		return "", err
	}
	if !ok {
		if computedName {
			return "", nil
		}
		return original, nil
	}
	return value, nil
}

func kbuildKnownCall(name string, args []string, original, srcarch string) (string, bool) {
	switch name {
	case "cc-disable-warning":
		if len(args) != 1 {
			return original, true
		}
		warning := strings.Join(strings.Fields(args[0]), "")
		if warning == "" {
			return "", true
		}
		return "-Wno-" + warning, true
	case "cc-option", "as-option", "ld-option":
		if len(args) < 1 || len(args) > 2 {
			return original, true
		}
		option := strings.TrimSpace(args[0])
		if !linuxLLVMKbuildProbeSupportsOption(option, srcarch) {
			if len(args) == 2 {
				return strings.TrimSpace(args[1]), true
			}
			return "", true
		}
		return option, true
	case "cc-option-yn":
		if len(args) != 1 {
			return original, true
		}
		if linuxLLVMKbuildProbeSupportsOption(strings.TrimSpace(args[0]), srcarch) {
			return "y", true
		}
		return "n", true
	default:
		return "", false
	}
}

func linuxLLVMProbeSupportsOption(option string) bool {
	switch option {
	case "", "-fno-code-hoisting", "-fno-conserve-stack", "-fmin-function-alignment=8", "-fsanitize=kernel-memory", "-mrecord-mcount":
		return false
	default:
		return true
	}
}

func linuxLLVMKbuildProbeSupportsOption(option, srcarch string) bool {
	if option == "-mno-outline-atomics" && srcarch != "arm64" {
		return false
	}
	return linuxLLVMProbeSupportsOption(option)
}

func (p *kbuildParser) expandVariable(name, original string, depth int) (string, bool, error) {
	for i := len(p.locals) - 1; i >= 0; i-- {
		value, ok := p.locals[i][name]
		if ok {
			return value, true, nil
		}
	}
	variable, ok := p.vars[name]
	if !ok {
		return "", false, nil
	}
	if !variable.recursive {
		return variable.value, true, nil
	}
	if p.expanding[name] {
		return original, true, nil
	}
	p.expanding[name] = true
	defer delete(p.expanding, name)
	expanded, err := p.expandDepth(variable.value, depth)
	if err != nil {
		return "", false, err
	}
	return expanded, true, nil
}

func (p *kbuildParser) lookupRawVar(name string) (string, bool) {
	for i := len(p.locals) - 1; i >= 0; i-- {
		value, ok := p.locals[i][name]
		if ok {
			return value, true
		}
	}
	variable, ok := p.vars[name]
	if !ok {
		return "", false
	}
	return variable.value, true
}

func (p *kbuildParser) pushLocal(values map[string]string) {
	p.locals = append(p.locals, values)
}

func (p *kbuildParser) popLocal() {
	p.locals = p.locals[:len(p.locals)-1]
}

func (p *kbuildParser) kbuildArchiveDirectoryToken(value string) (string, bool, bool) {
	if strings.ContainsAny(value, "$():=") {
		return "", false, false
	}
	for _, archive := range []string{"lib.a", "built-in.a"} {
		if !strings.HasSuffix(value, "/"+archive) {
			continue
		}
		dir := filepath.ToSlash(filepath.Clean(strings.TrimSuffix(value, archive)))
		rootRelative := false
		for _, rootVar := range []string{"objtree", "srctree"} {
			root, ok := p.lookupRawVar(rootVar)
			if !ok || root == "" {
				continue
			}
			root = filepath.ToSlash(filepath.Clean(root))
			if strings.HasPrefix(dir, root+"/") {
				dir = strings.TrimPrefix(dir, root+"/")
				rootRelative = true
				break
			}
		}
		if strings.HasPrefix(dir, "/") || dir == "." || dir == "" {
			return "", false, false
		}
		return strings.TrimSuffix(dir, "/") + "/", rootRelative, true
	}
	return "", false, false
}

func (p *kbuildParser) evalCall(args []string, original string, depth int) (string, error) {
	if len(args) == 0 {
		return original, nil
	}
	name, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	callArgs := make([]string, 0, len(args)-1)
	for _, arg := range args[1:] {
		expanded, err := p.expandDepth(arg, depth)
		if err != nil {
			return "", err
		}
		callArgs = append(callArgs, expanded)
	}
	srcarch, _ := p.lookupRawVar("SRCARCH")
	if value, ok := kbuildKnownCall(name, callArgs, original, srcarch); ok {
		return value, nil
	}
	body, ok := p.lookupRawVar(name)
	if !ok || name == "" {
		return original, nil
	}
	locals := map[string]string{"0": name}
	for i, expanded := range callArgs {
		locals[fmt.Sprintf("%d", i+1)] = expanded
	}
	p.pushLocal(locals)
	defer p.popLocal()
	return p.expandDepth(body, depth)
}

func (p *kbuildParser) evalForeach(args []string, original string, depth int) (string, error) {
	if len(args) != 3 {
		return original, nil
	}
	name, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	list, err := p.expandDepth(args[1], depth)
	if err != nil {
		return "", err
	}
	if name == "" || containsMakeReference(name) || containsMakeReference(list) {
		return original, nil
	}
	var out []string
	hasMultiline := false
	for _, word := range strings.Fields(list) {
		p.pushLocal(map[string]string{name: word})
		expanded, err := p.expandDepth(args[2], depth)
		p.popLocal()
		if err != nil {
			return "", err
		}
		expanded = strings.TrimSpace(expanded)
		if expanded != "" {
			hasMultiline = hasMultiline || strings.Contains(expanded, "\n")
			out = append(out, expanded)
		}
	}
	if hasMultiline {
		return strings.Join(out, "\n"), nil
	}
	return strings.Join(out, " "), nil
}

func (p *kbuildParser) evalLet(args []string, original string, depth int) (string, error) {
	if len(args) != 3 {
		return original, nil
	}
	valuesText, err := p.expandDepth(args[1], depth)
	if err != nil {
		return "", err
	}
	namesText := strings.TrimSpace(args[0])
	if containsMakeReference(namesText) || containsMakeReference(valuesText) {
		return original, nil
	}
	names := strings.Fields(namesText)
	if len(names) == 0 {
		return original, nil
	}
	values := strings.Fields(valuesText)
	locals := map[string]string{}
	for i, name := range names {
		switch {
		case i == len(names)-1:
			if i < len(values) {
				locals[name] = strings.Join(values[i:], " ")
			} else {
				locals[name] = ""
			}
		case i < len(values):
			locals[name] = values[i]
		default:
			locals[name] = ""
		}
	}
	p.pushLocal(locals)
	defer p.popLocal()
	return p.expandDepth(args[2], depth)
}

func (p *kbuildParser) evalEval(args []string, original string, depth int) (string, error) {
	if len(args) != 1 {
		return original, nil
	}
	expanded, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(expanded, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := p.parseLine(line, p.currentPos); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (p *kbuildParser) evalDiagnostic(args []string, original string, depth int, fatal bool) (string, error) {
	message, err := p.expandDepth(strings.Join(args, ","), depth)
	if err != nil {
		return "", err
	}
	if fatal {
		if !p.definitelyActive() {
			return original, nil
		}
		if strings.TrimSpace(message) == "" {
			message = original
		}
		return "", fmt.Errorf("%s: %s", p.currentPos, strings.TrimSpace(message))
	}
	return "", nil
}

func (p *kbuildParser) evalOrigin(args []string, original string, depth int) (string, error) {
	name, ok, err := p.evalVariableIntrospectionName(args, depth)
	if err != nil || !ok {
		return original, err
	}
	for i := len(p.locals) - 1; i >= 0; i-- {
		if _, ok := p.locals[i][name]; ok {
			return "automatic", nil
		}
	}
	if _, ok := p.vars[name]; ok {
		return "file", nil
	}
	return "undefined", nil
}

func (p *kbuildParser) evalFlavor(args []string, original string, depth int) (string, error) {
	name, ok, err := p.evalVariableIntrospectionName(args, depth)
	if err != nil || !ok {
		return original, err
	}
	for i := len(p.locals) - 1; i >= 0; i-- {
		if _, ok := p.locals[i][name]; ok {
			return "simple", nil
		}
	}
	variable, ok := p.vars[name]
	if !ok {
		return "undefined", nil
	}
	if variable.recursive {
		return "recursive", nil
	}
	return "simple", nil
}

func (p *kbuildParser) evalValue(args []string, original string, depth int) (string, error) {
	name, ok, err := p.evalVariableIntrospectionName(args, depth)
	if err != nil || !ok {
		return original, err
	}
	value, ok := p.lookupRawVar(name)
	if !ok {
		return "", nil
	}
	return value, nil
}

func (p *kbuildParser) evalVariableIntrospectionName(args []string, depth int) (string, bool, error) {
	if len(args) != 1 {
		return "", false, nil
	}
	name, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", false, err
	}
	if containsMakeReference(name) {
		return "", false, nil
	}
	return strings.TrimSpace(name), true, nil
}

func (p *kbuildParser) evalIf(args []string, original string, depth int) (string, error) {
	if len(args) < 2 || len(args) > 3 {
		return original, nil
	}
	condition, err := p.expandDepth(args[0], depth)
	if err != nil {
		return "", err
	}
	if containsMakeReference(condition) {
		return original, nil
	}
	if strings.TrimSpace(condition) != "" {
		return p.expandDepth(args[1], depth)
	}
	if len(args) == 3 {
		return p.expandDepth(args[2], depth)
	}
	return "", nil
}

func (p *kbuildParser) evalAnd(args []string, original string, depth int) (string, error) {
	result := ""
	for i, arg := range args {
		expanded, err := p.expandDepth(arg, depth)
		if err != nil {
			return "", err
		}
		if i < len(args)-1 && containsMakeReference(expanded) {
			return original, nil
		}
		if strings.TrimSpace(expanded) == "" {
			return "", nil
		}
		result = expanded
	}
	return result, nil
}

func (p *kbuildParser) evalOr(args []string, original string, depth int) (string, error) {
	for _, arg := range args {
		expanded, err := p.expandDepth(arg, depth)
		if err != nil {
			return "", err
		}
		if containsMakeReference(expanded) {
			return original, nil
		}
		if strings.TrimSpace(expanded) != "" {
			return expanded, nil
		}
	}
	return "", nil
}

type kbuildTreeParser struct {
	opts    KbuildOptions
	seen    map[string]bool
	parsing map[string]bool
}

func (p *kbuildTreeParser) parseInto(parser *kbuildParser, path string, depth int) error {
	if depth > p.opts.MaxIncludeDepth {
		return fmt.Errorf("%s: maximum Kbuild include depth exceeded", path)
	}
	resolved := p.resolvePath(path, parser.baseDir)
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return err
	}
	if p.parsing[abs] {
		return fmt.Errorf("%s: recursive Kbuild include", path)
	}
	if p.seen[abs] {
		return nil
	}
	file, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer file.Close()

	baseDir := filepath.Dir(abs)
	previousBaseDir := parser.baseDir
	previousDepth := parser.includeDepth
	parser.baseDir = baseDir
	parser.includeDepth = depth
	p.parsing[abs] = true
	err = parser.parseReader(file, abs)
	delete(p.parsing, abs)
	parser.baseDir = previousBaseDir
	parser.includeDepth = previousDepth
	if err != nil {
		return err
	}
	p.seen[abs] = true
	return nil
}

func (p *kbuildTreeParser) parseIncludes(parser *kbuildParser, includes []KbuildInclude) error {
	baseDir := parser.baseDir
	depth := parser.includeDepth + 1
	for _, include := range includes {
		includePath, ok := p.resolveInclude(include.Path, baseDir)
		if !ok {
			continue
		}
		err := p.parseInto(parser, includePath, depth)
		if err != nil {
			if include.Optional && os.IsNotExist(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func (p *kbuildTreeParser) resolvePath(path, baseDir string) string {
	path = p.expand(path)
	if filepath.IsAbs(path) {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if baseDir != "" {
		candidate := filepath.Join(baseDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p.opts.RootDir != "" {
		candidate := filepath.Join(p.opts.RootDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if mapped, ok := mappedSourceRootPath(path, p.opts.SourceRoots); ok {
		return mapped
	}
	if baseDir != "" {
		return filepath.Join(baseDir, path)
	}
	if p.opts.RootDir != "" {
		return filepath.Join(p.opts.RootDir, path)
	}
	return path
}

func (p *kbuildTreeParser) resolveInclude(path, baseDir string) (string, bool) {
	expanded := p.expand(path)
	if strings.Contains(expanded, "$") || expanded == "" {
		return "", false
	}
	return p.resolvePath(expanded, baseDir), true
}

func (p *kbuildTreeParser) expand(value string) string {
	vars := p.variables()
	for key, val := range vars {
		value = strings.ReplaceAll(value, "$("+key+")", val)
		value = strings.ReplaceAll(value, "${"+key+"}", val)
	}
	return value
}

func (p *kbuildTreeParser) variables() map[string]string {
	vars := map[string]string{}
	for key, val := range p.opts.Variables {
		vars[key] = val
	}
	if p.opts.RootDir != "" {
		if _, ok := vars["srctree"]; !ok {
			vars["srctree"] = p.opts.RootDir
		}
	}
	return vars
}

func (kb *KbuildFile) merge(other *KbuildFile) {
	kb.Objects = append(kb.Objects, other.Objects...)
	kb.Flags = append(kb.Flags, other.Flags...)
	kb.RemoveFlags = append(kb.RemoveFlags, other.RemoveFlags...)
	kb.Directories = append(kb.Directories, other.Directories...)
	kb.Generated = append(kb.Generated, other.Generated...)
	kb.Includes = append(kb.Includes, other.Includes...)
	kb.Rules = append(kb.Rules, other.Rules...)
	kb.TargetVariables = append(kb.TargetVariables, other.TargetVariables...)
	kb.objectAssigns = append(kb.objectAssigns, other.objectAssigns...)
	kb.compositeMembers = append(kb.compositeMembers, other.compositeMembers...)
	kb.compositeAssigns = append(kb.compositeAssigns, other.compositeAssigns...)
	kb.objectSettings = append(kb.objectSettings, other.objectSettings...)
}

type kbuildDirectoryTreeParser struct {
	opts    KbuildOptions
	rootDir string
	cache   map[string]*KbuildFile
	stack   map[string]bool
}

func (p *kbuildDirectoryTreeParser) parsePath(path, objectDir string, gate KbuildCondition, linkRoots bool) (*KbuildFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if p.stack[abs] {
		return nil, fmt.Errorf("%s: recursive Kbuild directory descent", path)
	}
	local, ok := p.cache[abs]
	if !ok {
		vars := p.variables(objectDir)
		parsed, err := ParseKbuildFileTree(abs, KbuildOptions{
			RootDir:         p.rootDir,
			Variables:       vars,
			MaxIncludeDepth: p.opts.MaxIncludeDepth,
		})
		if err != nil {
			return nil, err
		}
		local = parsed
		p.cache[abs] = local
	}

	p.stack[abs] = true
	defer delete(p.stack, abs)

	out := prefixKbuildFile(local, objectDir, gate, linkRoots)
	sources := []struct {
		raw       *KbuildFile
		prefixed  *KbuildFile
		objectDir string
	}{{raw: local, prefixed: out, objectDir: objectDir}}
	if objectDir == "" {
		for _, rootMakefile := range p.opts.RootMakefiles {
			rootLocal, err := p.parseRootMakefile(rootMakefile)
			if err != nil {
				return nil, err
			}
			rootPrefixed := prefixKbuildFile(rootLocal, "", gate, linkRoots)
			out.merge(rootPrefixed)
			sources = append(sources, struct {
				raw       *KbuildFile
				prefixed  *KbuildFile
				objectDir string
			}{raw: rootLocal, prefixed: rootPrefixed, objectDir: ""})
		}
	}

	type orderedEvent struct {
		order      int
		assignment *kbuildObjectAssignment
		dir        *KbuildDir
		objectDir  string
		index      int
	}
	var orderedAssignments []kbuildObjectAssignment
	var events []orderedEvent
	for _, source := range sources {
		events = events[:0]
		for i := range source.prefixed.objectAssigns {
			assignment := source.prefixed.objectAssigns[i]
			events = append(events, orderedEvent{
				order:      assignment.order,
				assignment: &assignment,
				index:      len(events),
			})
		}
		for i := range source.raw.Directories {
			dir := source.raw.Directories[i]
			events = append(events, orderedEvent{
				order:     dir.order,
				dir:       &dir,
				objectDir: source.objectDir,
				index:     len(events),
			})
		}
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].order != events[j].order {
				return events[i].order < events[j].order
			}
			return events[i].index < events[j].index
		})
		for _, event := range events {
			if event.assignment != nil {
				orderedAssignments = append(orderedAssignments, *event.assignment)
				continue
			}
			dir := *event.dir
			childDir := filepath.ToSlash(filepath.Clean(filepath.Join(event.objectDir, dir.Directory)))
			if dir.Root {
				childDir = filepath.ToSlash(filepath.Clean(dir.Directory))
			}
			if childDir == "." {
				childDir = ""
			}
			childPath, ok := p.kbuildFileForDir(childDir)
			if !ok {
				continue
			}
			childGate := combineKbuildConditions(gate, dir.Condition)
			childLinkRoots := linkRoots && dir.Kind != "subdir"
			child, err := p.parsePath(childPath, childDir, childGate, childLinkRoots)
			if err != nil {
				return nil, err
			}
			out.merge(child)
			orderedAssignments = append(orderedAssignments, child.objectAssigns...)
		}
	}
	out.objectAssigns = orderedAssignments
	return out, nil
}

func (p *kbuildDirectoryTreeParser) parseRootMakefile(path string) (*KbuildFile, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(p.rootDir, path)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return nil, err
	}
	local, ok := p.cache[abs]
	if ok {
		return local, nil
	}
	vars := p.variables("")
	parsed, err := ParseKbuildFileTree(abs, KbuildOptions{
		RootDir:         p.rootDir,
		Variables:       vars,
		MaxIncludeDepth: p.opts.MaxIncludeDepth,
	})
	if err != nil {
		return nil, err
	}
	p.cache[abs] = parsed
	return parsed, nil
}

func (p *kbuildDirectoryTreeParser) variables(objectDir string) map[string]string {
	vars := map[string]string{}
	for key, value := range p.opts.Variables {
		vars[key] = value
	}
	vars["src"] = filepath.ToSlash(filepath.Join(p.rootDir, objectDir))
	vars["obj"] = objectDir
	if _, ok := vars["objtree"]; !ok {
		vars["objtree"] = p.rootDir
	}
	if _, ok := vars["srctree"]; !ok {
		vars["srctree"] = p.rootDir
	}
	return vars
}

func (p *kbuildDirectoryTreeParser) kbuildFileForDir(dir string) (string, bool) {
	for _, base := range []string{"Kbuild", "Makefile"} {
		path := filepath.Join(p.rootDir, filepath.FromSlash(dir), base)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

func prefixKbuildFile(kb *KbuildFile, dir string, gate KbuildCondition, linkRoots bool) *KbuildFile {
	out := &KbuildFile{}
	for _, object := range kb.Objects {
		object.Directory = dir
		object.Object = prefixKbuildPath(dir, object.Object)
		object.Condition = combineKbuildConditions(gate, object.Condition)
		object.Root = object.Root && linkRoots
		out.Objects = append(out.Objects, object)
	}
	for _, flag := range kb.Flags {
		if flag.Scope == "object" {
			flag.Object = prefixKbuildPath(dir, flag.Object)
		}
		if flag.Scope == "global" && dir != "" {
			flag.Directory = dir
		} else if flag.Scope == "global" {
			flag.Directory = ""
		}
		flag.Condition = combineKbuildConditions(gate, flag.Condition)
		out.Flags = append(out.Flags, flag)
	}
	for _, flag := range kb.RemoveFlags {
		if flag.Scope == "object" {
			flag.Object = prefixKbuildPath(dir, flag.Object)
		}
		if flag.Scope == "global" && dir != "" {
			flag.Directory = dir
		}
		flag.Condition = combineKbuildConditions(gate, flag.Condition)
		out.RemoveFlags = append(out.RemoveFlags, flag)
	}
	for _, target := range kb.Generated {
		target.Target = prefixKbuildPath(dir, target.Target)
		target.Condition = combineKbuildConditions(gate, target.Condition)
		out.Generated = append(out.Generated, target)
	}
	for _, rule := range kb.Rules {
		for i, target := range rule.Targets {
			rule.Targets[i] = prefixKbuildPath(dir, target)
		}
		out.Rules = append(out.Rules, rule)
	}
	for _, variable := range kb.TargetVariables {
		for i, target := range variable.Targets {
			variable.Targets[i] = prefixKbuildPath(dir, target)
		}
		out.TargetVariables = append(out.TargetVariables, variable)
	}
	for _, assignment := range kb.objectAssigns {
		assignment.Directory = dir
		for i, object := range assignment.Objects {
			assignment.Objects[i] = prefixKbuildPath(dir, object)
		}
		assignment.Condition = combineKbuildConditions(gate, assignment.Condition)
		assignment.Root = assignment.Root && linkRoots
		out.objectAssigns = append(out.objectAssigns, assignment)
	}
	for _, member := range kb.compositeMembers {
		member.Directory = dir
		member.Composite = prefixKbuildPath(dir, member.Composite)
		member.Object = prefixKbuildPath(dir, member.Object)
		member.Condition = combineKbuildConditions(gate, member.Condition)
		out.compositeMembers = append(out.compositeMembers, member)
	}
	for _, assignment := range kb.compositeAssigns {
		assignment.Directory = dir
		assignment.Composite = prefixKbuildPath(dir, assignment.Composite)
		for i, object := range assignment.Objects {
			assignment.Objects[i] = prefixKbuildPath(dir, object)
		}
		assignment.Condition = combineKbuildConditions(gate, assignment.Condition)
		out.compositeAssigns = append(out.compositeAssigns, assignment)
	}
	for _, setting := range kb.objectSettings {
		setting.Directory = dir
		if setting.Object != "" {
			setting.Object = prefixKbuildPath(dir, setting.Object)
		}
		out.objectSettings = append(out.objectSettings, setting)
	}
	return out
}

func prefixKbuildPath(dir, path string) string {
	if dir == "" || path == "" || filepath.IsAbs(path) {
		return filepath.ToSlash(path)
	}
	dir = filepath.ToSlash(strings.TrimSuffix(dir, "/"))
	path = filepath.ToSlash(path)
	if path == dir || strings.HasPrefix(path, dir+"/") {
		return path
	}
	return filepath.ToSlash(filepath.Join(dir, path))
}

func stripKbuildComment(line string) string {
	var closers []byte
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && len(closers) == 0 && !makeEscaped(line, i) {
			return line[:i]
		}
		if line[i] == '$' && i+1 < len(line) && !makeEscaped(line, i) {
			switch line[i+1] {
			case '(':
				closers = append(closers, ')')
				i++
				continue
			case '{':
				closers = append(closers, '}')
				i++
				continue
			}
		}
		if len(closers) == 0 {
			continue
		}
		switch {
		case line[i] == '(' && closers[len(closers)-1] == ')':
			closers = append(closers, ')')
		case line[i] == '{' && closers[len(closers)-1] == '}':
			closers = append(closers, '}')
		case line[i] == closers[len(closers)-1]:
			closers = closers[:len(closers)-1]
		}
	}
	return line
}

func makeEscaped(line string, index int) bool {
	backslashes := 0
	for i := index - 1; i >= 0 && line[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func splitKbuildAssignment(line string) (string, string, string, bool) {
	line = stripMakeAssignmentModifiers(line)
	depth := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '(', '{':
			depth++
			continue
		case ')', '}':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		for _, op := range []string{"+=", ":=", "?=", "="} {
			if strings.HasPrefix(line[i:], op) {
				return strings.TrimSpace(line[:i]), op, strings.TrimSpace(line[i+len(op):]), true
			}
		}
	}
	return "", "", "", false
}

func splitKbuildRule(line string) (string, string, string, string, bool) {
	colon := -1
	separator := ""
	depth := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case '&':
			if depth == 0 && i+1 < len(line) && line[i+1] == ':' {
				colon = i
				separator = "&:"
				i = len(line)
			}
		case ':':
			if depth != 0 {
				continue
			}
			if i+1 < len(line) && line[i+1] == '=' {
				return "", "", "", "", false
			}
			colon = i
			separator = ":"
			if i+1 < len(line) && line[i+1] == ':' {
				separator = "::"
			}
			i = len(line)
		}
	}
	if colon < 0 {
		return "", "", "", "", false
	}
	targets := strings.TrimSpace(line[:colon])
	if targets == "" {
		return "", "", "", "", false
	}
	rest := line[colon+len(separator):]
	prerequisites, recipe := splitKbuildInlineRecipe(rest)
	return targets, separator, strings.TrimSpace(prerequisites), recipe, true
}

func splitKbuildInlineRecipe(value string) (string, string) {
	depth := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				return value[:i], value[i+1:]
			}
		}
	}
	return value, ""
}

func splitKbuildTargetVariable(value string) (string, string, string, []string, bool) {
	modifiers, assignment := splitMakeAssignmentModifiers(value)
	lhs, op, rhs, ok := splitKbuildAssignment(assignment)
	if !ok || lhs == "" || strings.ContainsAny(lhs, " \t") {
		return "", "", "", nil, false
	}
	return lhs, op, rhs, modifiers, true
}

func splitKbuildDefine(line string) (string, string, bool) {
	rest, ok := makeDirectiveRest(stripMakeAssignmentModifiers(line), "define")
	if !ok {
		return "", "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", false
	}
	if len(fields) > 2 {
		return "", "", false
	}
	name := fields[0]
	op := "="
	if len(fields) > 1 {
		switch fields[1] {
		case "=", ":=", "+=", "?=":
			op = fields[1]
		default:
			return "", "", false
		}
	}
	return name, op, true
}

func stripMakeAssignmentModifiers(line string) string {
	_, rest := splitMakeAssignmentModifiers(line)
	return rest
}

func splitMakeAssignmentModifiers(line string) ([]string, string) {
	line = strings.TrimSpace(line)
	var modifiers []string
	for {
		stripped := false
		for _, modifier := range []string{"export", "override", "private"} {
			rest, ok := makeDirectiveRest(line, modifier)
			if ok {
				modifiers = append(modifiers, modifier)
				line = rest
				stripped = true
				break
			}
		}
		if !stripped {
			return modifiers, line
		}
	}
}

func makeDirectiveRest(line, keyword string) (string, bool) {
	if line == keyword {
		return "", true
	}
	rest, ok := strings.CutPrefix(line, keyword)
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t' && rest[0] != '(') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

func splitMakeSubstitution(clause string) (string, string, string, bool) {
	colon := -1
	depth := 0
	for i := 0; i < len(clause); i++ {
		switch clause[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				colon = i
				i = len(clause)
			}
		}
	}
	if colon <= 0 {
		return "", "", "", false
	}
	equals := -1
	depth = 0
	for i := colon + 1; i < len(clause); i++ {
		switch clause[i] {
		case '(', '{':
			depth++
		case ')', '}':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 {
				equals = i
				i = len(clause)
			}
		}
	}
	if equals < 0 {
		return "", "", "", false
	}
	return clause[:colon], clause[colon+1 : equals], clause[equals+1:], true
}

type kbuildConditionalEval struct {
	known        bool
	value        bool
	condition    KbuildCondition
	hasCondition bool
}

func (p *kbuildParser) evalConditional(keyword, rest string) kbuildConditionalEval {
	switch keyword {
	case "ifeq", "ifneq":
		left, right, ok := parseMakeConditionArgs(rest)
		if !ok {
			return kbuildConditionalEval{}
		}
		if condition, ok := makeConfigComparisonCondition(keyword, left, right); ok {
			return kbuildConditionalEval{condition: condition, hasCondition: true}
		}
		leftExpanded, leftErr := p.expand(left)
		rightExpanded, rightErr := p.expand(right)
		if leftErr != nil || rightErr != nil || containsMakeReference(leftExpanded) || containsMakeReference(rightExpanded) {
			return kbuildConditionalEval{}
		}
		equal := strings.TrimSpace(leftExpanded) == strings.TrimSpace(rightExpanded)
		if keyword == "ifneq" {
			return kbuildConditionalEval{known: true, value: !equal}
		}
		return kbuildConditionalEval{known: true, value: equal}
	case "ifdef", "ifndef":
		name, err := p.expand(strings.TrimSpace(rest))
		if err != nil || containsMakeReference(name) {
			rawName := strings.TrimSpace(rest)
			if strings.HasPrefix(rawName, "CONFIG_") {
				condition := KbuildCondition{Kind: "config_ne", Symbol: rawName, State: "n"}
				if keyword == "ifndef" {
					condition = invertKbuildCondition(condition)
				}
				return kbuildConditionalEval{condition: condition, hasCondition: true}
			}
			return kbuildConditionalEval{}
		}
		name = strings.TrimSpace(name)
		value, ok := p.lookupRawVar(name)
		if !ok && strings.HasPrefix(name, "CONFIG_") {
			condition := KbuildCondition{Kind: "config_ne", Symbol: name, State: "n"}
			if keyword == "ifndef" {
				condition = invertKbuildCondition(condition)
			}
			return kbuildConditionalEval{condition: condition, hasCondition: true}
		}
		set := ok && strings.TrimSpace(value) != ""
		if keyword == "ifndef" {
			return kbuildConditionalEval{known: true, value: !set}
		}
		return kbuildConditionalEval{known: true, value: set}
	default:
		return kbuildConditionalEval{}
	}
}

func makeConfigComparisonCondition(keyword, left, right string) (KbuildCondition, bool) {
	leftSymbol, leftConfig := unwrapConfigReference(strings.TrimSpace(left))
	rightSymbol, rightConfig := unwrapConfigReference(strings.TrimSpace(right))
	if leftConfig == rightConfig {
		return KbuildCondition{}, false
	}
	symbol := leftSymbol
	state := strings.TrimSpace(right)
	if rightConfig {
		symbol = rightSymbol
		state = strings.TrimSpace(left)
	}
	state = strings.Trim(state, `"'`)
	if state == "" {
		state = "n"
	}
	if state != "y" && state != "m" && state != "n" {
		return KbuildCondition{}, false
	}
	kind := "config_eq"
	if keyword == "ifneq" {
		kind = "config_ne"
	}
	return KbuildCondition{Kind: kind, Symbol: symbol, State: state}, true
}

func parseMakeConditionArgs(rest string) (string, string, bool) {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") {
		end, err := matchingParen(rest, 0)
		if err != nil || strings.TrimSpace(rest[end+1:]) != "" {
			return "", "", false
		}
		args := splitFunctionArgs(rest[1:end])
		if len(args) != 2 {
			return "", "", false
		}
		return unquoteMakeConditionArg(args[0]), unquoteMakeConditionArg(args[1]), true
	}
	left, remaining, ok := readMakeConditionWord(rest)
	if !ok {
		return "", "", false
	}
	right, remaining, ok := readMakeConditionWord(strings.TrimSpace(remaining))
	if !ok || strings.TrimSpace(remaining) != "" {
		return "", "", false
	}
	return left, right, true
}

func readMakeConditionWord(rest string) (string, string, bool) {
	if rest == "" {
		return "", "", false
	}
	if rest[0] == '"' || rest[0] == '\'' {
		quote := rest[0]
		for i := 1; i < len(rest); i++ {
			if rest[i] == quote {
				return rest[1:i], rest[i+1:], true
			}
		}
		return "", "", false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == ' ' || rest[i] == '\t' {
			return rest[:i], rest[i:], true
		}
	}
	return rest, "", true
}

func unquoteMakeConditionArg(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

func containsMakeReference(value string) bool {
	return strings.Contains(value, "$(") || strings.Contains(value, "${")
}

func matchingKbuildReference(in string, open int) (int, error) {
	if in[open] == '(' {
		return matchingParen(in, open)
	}
	depth := 0
	for i := open + 1; i < len(in); i++ {
		switch in[i] {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return i, nil
			}
			depth--
		}
	}
	return 0, fmt.Errorf("unterminated reference %q: missing '}'", in[open:])
}

func splitMakeFunction(clause string) (string, []string, bool) {
	clause = strings.TrimSpace(clause)
	for _, name := range []string{
		"abspath",
		"addprefix",
		"addsuffix",
		"and",
		"basename",
		"call",
		"dir",
		"error",
		"eval",
		"file",
		"filter",
		"filter-out",
		"findstring",
		"foreach",
		"firstword",
		"flavor",
		"if",
		"info",
		"intcmp",
		"join",
		"lastword",
		"let",
		"notdir",
		"or",
		"origin",
		"patsubst",
		"realpath",
		"sort",
		"strip",
		"subst",
		"suffix",
		"value",
		"warning",
		"wildcard",
		"word",
		"wordlist",
		"words",
	} {
		rest, ok := strings.CutPrefix(clause, name)
		if ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			return name, splitFunctionArgs(strings.TrimSpace(rest)), true
		}
	}
	return "", nil, false
}

func (p *kbuildParser) evalMakeFunction(name string, args []string, original string) string {
	switch name {
	case "subst":
		if len(args) != 3 {
			return original
		}
		if containsMakeReference(args[0]) || containsMakeReference(args[1]) {
			return original
		}
		return strings.ReplaceAll(args[2], args[0], args[1])
	}
	if makeArgsContainReference(args) {
		return original
	}
	switch name {
	case "abspath":
		if len(args) != 1 {
			return original
		}
		return mapMakeWords(args[0], p.makeAbsPath)
	case "addprefix":
		if len(args) != 2 {
			return original
		}
		return mapMakeWords(args[1], func(word string) string {
			return strings.TrimSpace(args[0]) + word
		})
	case "addsuffix":
		if len(args) != 2 {
			return original
		}
		return mapMakeWords(args[1], func(word string) string {
			return word + strings.TrimSpace(args[0])
		})
	case "basename":
		if len(args) != 1 {
			return original
		}
		return mapMakeWords(args[0], makeBasename)
	case "dir":
		if len(args) != 1 {
			return original
		}
		return mapMakeWords(args[0], makeDir)
	case "file":
		if len(args) != 1 {
			return original
		}
		return p.makeFile(args[0], original)
	case "filter":
		if len(args) != 2 {
			return original
		}
		return filterMakeWords(args[0], args[1], false)
	case "filter-out":
		if len(args) != 2 {
			return original
		}
		return filterMakeWords(args[0], args[1], true)
	case "findstring":
		if len(args) != 2 {
			return original
		}
		if strings.Contains(args[1], args[0]) {
			return args[0]
		}
		return ""
	case "firstword":
		if len(args) != 1 {
			return original
		}
		words := strings.Fields(args[0])
		if len(words) == 0 {
			return ""
		}
		return words[0]
	case "intcmp":
		if len(args) < 2 || len(args) > 5 {
			return original
		}
		value, ok := makeIntcmp(args)
		if !ok {
			return original
		}
		return value
	case "join":
		if len(args) != 2 {
			return original
		}
		return makeJoin(args[0], args[1])
	case "lastword":
		if len(args) != 1 {
			return original
		}
		words := strings.Fields(args[0])
		if len(words) == 0 {
			return ""
		}
		return words[len(words)-1]
	case "notdir":
		if len(args) != 1 {
			return original
		}
		return mapMakeWords(args[0], makeNotdir)
	case "patsubst":
		if len(args) != 3 {
			return original
		}
		pattern := strings.TrimSpace(args[0])
		replacement := strings.TrimSpace(args[1])
		return mapMakeWords(args[2], func(word string) string {
			return makePatsubst(pattern, replacement, word)
		})
	case "realpath":
		if len(args) != 1 {
			return original
		}
		return mapMakeWordsDropEmpty(args[0], p.makeRealPath)
	case "sort":
		if len(args) != 1 {
			return original
		}
		words := strings.Fields(args[0])
		sort.Strings(words)
		out := words[:0]
		for _, word := range words {
			if len(out) == 0 || out[len(out)-1] != word {
				out = append(out, word)
			}
		}
		return strings.Join(out, " ")
	case "strip":
		if len(args) != 1 {
			return original
		}
		return strings.Join(strings.Fields(args[0]), " ")
	case "suffix":
		if len(args) != 1 {
			return original
		}
		return mapMakeWords(args[0], makeSuffix)
	case "wildcard":
		if len(args) != 1 {
			return original
		}
		return p.expandWildcard(args[0])
	case "word":
		if len(args) != 2 {
			return original
		}
		return makeWord(args[0], args[1])
	case "wordlist":
		if len(args) != 3 {
			return original
		}
		return makeWordList(args[0], args[1], args[2])
	case "words":
		if len(args) != 1 {
			return original
		}
		return fmt.Sprintf("%d", len(strings.Fields(args[0])))
	default:
		return original
	}
}

func (p *kbuildParser) expandWildcard(patterns string) string {
	var out []string
	for _, pattern := range strings.Fields(patterns) {
		matches, relBase := p.glob(pattern)
		sort.Strings(matches)
		for _, match := range matches {
			if relBase != "" {
				if rel, err := filepath.Rel(relBase, match); err == nil {
					match = rel
				}
			}
			out = append(out, filepath.ToSlash(match))
		}
	}
	return strings.Join(out, " ")
}

func (p *kbuildParser) makeAbsPath(word string) string {
	if filepath.IsAbs(word) {
		return filepath.ToSlash(filepath.Clean(word))
	}
	if p.baseDir != "" {
		return filepath.ToSlash(filepath.Clean(filepath.Join(p.baseDir, word)))
	}
	abs, err := filepath.Abs(word)
	if err != nil {
		return filepath.ToSlash(filepath.Clean(word))
	}
	return filepath.ToSlash(abs)
}

func (p *kbuildParser) makeRealPath(word string) string {
	resolved, err := filepath.EvalSymlinks(p.makeAbsPath(word))
	if err != nil {
		return ""
	}
	return filepath.ToSlash(resolved)
}

func (p *kbuildParser) makeFile(arg, original string) string {
	path, ok := strings.CutPrefix(strings.TrimSpace(arg), "<")
	if !ok {
		return original
	}
	path = strings.TrimSpace(path)
	if path == "" || containsMakeReference(path) {
		return ""
	}
	if !filepath.IsAbs(path) && p.baseDir != "" {
		path = filepath.Join(p.baseDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(data), "\n")
}

func (p *kbuildParser) glob(pattern string) ([]string, string) {
	matches, err := filepath.Glob(pattern)
	if err == nil && len(matches) > 0 {
		return matches, ""
	}
	if filepath.IsAbs(pattern) || p.baseDir == "" {
		return nil, ""
	}
	matches, err = filepath.Glob(filepath.Join(p.baseDir, pattern))
	if err != nil {
		return nil, ""
	}
	return matches, p.baseDir
}

func makeArgsContainReference(args []string) bool {
	for _, arg := range args {
		if containsMakeReference(arg) {
			return true
		}
	}
	return false
}

func mapMakeWords(value string, mapWord func(string) string) string {
	words := strings.Fields(value)
	for i, word := range words {
		words[i] = mapWord(word)
	}
	return strings.Join(words, " ")
}

func mapMakeWordsDropEmpty(value string, mapWord func(string) string) string {
	var out []string
	for _, word := range strings.Fields(value) {
		mapped := mapWord(word)
		if mapped != "" {
			out = append(out, mapped)
		}
	}
	return strings.Join(out, " ")
}

func filterMakeWords(patterns string, words string, invert bool) string {
	patternList := strings.Fields(patterns)
	var out []string
	for _, word := range strings.Fields(words) {
		matched := false
		for _, pattern := range patternList {
			if makePatternMatch(pattern, word) {
				matched = true
				break
			}
		}
		if matched != invert {
			out = append(out, word)
		}
	}
	return strings.Join(out, " ")
}

func makePatternMatch(pattern, word string) bool {
	idx := strings.IndexByte(pattern, '%')
	if idx < 0 {
		return pattern == word
	}
	return strings.HasPrefix(word, pattern[:idx]) && strings.HasSuffix(word, pattern[idx+1:])
}

func makePatsubst(pattern, replacement, word string) string {
	idx := strings.IndexByte(pattern, '%')
	if idx < 0 {
		if pattern == word {
			return replacement
		}
		return word
	}
	prefix := pattern[:idx]
	suffix := pattern[idx+1:]
	if !strings.HasPrefix(word, prefix) || !strings.HasSuffix(word, suffix) {
		return word
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(word, prefix), suffix)
	return strings.ReplaceAll(replacement, "%", stem)
}

func makeWord(index string, words string) string {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(index), "%d", &n); err != nil || n < 1 {
		return ""
	}
	fields := strings.Fields(words)
	if n > len(fields) {
		return ""
	}
	return fields[n-1]
}

func makeWordList(start string, end string, words string) string {
	var startIndex, endIndex int
	if _, err := fmt.Sscanf(strings.TrimSpace(start), "%d", &startIndex); err != nil {
		return ""
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(end), "%d", &endIndex); err != nil {
		return ""
	}
	if startIndex < 1 || endIndex < startIndex {
		return ""
	}
	fields := strings.Fields(words)
	if startIndex > len(fields) {
		return ""
	}
	if endIndex > len(fields) {
		endIndex = len(fields)
	}
	return strings.Join(fields[startIndex-1:endIndex], " ")
}

func makeJoin(left, right string) string {
	leftFields := strings.Fields(left)
	rightFields := strings.Fields(right)
	n := len(leftFields)
	if len(rightFields) > n {
		n = len(rightFields)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		value := ""
		if i < len(leftFields) {
			value += leftFields[i]
		}
		if i < len(rightFields) {
			value += rightFields[i]
		}
		out = append(out, value)
	}
	return strings.Join(out, " ")
}

func makeIntcmp(args []string) (string, bool) {
	left, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil {
		return "", false
	}
	right, err := strconv.ParseInt(strings.TrimSpace(args[1]), 10, 64)
	if err != nil {
		return "", false
	}
	lt := ""
	if len(args) >= 3 {
		lt = args[2]
	}
	eq := ""
	if len(args) >= 4 {
		eq = args[3]
	}
	gt := eq
	if len(args) >= 5 {
		gt = args[4]
	}
	switch {
	case left < right:
		return lt, true
	case left > right:
		return gt, true
	default:
		return eq, true
	}
}

func makeBasename(word string) string {
	ext := filepath.Ext(word)
	if ext == "" {
		return word
	}
	return strings.TrimSuffix(word, ext)
}

func makeDir(word string) string {
	idx := strings.LastIndexByte(word, '/')
	if idx < 0 {
		return "./"
	}
	return word[:idx+1]
}

func makeNotdir(word string) string {
	idx := strings.LastIndexByte(word, '/')
	if idx < 0 {
		return word
	}
	return word[idx+1:]
}

func makeSuffix(word string) string {
	return filepath.Ext(word)
}

func collectionCondition(lhs string) (string, KbuildCondition, bool) {
	for _, prefix := range []string{"obj-", "lib-", "subdir-", "core-", "drivers-", "libs-", "net-", "virt-"} {
		if rest, ok := strings.CutPrefix(lhs, prefix); ok {
			if rest == "" && prefix == "subdir-" {
				return strings.TrimSuffix(prefix, "-"), KbuildCondition{Kind: "const", State: "y"}, true
			}
			cond, ok := parseKbuildCondition(rest)
			if !ok {
				return "", KbuildCondition{}, false
			}
			return strings.TrimSuffix(prefix, "-"), cond, true
		}
	}
	return "", KbuildCondition{}, false
}

func generatedTargetCondition(lhs string) (string, KbuildCondition, bool) {
	if lhs == "targets" {
		return "targets", KbuildCondition{Kind: "const", State: "y"}, true
	}
	for _, prefix := range []string{"always-", "extra-", "hostprogs-", "userprogs-", "hostprogs-always-", "userprogs-always-"} {
		if rest, ok := strings.CutPrefix(lhs, prefix); ok {
			cond, ok := parseKbuildCondition(rest)
			if !ok {
				return "", KbuildCondition{}, false
			}
			return strings.TrimSuffix(prefix, "-"), cond, true
		}
	}
	return "", KbuildCondition{}, false
}

func localKbuildFlagVariable(lhs string) (string, bool) {
	switch lhs {
	case "KBUILD_CPPFLAGS":
		return "any", true
	case "KBUILD_CFLAGS", "KBUILD_CFLAGS_KERNEL":
		return "c", true
	case "KBUILD_AFLAGS", "KBUILD_AFLAGS_KERNEL":
		return "asm", true
	default:
		return "", false
	}
}

func assignmentReferencesVariable(rhs, name string) bool {
	return strings.Contains(rhs, "$("+name+")") || strings.Contains(rhs, "${"+name+"}")
}

func concreteKbuildFlags(values []string) []string {
	flags := make([]string, 0, len(values))
	for _, value := range values {
		if containsMakeReference(value) {
			continue
		}
		flags = append(flags, value)
	}
	return flags
}

func globalFlagCondition(lhs string) (bool, string, KbuildCondition, bool) {
	for prefix, language := range map[string]string{
		"asflags-": "asm",
		"ccflags-": "c",
	} {
		if rest, ok := strings.CutPrefix(lhs, prefix); ok {
			cond, ok := parseKbuildCondition(rest)
			return false, language, cond, ok
		}
	}
	for prefix, language := range map[string]string{
		"subdir-asflags-": "asm",
		"subdir-ccflags-": "c",
	} {
		if rest, ok := strings.CutPrefix(lhs, prefix); ok {
			cond, ok := parseKbuildCondition(rest)
			return true, language, cond, ok
		}
	}
	return false, "", KbuildCondition{}, false
}

func parseKbuildCondition(value string) (KbuildCondition, bool) {
	switch value {
	case "y", "m", "-":
		return KbuildCondition{Kind: "const", State: value}, true
	}
	if sym, ok := unwrapConfigReference(value); ok {
		return KbuildCondition{Kind: "config", Symbol: sym}, true
	}
	return KbuildCondition{}, false
}

func unwrapConfigReference(value string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, wrapper := range [][2]string{{"$(", ")"}, {"${", "}"}} {
		if strings.HasPrefix(value, wrapper[0]) && strings.HasSuffix(value, wrapper[1]) {
			inner := strings.TrimSuffix(strings.TrimPrefix(value, wrapper[0]), wrapper[1])
			if strings.HasPrefix(inner, "CONFIG_") && len(inner) > len("CONFIG_") {
				return inner, true
			}
		}
	}
	return "", false
}

func perObjectFlagTarget(lhs string) (string, string, bool) {
	for prefix, language := range map[string]string{
		"AFLAGS_": "asm",
		"CFLAGS_": "c",
	} {
		rest, ok := strings.CutPrefix(lhs, prefix)
		if !ok {
			continue
		}
		object, ok := kbuildObjectToken(rest)
		if !ok {
			return "", "", false
		}
		return object, language, true
	}
	return "", "", false
}

func withoutExplicitSanitizerFlagReferences(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if assignmentReferencesVariable(value, "CFLAGS_KASAN") ||
			assignmentReferencesVariable(value, "CFLAGS_KCSAN") {
			continue
		}
		out = append(out, value)
	}
	return out
}

func removeFlagTarget(lhs string) (string, string, bool) {
	for prefix, language := range map[string]string{
		"AFLAGS_REMOVE_": "asm",
		"CFLAGS_REMOVE_": "c",
	} {
		rest, ok := strings.CutPrefix(lhs, prefix)
		if !ok {
			continue
		}
		object, ok := kbuildObjectToken(rest)
		if !ok {
			return "", "", false
		}
		return object, language, true
	}
	return "", "", false
}

func kbuildObjectSettingName(variable string) (string, string, bool) {
	for _, name := range []string{
		"KASAN_SANITIZE",
		"KCSAN_INSTRUMENT_BARRIERS",
		"KCSAN_SANITIZE",
		"OBJECT_FILES_NON_STANDARD",
		"UBSAN_INTEGER_WRAP",
		"UBSAN_SANITIZE",
		"UBSAN_SIGNED_WRAP",
	} {
		if variable == name {
			return name, "", true
		}
		rest, ok := strings.CutPrefix(variable, name+"_")
		if !ok {
			continue
		}
		object, ok := kbuildObjectToken(rest)
		if !ok {
			return "", "", false
		}
		return name, object, true
	}
	return "", "", false
}

func compositeMemberCondition(lhs string) (string, KbuildCondition, bool) {
	if base, ok := strings.CutSuffix(lhs, "-objs"); ok {
		if base == "" || ignoredCompositeBase(base) {
			return "", KbuildCondition{}, false
		}
		return filepath.ToSlash(base + ".o"), KbuildCondition{Kind: "const", State: "y"}, true
	}
	idx := strings.LastIndexByte(lhs, '-')
	if idx <= 0 {
		return "", KbuildCondition{}, false
	}
	if ignoredCompositeBase(lhs[:idx]) {
		return "", KbuildCondition{}, false
	}
	cond, ok := parseKbuildCondition(lhs[idx+1:])
	if !ok {
		return "", KbuildCondition{}, false
	}
	return filepath.ToSlash(lhs[:idx] + ".o"), cond, true
}

func normalizeCompositeMemberTarget(composite string) string {
	if composite == "hyp-obj.o" {
		return "kvm_nvhe.o"
	}
	return composite
}

func normalizeCompositeMemberObject(composite, object string) string {
	if composite == "kvm_nvhe.o" && strings.HasSuffix(object, ".o") && !strings.HasSuffix(object, ".nvhe.o") {
		return strings.TrimSuffix(object, ".o") + ".nvhe.o"
	}
	return object
}

func kbuildObjectToken(value string) (string, bool) {
	if strings.Contains(value, "$") || !strings.HasSuffix(value, ".o") {
		return "", false
	}
	return filepath.ToSlash(value), true
}

func kbuildDirectoryToken(value string, allowBare bool) (string, bool) {
	if strings.ContainsAny(value, "$():=") {
		if strings.HasSuffix(value, "/") {
			return filepath.ToSlash(value), true
		}
		return "", false
	}
	if strings.HasSuffix(value, "/") {
		return filepath.ToSlash(value), true
	}
	if allowBare && value != "" {
		return filepath.ToSlash(value) + "/", true
	}
	return "", false
}

func kbuildGeneratedToken(value string) (string, bool) {
	if value == "" || strings.Contains(value, "$") || strings.HasSuffix(value, "/") {
		return "", false
	}
	return filepath.ToSlash(value), true
}

func ignoredCompositeBase(base string) bool {
	switch base {
	case "always", "targets", "hostprogs", "userprogs", "extra", "subdir":
		return true
	default:
		return false
	}
}

func (c KbuildCondition) isEmpty() bool {
	return c.Kind == "" && c.Symbol == "" && c.State == "" && len(c.Conditions) == 0
}

func combineKbuildConditions(conditions ...KbuildCondition) KbuildCondition {
	out := make([]KbuildCondition, 0, len(conditions))
	for _, condition := range conditions {
		if condition.isEmpty() {
			continue
		}
		if condition.Kind == "const" && condition.State == "y" {
			continue
		}
		if condition.Kind == "all" {
			out = append(out, condition.Conditions...)
			continue
		}
		out = append(out, condition)
	}
	if len(out) == 0 {
		return KbuildCondition{Kind: "const", State: "y"}
	}
	if len(out) == 1 {
		return out[0]
	}
	return KbuildCondition{Kind: "all", Conditions: out}
}

func combineKbuildAny(conditions ...KbuildCondition) KbuildCondition {
	out := make([]KbuildCondition, 0, len(conditions))
	for _, condition := range conditions {
		if condition.isEmpty() {
			continue
		}
		if condition.Kind == "any" {
			out = append(out, condition.Conditions...)
			continue
		}
		out = append(out, condition)
	}
	if len(out) == 0 {
		return KbuildCondition{}
	}
	if len(out) == 1 {
		return out[0]
	}
	return KbuildCondition{Kind: "any", Conditions: out}
}

func invertKbuildCondition(condition KbuildCondition) KbuildCondition {
	switch condition.Kind {
	case "config_eq":
		condition.Kind = "config_ne"
		return condition
	case "config_ne":
		condition.Kind = "config_eq"
		return condition
	case "const":
		if condition.State == "y" {
			return KbuildCondition{Kind: "const", State: "n"}
		}
		return KbuildCondition{Kind: "const", State: "y"}
	default:
		return KbuildCondition{Kind: "not", Conditions: []KbuildCondition{condition}}
	}
}

func (c KbuildCondition) Refs() []string {
	switch c.Kind {
	case "config", "config_eq", "config_ne":
		if c.Symbol == "" {
			return nil
		}
		return []string{c.Symbol}
	case "all", "any", "not":
		refs := map[string]bool{}
		for _, condition := range c.Conditions {
			for _, ref := range condition.Refs() {
				refs[ref] = true
			}
		}
		return slices.Sorted(maps.Keys(refs))
	default:
		return nil
	}
}

func (c KbuildCondition) Mode(config *ResolvedConfig) string {
	switch c.Kind {
	case "const":
		if c.State == "-" {
			return "n"
		}
		return c.State
	case "config":
		value := kbuildConfigState(config, c.Symbol)
		if value == "y" || value == "m" {
			return value
		}
	case "config_eq":
		if kbuildConfigState(config, c.Symbol) == c.State {
			return "y"
		}
	case "config_ne":
		if kbuildConfigState(config, c.Symbol) != c.State {
			return "y"
		}
	case "all":
		mode := "y"
		for i, condition := range c.Conditions {
			conditionMode := condition.Mode(config)
			if conditionMode == "n" {
				return "n"
			}
			if i == len(c.Conditions)-1 {
				mode = conditionMode
			}
		}
		return mode
	case "any":
		for _, condition := range c.Conditions {
			if condition.Mode(config) != "n" {
				return "y"
			}
		}
	case "not":
		if len(c.Conditions) == 1 && c.Conditions[0].Mode(config) == "n" {
			return "y"
		}
	}
	return "n"
}

func kbuildConfigState(config *ResolvedConfig, symbol string) string {
	if config == nil || !config.ShouldWrite(symbol) {
		return "n"
	}
	value := config.Value(symbol)
	if value == "y" || value == "m" {
		return value
	}
	return "n"
}

func (c KbuildCondition) Enabled(config *ResolvedConfig) bool {
	return c.Mode(config) != "n"
}

func sortedKbuildObjects(objects []KbuildObject) {
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Object != objects[j].Object {
			return objects[i].Object < objects[j].Object
		}
		return objects[i].Position.String() < objects[j].Position.String()
	})
}
