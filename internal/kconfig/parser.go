// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type parser struct {
	ctx           context.Context
	opts          Options
	tree          *Tree
	pp            *preprocessor
	rootDir       string
	includeStack  []string
	currentChoice *Menu
}

type sourceLine struct {
	text string
	pos  Position
}

func newParser(ctx context.Context, opts Options) (*parser, error) {
	tree := newTree()
	if opts.MaxIncludeDepth == 0 {
		opts.MaxIncludeDepth = 128
	}
	p := &parser{
		ctx:     ctx,
		opts:    opts,
		tree:    tree,
		rootDir: opts.RootDir,
	}
	p.pp = newPreprocessor(ctx, opts, &tree.Diagnostics)
	return p, nil
}

func (p *parser) parseFile(path string, parent *Menu, from Position) error {
	resolved := p.resolvePath(path)
	if p.rootDir == "" {
		p.rootDir = filepath.Dir(resolved)
	}
	abs, err := filepath.Abs(resolved)
	if err == nil {
		resolved = abs
	}
	for _, active := range p.includeStack {
		if active == resolved {
			return fmt.Errorf("%s: recursive inclusion of %q", from, path)
		}
	}
	if len(p.includeStack) >= p.opts.MaxIncludeDepth {
		return fmt.Errorf("%s: maximum include depth exceeded", from)
	}

	f, err := os.Open(resolved)
	if err != nil {
		return fmt.Errorf("%s: open %q: %w", from, path, err)
	}
	defer f.Close()

	p.includeStack = append(p.includeStack, resolved)
	defer func() {
		p.includeStack = p.includeStack[:len(p.includeStack)-1]
	}()
	return p.parseReader(f, path, parent)
}

func (p *parser) parseReader(r io.Reader, filename string, parent *Menu) error {
	lines, err := readLogicalLines(r, filename)
	if err != nil {
		return err
	}
	_, err = p.parseBlock(lines, 0, parent, nil)
	return err
}

func (p *parser) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if p.rootDir != "" {
		candidate := filepath.Join(p.rootDir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if mapped, ok := mappedSourceRootPath(path, p.opts.SourceRoots); ok {
		return mapped
	}
	if p.rootDir == "" {
		return path
	}
	return filepath.Join(p.rootDir, path)
}

func readLogicalLines(r io.Reader, filename string) ([]sourceLine, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var lines []sourceLine
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		text := scanner.Text()
		pos := Position{Filename: filename, Line: lineNo}
		for strings.HasSuffix(text, "\\") {
			text = strings.TrimSuffix(text, "\\")
			if !scanner.Scan() {
				break
			}
			lineNo++
			text += scanner.Text()
		}
		lines = append(lines, sourceLine{text: text, pos: pos})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func (p *parser) parseBlock(lines []sourceLine, idx int, parent *Menu, end map[string]bool) (int, error) {
	for idx < len(lines) {
		stmt := strings.TrimSpace(stripComment(lines[idx].text))
		if stmt == "" {
			idx++
			continue
		}
		if name, flavor, value, ok := splitAssignment(stmt); ok {
			if err := p.pp.addVariable(name, value, flavor, lines[idx].pos); err != nil {
				return idx, err
			}
			idx++
			continue
		}
		toks, err := tokenize(stmt, lines[idx].pos, p.pp)
		if err != nil {
			return idx, err
		}
		if len(toks) == 0 {
			idx++
			continue
		}
		kw := toks[0].value
		if end != nil && end[kw] {
			return idx + 1, nil
		}

		switch kw {
		case "mainmenu":
			if len(toks) != 2 || !toks[1].quoted {
				return idx, p.parseError(lines[idx].pos, "mainmenu expects one quoted prompt")
			}
			parent.addProperty(&Property{Type: PropertyMenu, Text: toks[1].value, Position: lines[idx].pos})
			idx++
		case "config":
			next, err := p.parseConfig(lines, idx, parent, MenuNormal)
			if err != nil {
				return idx, err
			}
			idx = next
		case "menuconfig":
			next, err := p.parseConfig(lines, idx, parent, MenuMenu)
			if err != nil {
				return idx, err
			}
			idx = next
		case "choice":
			next, err := p.parseChoice(lines, idx, parent)
			if err != nil {
				return idx, err
			}
			idx = next
		case "menu":
			next, err := p.parseMenu(lines, idx, parent)
			if err != nil {
				return idx, err
			}
			idx = next
		case "if":
			next, err := p.parseIf(lines, idx, parent)
			if err != nil {
				return idx, err
			}
			idx = next
		case "comment":
			next, err := p.parseComment(lines, idx, parent)
			if err != nil {
				return idx, err
			}
			idx = next
		case "source":
			if len(toks) != 2 || !toks[1].quoted {
				return idx, p.parseError(lines[idx].pos, "source expects one quoted path")
			}
			p.tree.Sources = append(p.tree.Sources, Source{From: lines[idx].pos, Path: toks[1].value})
			if err := p.parseFile(toks[1].value, parent, lines[idx].pos); err != nil {
				return idx, err
			}
			idx++
		default:
			return idx, p.parseError(lines[idx].pos, "unknown statement %q", kw)
		}
	}
	if end != nil {
		var expected []string
		for token := range end {
			expected = append(expected, token)
		}
		return idx, fmt.Errorf("unexpected end of file, expected one of %s", strings.Join(expected, ", "))
	}
	return idx, nil
}

func (p *parser) parseConfig(lines []sourceLine, idx int, parent *Menu, menuType MenuType) (int, error) {
	toks, err := p.lineTokens(lines[idx])
	if err != nil {
		return idx, err
	}
	if len(toks) != 2 || toks[1].quoted {
		return idx, p.parseError(lines[idx].pos, "%s expects one symbol name", toks[0].value)
	}
	sym := p.tree.symbol(toks[1].value, false)
	entry := &Menu{Type: menuType, Symbol: sym, Position: lines[idx].pos}
	parent.addChild(entry)
	idx++

	idx, err = p.parseEntryOptions(lines, idx, entry, configOption)
	if err != nil {
		return idx, err
	}
	if err := p.checkTransitional(entry); err != nil {
		return idx, err
	}
	if menuType == MenuMenu {
		if entry.Prompt == nil {
			return idx, p.parseError(entry.Position, "menuconfig statement without prompt")
		}
		entry.Prompt.Type = PropertyMenu
	}
	if p.currentChoice != nil {
		if entry.Prompt == nil {
			return idx, p.parseError(entry.Position, "choice member %q must have a prompt", sym.Name)
		}
		if sym.Type != SymbolBool {
			return idx, p.parseError(entry.Position, "choice member %q must be bool", sym.Name)
		}
		sym.Choice = p.currentChoice.Symbol
		p.currentChoice.Symbol.ChoiceMembers = append(p.currentChoice.Symbol.ChoiceMembers, sym)
	}
	return idx, nil
}

func (p *parser) checkTransitional(entry *Menu) error {
	if entry.Symbol == nil || !entry.Symbol.Transitional {
		return nil
	}
	if entry.Symbol.Type == SymbolUnknown {
		return p.parseError(entry.Position, "transitional symbols must have a type")
	}
	if !exprIsYes(entry.Dep) || !exprIsYes(entry.Visibility) {
		return p.parseError(entry.Position, "transitional symbols can only have help sections")
	}
	for _, prop := range entry.Symbol.Properties {
		return p.parseError(prop.Position, "transitional symbols can only have help sections")
	}
	return nil
}

func (p *parser) parseChoice(lines []sourceLine, idx int, parent *Menu) (int, error) {
	toks, err := p.lineTokens(lines[idx])
	if err != nil {
		return idx, err
	}
	if len(toks) != 1 {
		return idx, p.parseError(lines[idx].pos, "choice does not accept arguments")
	}
	sym := p.tree.anonymousSymbol(lines[idx].pos)
	entry := &Menu{Type: MenuChoice, Symbol: sym, Position: lines[idx].pos}
	parent.addChild(entry)
	idx++

	idx, err = p.parseEntryOptions(lines, idx, entry, choiceOption)
	if err != nil {
		return idx, err
	}
	if entry.Prompt == nil {
		return idx, p.parseError(entry.Position, "choice must have a prompt")
	}
	previousChoice := p.currentChoice
	p.currentChoice = entry
	idx, err = p.parseBlock(lines, idx, entry, map[string]bool{"endchoice": true})
	p.currentChoice = previousChoice
	return idx, err
}

func (p *parser) parseMenu(lines []sourceLine, idx int, parent *Menu) (int, error) {
	toks, err := p.lineTokens(lines[idx])
	if err != nil {
		return idx, err
	}
	if len(toks) != 2 || !toks[1].quoted {
		return idx, p.parseError(lines[idx].pos, "menu expects one quoted prompt")
	}
	entry := &Menu{Type: MenuMenu, Position: lines[idx].pos}
	entry.addProperty(&Property{Type: PropertyMenu, Text: toks[1].value, Position: lines[idx].pos})
	parent.addChild(entry)
	idx++

	idx, err = p.parseEntryOptions(lines, idx, entry, menuOption)
	if err != nil {
		return idx, err
	}
	return p.parseBlock(lines, idx, entry, map[string]bool{"endmenu": true})
}

func (p *parser) parseIf(lines []sourceLine, idx int, parent *Menu) (int, error) {
	toks, err := p.lineTokens(lines[idx])
	if err != nil {
		return idx, err
	}
	if len(toks) < 2 {
		return idx, p.parseError(lines[idx].pos, "if expects an expression")
	}
	dep, err := p.parseExpr(toks[1:])
	if err != nil {
		return idx, err
	}
	entry := &Menu{Type: MenuIf, Dep: dep, Position: lines[idx].pos}
	parent.addChild(entry)
	return p.parseBlock(lines, idx+1, entry, map[string]bool{"endif": true})
}

func (p *parser) parseComment(lines []sourceLine, idx int, parent *Menu) (int, error) {
	toks, err := p.lineTokens(lines[idx])
	if err != nil {
		return idx, err
	}
	if len(toks) != 2 || !toks[1].quoted {
		return idx, p.parseError(lines[idx].pos, "comment expects one quoted prompt")
	}
	entry := &Menu{Type: MenuComment, Position: lines[idx].pos}
	entry.addProperty(&Property{Type: PropertyComment, Text: toks[1].value, Position: lines[idx].pos})
	parent.addChild(entry)
	return p.parseEntryOptions(lines, idx+1, entry, commentOption)
}

type optionClass int

const (
	configOption optionClass = iota
	choiceOption
	menuOption
	commentOption
)

func (p *parser) parseEntryOptions(lines []sourceLine, idx int, entry *Menu, class optionClass) (int, error) {
	for idx < len(lines) {
		stmt := strings.TrimSpace(stripComment(lines[idx].text))
		if stmt == "" {
			idx++
			continue
		}
		if name, flavor, value, ok := splitAssignment(stmt); ok {
			if err := p.pp.addVariable(name, value, flavor, lines[idx].pos); err != nil {
				return idx, err
			}
			idx++
			continue
		}
		toks, err := tokenize(stmt, lines[idx].pos, p.pp)
		if err != nil {
			return idx, err
		}
		if len(toks) == 0 {
			idx++
			continue
		}
		if !isOptionKeyword(toks[0].value, class) {
			return idx, nil
		}
		if toks[0].value == "help" {
			help, next := collectHelp(lines, idx+1, visualIndent(lines[idx].text))
			if strings.TrimSpace(help) == "" {
				return idx, p.parseError(lines[idx].pos, "blank help text")
			}
			entry.Help = help
			idx = next
			continue
		}
		if err := p.applyOption(entry, toks, class); err != nil {
			return idx, err
		}
		idx++
	}
	return idx, nil
}

func isOptionKeyword(kw string, class optionClass) bool {
	switch kw {
	case "depends", "help":
		return true
	}
	switch class {
	case configOption:
		switch kw {
		case "bool", "tristate", "int", "hex", "string", "prompt", "default", "def_bool", "def_tristate", "select", "imply", "range", "modules", "transitional":
			return true
		}
	case choiceOption:
		switch kw {
		case "prompt", "default":
			return true
		}
	case menuOption:
		return kw == "visible"
	case commentOption:
		return false
	}
	return false
}

func (p *parser) applyOption(entry *Menu, toks []token, class optionClass) error {
	kw := toks[0].value
	switch kw {
	case "bool", "tristate", "int", "hex", "string":
		if class != configOption {
			return p.parseError(toks[0].pos, "%q is not valid here", kw)
		}
		if err := p.setType(entry, typeForKeyword(kw)); err != nil {
			return err
		}
		if len(toks) > 1 {
			if !toks[1].quoted {
				return p.parseError(toks[1].pos, "%s prompt must be quoted", kw)
			}
			visible, err := p.parseOptionalIf(toks[2:])
			if err != nil {
				return err
			}
			entry.addProperty(&Property{Type: PropertyPrompt, Text: toks[1].value, Visible: visible, Position: toks[1].pos})
		}
	case "prompt":
		if len(toks) < 2 || !toks[1].quoted {
			return p.parseError(toks[0].pos, "prompt expects a quoted string")
		}
		visible, err := p.parseOptionalIf(toks[2:])
		if err != nil {
			return err
		}
		entry.addProperty(&Property{Type: PropertyPrompt, Text: toks[1].value, Visible: visible, Position: toks[0].pos})
	case "default", "def_bool", "def_tristate":
		if len(toks) < 2 {
			return p.parseError(toks[0].pos, "%s expects a value", kw)
		}
		if kw == "def_bool" {
			if err := p.setType(entry, SymbolBool); err != nil {
				return err
			}
		}
		if kw == "def_tristate" {
			if err := p.setType(entry, SymbolTristate); err != nil {
				return err
			}
		}
		valueTokens, ifTokens := splitIf(toks[1:])
		expr, err := p.parseExpr(valueTokens)
		if err != nil {
			return err
		}
		visible, err := p.parseOptionalIf(ifTokens)
		if err != nil {
			return err
		}
		entry.addProperty(&Property{Type: PropertyDefault, Expr: expr, Visible: visible, Position: toks[0].pos})
	case "select", "imply":
		if len(toks) < 2 || toks[1].quoted {
			return p.parseError(toks[0].pos, "%s expects a symbol", kw)
		}
		visible, err := p.parseOptionalIf(toks[2:])
		if err != nil {
			return err
		}
		propType := PropertySelect
		if kw == "imply" {
			propType = PropertyImply
		}
		entry.addProperty(&Property{Type: propType, Expr: symbolExpr(p.tree.symbol(toks[1].value, false)), Visible: visible, Position: toks[0].pos})
	case "range":
		valueTokens, ifTokens := splitIf(toks[1:])
		if len(valueTokens) != 2 {
			return p.parseError(toks[0].pos, "range expects lower and upper symbols")
		}
		left := p.symbolFromToken(valueTokens[0])
		right := p.symbolFromToken(valueTokens[1])
		visible, err := p.parseOptionalIf(ifTokens)
		if err != nil {
			return err
		}
		entry.addProperty(&Property{
			Type:     PropertyRange,
			Expr:     &CompareExpr{Op: "..", Left: symbolExpr(left), Right: symbolExpr(right)},
			Visible:  visible,
			Position: toks[0].pos,
		})
	case "depends":
		if len(toks) < 3 || toks[1].value != "on" {
			return p.parseError(toks[0].pos, "depends expects 'on <expr>'")
		}
		depTokens, ifTokens := splitIf(toks[2:])
		dep, err := p.parseExpr(depTokens)
		if err != nil {
			return err
		}
		cond, err := p.parseOptionalIf(ifTokens)
		if err != nil {
			return err
		}
		entry.Dep = andExpr(entry.Dep, conditionalDep(dep, cond, p.tree.symbol("n", true)))
	case "visible":
		if len(toks) < 3 || toks[1].value != "if" {
			return p.parseError(toks[0].pos, "visible expects 'if <expr>'")
		}
		expr, err := p.parseExpr(toks[2:])
		if err != nil {
			return err
		}
		entry.Visibility = andExpr(entry.Visibility, expr)
	case "modules":
		if entry.Symbol == nil {
			return p.parseError(toks[0].pos, "modules option requires a symbol")
		}
		if p.tree.modulesSym != nil && p.tree.modulesSym != entry.Symbol {
			return p.parseError(toks[0].pos, "modules option already defined by %q", p.tree.modulesSym.Name)
		}
		p.tree.modulesSym = entry.Symbol
	case "transitional":
		if entry.Symbol == nil {
			return p.parseError(toks[0].pos, "transitional option requires a symbol")
		}
		entry.Symbol.Transitional = true
	default:
		return p.parseError(toks[0].pos, "unsupported option %q", kw)
	}
	return nil
}

func (p *parser) setType(entry *Menu, typ SymbolType) error {
	if entry.Symbol == nil {
		return p.parseError(entry.Position, "type option requires a symbol")
	}
	if entry.Symbol.Type == SymbolUnknown {
		entry.Symbol.Type = typ
		return nil
	}
	if entry.Symbol.Type == typ {
		return nil
	}
	p.tree.Diagnostics = append(p.tree.Diagnostics, Diagnostic{
		Position: entry.Position,
		Message:  fmt.Sprintf("ignoring type redefinition of %q from %q to %q", entry.Symbol.Name, entry.Symbol.Type, typ),
	})
	return nil
}

func typeForKeyword(kw string) SymbolType {
	switch kw {
	case "bool":
		return SymbolBool
	case "tristate":
		return SymbolTristate
	case "int":
		return SymbolInt
	case "hex":
		return SymbolHex
	case "string":
		return SymbolString
	default:
		return SymbolUnknown
	}
}

func (p *parser) parseOptionalIf(toks []token) (Expr, error) {
	if len(toks) == 0 {
		return nil, nil
	}
	if toks[0].value != "if" {
		return nil, p.parseError(toks[0].pos, "expected optional if expression")
	}
	if len(toks) == 1 {
		return nil, p.parseError(toks[0].pos, "if expects an expression")
	}
	return p.parseExpr(toks[1:])
}

func splitIf(toks []token) (before []token, ifTokens []token) {
	depth := 0
	for i, tok := range toks {
		switch tok.value {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case "if":
			if depth == 0 {
				return toks[:i], toks[i:]
			}
		}
	}
	return toks, nil
}

func collectHelp(lines []sourceLine, idx int, helpIndent int) (string, int) {
	firstIndent := -1
	var out []string
	for idx < len(lines) {
		raw := lines[idx].text
		if strings.TrimSpace(raw) == "" {
			out = append(out, "")
			idx++
			continue
		}
		indent := visualIndent(raw)
		if indent <= helpIndent && isBlockKeyword(strings.TrimSpace(raw)) {
			break
		}
		if firstIndent == -1 {
			firstIndent = indent
		}
		out = append(out, trimVisualIndent(raw, firstIndent))
		idx++
	}
	return strings.Join(out, "\n"), idx
}

func isBlockKeyword(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "config", "menuconfig", "choice", "source", "rsource", "osource", "orsource", "endchoice", "endmenu", "endif":
		return true
	case "if":
		return len(fields) > 1 && looksLikeKconfigExprStart(fields[1])
	case "menu", "comment":
		return len(fields) > 1 && strings.HasPrefix(fields[1], `"`)
	default:
		return false
	}
}

func looksLikeKconfigExprStart(value string) bool {
	if value == "" {
		return false
	}
	switch value[0] {
	case '!', '(':
		return true
	}
	return (value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= '0' && value[0] <= '9')
}

func visualIndent(s string) int {
	indent := 0
	for _, r := range s {
		switch r {
		case '\t':
			indent = (indent &^ 7) + 8
		case ' ':
			indent++
		default:
			return indent
		}
	}
	return indent
}

func trimVisualIndent(s string, max int) string {
	indent := 0
	for i, r := range s {
		next := indent
		if r == '\t' {
			next = (indent &^ 7) + 8
		} else if r == ' ' {
			next++
		} else {
			return s[i:]
		}
		if next > max {
			return s[i:]
		}
		indent = next
		if indent == max {
			return s[i+len(string(r)):]
		}
	}
	return ""
}

func (p *parser) lineTokens(line sourceLine) ([]token, error) {
	return tokenize(strings.TrimSpace(stripComment(line.text)), line.pos, p.pp)
}

func (p *parser) symbolFromToken(tok token) *Symbol {
	return p.tree.symbol(tok.value, tok.quoted || tok.value == "y" || tok.value == "m" || tok.value == "n" || isNumericLiteral(tok.value))
}

func isNumericLiteral(value string) bool {
	if value == "" {
		return false
	}
	if value[0] == '-' {
		value = value[1:]
	}
	if value == "" {
		return false
	}
	if len(value) > 2 && value[0] == '0' && (value[1] == 'x' || value[1] == 'X') {
		value = value[2:]
		if value == "" {
			return false
		}
		for i := 0; i < len(value); i++ {
			if !isHexDigit(value[i]) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (p *parser) parseError(pos Position, format string, args ...any) error {
	return fmt.Errorf("%s: %s", pos, fmt.Sprintf(format, args...))
}

func (p *parser) finalize() {
	if p.tree.modulesSym == nil {
		p.tree.modulesSym = p.tree.symbol("n", true)
	}
	p.finalizeMenu(p.tree.Root, nil, nil, false)
	p.flattenPromptless(p.tree.Root)
}

func (p *parser) finalizeMenu(menu *Menu, parentDep Expr, parentVisibility Expr, insideChoice bool) {
	menu.Dep = andExpr(parentDep, rewriteMExpr(menu.Dep, p.tree.modulesSym))
	menu.Visibility = rewriteMExpr(menu.Visibility, p.tree.modulesSym)
	for _, prop := range menu.Properties {
		propVisibility := rewriteMExpr(prop.Visible, p.tree.modulesSym)
		if prop.Type == PropertyPrompt || (prop.Type == PropertyMenu && menu.Symbol != nil) {
			propVisibility = andExpr(parentVisibility, propVisibility)
		}
		prop.Visible = andExpr(menu.Dep, propVisibility)
		switch prop.Type {
		case PropertySelect:
			if target := propSymbol(prop); target != nil && menu.Symbol != nil {
				target.RevDep = orExpr(target.RevDep, andExpr(symbolExpr(menu.Symbol), prop.Visible))
			}
		case PropertyImply:
			if target := propSymbol(prop); target != nil && menu.Symbol != nil {
				target.Implied = orExpr(target.Implied, andExpr(symbolExpr(menu.Symbol), prop.Visible))
			}
		}
	}
	if menu.Symbol != nil && menu.Type != MenuChoice {
		menu.Symbol.DirDep = orExpr(menu.Symbol.DirDep, menu.Dep)
	}
	p.createAutomaticSubmenus(menu, insideChoice)
	childInsideChoice := insideChoice || menu.Type == MenuChoice
	childVisibility := parentVisibility
	if menu.Type == MenuMenu && menu.Visibility != nil {
		childVisibility = andExpr(childVisibility, menu.Visibility)
	}
	for _, child := range menu.Children {
		p.finalizeMenu(child, menu.Dep, childVisibility, childInsideChoice)
	}
}

func (p *parser) createAutomaticSubmenus(parent *Menu, insideChoice bool) {
	if insideChoice {
		return
	}
	for i := 0; i < len(parent.Children); i++ {
		base := parent.Children[i]
		if base.Symbol == nil || len(base.Children) != 0 {
			continue
		}
		j := i + 1
		for j < len(parent.Children) {
			candidate := parent.Children[j]
			dep := candidate.Dep
			if candidate.Prompt != nil && candidate.Prompt.Visible != nil {
				dep = candidate.Prompt.Visible
			}
			if !exprImpliesSymbol(dep, base.Symbol) {
				break
			}
			candidate.Parent = base
			base.Children = append(base.Children, candidate)
			j++
		}
		if j > i+1 {
			parent.Children = append(parent.Children[:i+1], parent.Children[j:]...)
		}
	}
}

func (p *parser) flattenPromptless(menu *Menu) {
	for i := 0; i < len(menu.Children); i++ {
		child := menu.Children[i]
		p.flattenPromptless(child)
		if len(child.Children) == 0 || child.Prompt != nil {
			continue
		}
		for _, grandchild := range child.Children {
			grandchild.Parent = menu
		}
		replacement := append([]*Menu{}, child.Children...)
		replacement = append(replacement, menu.Children[i+1:]...)
		menu.Children = append(menu.Children[:i], replacement...)
		i += len(child.Children) - 1
	}
}

func propSymbol(prop *Property) *Symbol {
	expr, ok := prop.Expr.(*SymbolExpr)
	if !ok {
		return nil
	}
	return expr.Symbol
}

type exprParser struct {
	parser *parser
	toks   []token
	pos    int
}

func (p *parser) parseExpr(toks []token) (Expr, error) {
	if len(toks) == 0 {
		return nil, p.parseError(Position{}, "expected expression")
	}
	ep := &exprParser{parser: p, toks: toks}
	expr, err := ep.parseOr()
	if err != nil {
		return nil, err
	}
	if ep.pos != len(toks) {
		return nil, p.parseError(toks[ep.pos].pos, "unexpected token %q in expression", toks[ep.pos].value)
	}
	return expr, nil
}

func (e *exprParser) parseOr() (Expr, error) {
	left, err := e.parseAnd()
	if err != nil {
		return nil, err
	}
	for e.accept("||") {
		right, err := e.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orExpr(left, right)
	}
	return left, nil
}

func (e *exprParser) parseAnd() (Expr, error) {
	left, err := e.parseCompare()
	if err != nil {
		return nil, err
	}
	for e.accept("&&") {
		right, err := e.parseCompare()
		if err != nil {
			return nil, err
		}
		left = andExpr(left, right)
	}
	return left, nil
}

func (e *exprParser) parseCompare() (Expr, error) {
	left, err := e.parseUnary()
	if err != nil {
		return nil, err
	}
	if e.pos >= len(e.toks) || !isCompareOp(e.toks[e.pos].value) {
		return left, nil
	}
	op := e.toks[e.pos].value
	e.pos++
	right, err := e.parseUnary()
	if err != nil {
		return nil, err
	}
	return &CompareExpr{Op: op, Left: left, Right: right}, nil
}

func (e *exprParser) parseUnary() (Expr, error) {
	if e.accept("!") {
		x, err := e.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "!", X: x}, nil
	}
	return e.parsePrimary()
}

func (e *exprParser) parsePrimary() (Expr, error) {
	if e.pos >= len(e.toks) {
		return nil, e.parser.parseError(Position{}, "unexpected end of expression")
	}
	tok := e.toks[e.pos]
	e.pos++
	if tok.value == "(" {
		expr, err := e.parseOr()
		if err != nil {
			return nil, err
		}
		if !e.accept(")") {
			return nil, e.parser.parseError(tok.pos, "missing ')' in expression")
		}
		return expr, nil
	}
	if tok.value == ")" {
		return nil, e.parser.parseError(tok.pos, "unexpected ')' in expression")
	}
	return symbolExpr(e.parser.symbolFromToken(tok)), nil
}

func (e *exprParser) accept(value string) bool {
	if e.pos >= len(e.toks) || e.toks[e.pos].value != value {
		return false
	}
	e.pos++
	return true
}

func isCompareOp(op string) bool {
	switch op {
	case "=", "!=", "<", "<=", ">", ">=":
		return true
	default:
		return false
	}
}
