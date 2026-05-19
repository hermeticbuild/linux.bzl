// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ParseConfig reads a Linux .config file into explicit CONFIG_* assignments.
// Comments, including unset comments, are intentionally ignored; absent symbols
// are resolved from their Kconfig defaults when a config is evaluated.
func ParseConfig(r io.Reader) (map[string]string, error) {
	flags := map[string]string{}
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected CONFIG_* assignment", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !isConfigKey(key) {
			return nil, fmt.Errorf("line %d: expected CONFIG_* key, got %q", lineNo, key)
		}
		if err := setParsedConfig(flags, key, value, lineNo); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return flags, nil
}

func setParsedConfig(flags map[string]string, key, value string, lineNo int) error {
	if _, ok := flags[key]; ok {
		return fmt.Errorf("line %d: duplicate config key %q", lineNo, key)
	}
	flags[key] = value
	return nil
}

func isConfigKey(key string) bool {
	return strings.HasPrefix(key, "CONFIG_") && len(key) > len("CONFIG_")
}

// definedSymbols returns the defined CONFIG_* symbols in sorted order.
// Constants, transitional, and undeclared symbols (those appearing only in
// expressions but never declared via `config X`) are excluded.
func (t *Tree) definedSymbols() []*Symbol {
	symbols := make([]*Symbol, 0, len(t.Symbols))
	for _, sym := range t.Symbols {
		if sym.Const || sym.Transitional || len(sym.Menus) == 0 {
			continue
		}
		symbols = append(symbols, sym)
	}
	sort.Slice(symbols, func(i, j int) bool {
		return symbols[i].Name < symbols[j].Name
	})
	return symbols
}

// ResolvedConfig contains the raw imported .config values plus the effective
// values after Kconfig dependency, select, and imply propagation.
type ResolvedConfig struct {
	Name      string            `json:"name"`
	Raw       map[string]string `json:"raw"`
	Effective map[string]string `json:"effective"`
	Written   map[string]bool   `json:"written"`
}

func (r *ResolvedConfig) Value(key string) string {
	if r == nil {
		return "n"
	}
	if value, ok := r.Effective[key]; ok {
		return value
	}
	return "n"
}

func (r *ResolvedConfig) ShouldWrite(key string) bool {
	if r == nil {
		return false
	}
	if r.Written != nil {
		return r.Written[key]
	}
	value := r.Value(key)
	return value != "" && value != "n"
}

type ResolveConfigOptions struct {
	AllNoConfig bool
}

// ResolveConfig computes effective Kconfig values for one imported .config.
func (t *Tree) ResolveConfig(name string, raw map[string]string) (*ResolvedConfig, error) {
	return t.ResolveConfigWithOptions(name, raw, ResolveConfigOptions{})
}

// ResolveConfigWithOptions computes effective Kconfig values for one imported
// .config with explicit resolver semantics.
func (t *Tree) ResolveConfigWithOptions(name string, raw map[string]string, opts ResolveConfigOptions) (*ResolvedConfig, error) {
	resolver := &configResolver{
		tree:        t,
		rawFlags:    raw,
		rawSet:      map[*Symbol]bool{},
		rawTri:      map[*Symbol]triValue{},
		effective:   map[*Symbol]triValue{},
		values:      map[*Symbol]string{},
		written:     map[*Symbol]bool{},
		allNoConfig: opts.AllNoConfig,
	}
	symbols := t.definedSymbols()
	choices := t.choiceSymbols()
	for _, sym := range symbols {
		key := "CONFIG_" + sym.Name
		value, explicit := raw[key]
		if explicit {
			resolver.rawSet[sym] = true
		}
		switch sym.Type {
		case SymbolBool, SymbolTristate:
			state := configTriValue(sym.Type, value)
			resolver.rawTri[sym] = state
			resolver.effective[sym] = state
			resolver.values[sym] = triName(state)
		default:
			if explicit {
				resolver.values[sym] = value
			} else {
				resolver.values[sym] = ""
			}
		}
	}

	for i := 0; i < len(symbols)*8+8; i++ {
		changed := false
		for _, sym := range symbols {
			if sym.Type == SymbolBool || sym.Type == SymbolTristate {
				continue
			}
			next, write := resolver.scalarValue(sym)
			if resolver.values[sym] != next || resolver.written[sym] != write {
				resolver.values[sym] = next
				resolver.written[sym] = write
				changed = true
			}
		}
		for _, sym := range symbols {
			if sym.Type != SymbolBool && sym.Type != SymbolTristate {
				continue
			}
			if sym.Choice != nil {
				continue
			}
			deps := resolver.evalDepExprDefault(sym.DirDep, triY)
			rawAllowed := minResolvedTri(resolver.baseTri(sym), deps)
			impliedAllowed := triN
			if !resolver.visibleUserValue(sym) {
				impliedAllowed = minResolvedTri(resolver.evalDepExprDefault(sym.Implied, triN), deps)
			}
			selected := resolver.evalDepExprDefault(sym.RevDep, triN)
			next := resolver.clampSymbolTri(sym, maxResolvedTri(maxResolvedTri(rawAllowed, impliedAllowed), selected))
			write := next != triN
			if resolver.effective[sym] != next || resolver.written[sym] != write {
				resolver.effective[sym] = next
				resolver.values[sym] = triName(next)
				resolver.written[sym] = write
				changed = true
			}
		}
		if resolver.applyChoiceSemantics(choices) {
			changed = true
		}
		if !changed {
			return resolver.result(name), nil
		}
	}
	return nil, fmt.Errorf("effective Kconfig values did not converge")
}

type configResolver struct {
	tree        *Tree
	rawFlags    map[string]string
	rawSet      map[*Symbol]bool
	rawTri      map[*Symbol]triValue
	effective   map[*Symbol]triValue
	values      map[*Symbol]string
	written     map[*Symbol]bool
	allNoConfig bool
}

func (t *Tree) choiceSymbols() []*Symbol {
	var choices []*Symbol
	var walk func(*Menu)
	walk = func(menu *Menu) {
		if menu == nil {
			return
		}
		if menu.Type == MenuChoice && menu.Symbol != nil {
			choices = append(choices, menu.Symbol)
		}
		for _, child := range menu.Children {
			walk(child)
		}
	}
	walk(t.Root)
	return choices
}

func (r *configResolver) result(name string) *ResolvedConfig {
	effective := map[string]string{}
	written := map[string]bool{}
	for _, sym := range r.tree.definedSymbols() {
		key := "CONFIG_" + sym.Name
		effective[key] = r.values[sym]
		if r.written[sym] {
			written[key] = true
		}
	}
	raw := map[string]string{}
	for key, value := range r.rawFlags {
		raw[key] = value
	}
	return &ResolvedConfig{
		Name:      name,
		Raw:       raw,
		Effective: effective,
		Written:   written,
	}
}

func (r *configResolver) baseTri(sym *Symbol) triValue {
	if r.rawSet[sym] {
		visible := r.promptVisibility(sym)
		if visible != triN {
			return minResolvedTri(r.rawTri[sym], visible)
		}
	}
	if r.allNoConfig {
		return triN
	}
	return r.defaultTri(sym)
}

func (r *configResolver) visibleUserValue(sym *Symbol) bool {
	return r.rawSet[sym] && r.promptVisibility(sym) != triN
}

func (r *configResolver) applyChoiceSemantics(choices []*Symbol) bool {
	changed := false
	for _, choice := range choices {
		selected := r.choiceSelection(choice)
		for _, member := range choice.ChoiceMembers {
			if member.Type != SymbolBool && member.Type != SymbolTristate {
				continue
			}
			next := triN
			if member == selected {
				next = triY
			}
			write := next != triN
			if r.effective[member] != next || r.written[member] != write {
				r.effective[member] = next
				r.values[member] = triName(next)
				r.written[member] = write
				changed = true
			}
		}
	}
	return changed
}

func (r *configResolver) choiceSelection(choice *Symbol) *Symbol {
	visible := map[*Symbol]bool{}
	for _, member := range choice.ChoiceMembers {
		if r.promptVisibility(member) != triN {
			visible[member] = true
		}
	}
	if len(visible) == 0 {
		return nil
	}

	for _, member := range choice.ChoiceMembers {
		if visible[member] && r.rawSet[member] && r.rawTri[member] == triY {
			return member
		}
	}

	if def := r.choiceDefault(choice, visible); def != nil {
		return def
	}

	if r.allNoConfig {
		return nil
	}

	for _, member := range choice.ChoiceMembers {
		if visible[member] && !r.rawSet[member] {
			return member
		}
	}

	for i := len(choice.ChoiceMembers) - 1; i >= 0; i-- {
		member := choice.ChoiceMembers[i]
		if visible[member] {
			return member
		}
	}
	return nil
}

func (r *configResolver) choiceDefault(choice *Symbol, visible map[*Symbol]bool) *Symbol {
	for _, prop := range choice.Properties {
		if prop.Type != PropertyDefault {
			continue
		}
		if r.evalDepExprDefault(prop.Visible, triY) == triN {
			continue
		}
		member := choiceDefaultMember(prop.Expr)
		if member == nil || !visible[member] {
			continue
		}
		if r.rawSet[member] && r.rawTri[member] == triN {
			return nil
		}
		return member
	}
	return nil
}

func choiceDefaultMember(expr Expr) *Symbol {
	symExpr, ok := expr.(*SymbolExpr)
	if !ok || symExpr.Symbol == nil || symExpr.Symbol.Const {
		return nil
	}
	return symExpr.Symbol
}

func (r *configResolver) promptVisibility(sym *Symbol) triValue {
	visible := triN
	for _, prop := range sym.Properties {
		switch prop.Type {
		case PropertyPrompt, PropertyMenu:
			visible = maxResolvedTri(visible, r.evalDepExprDefault(prop.Visible, triY))
		}
	}
	return r.clampSymbolTri(sym, visible)
}

func (r *configResolver) defaultTri(sym *Symbol) triValue {
	for _, prop := range sym.Properties {
		if prop.Type != PropertyDefault {
			continue
		}
		visible := r.evalDepExprDefault(prop.Visible, triY)
		if visible == triN {
			continue
		}
		value := minResolvedTri(r.evalExprDefault(prop.Expr, triN), visible)
		return r.clampSymbolTri(sym, value)
	}
	return triN
}

func (r *configResolver) scalarValue(sym *Symbol) (string, bool) {
	var value string
	write := false
	if r.rawSet[sym] && r.promptVisibility(sym) != triN {
		value = r.rawFlags["CONFIG_"+sym.Name]
		write = true
	} else {
		for _, prop := range sym.Properties {
			if prop.Type != PropertyDefault {
				continue
			}
			visible := r.evalDepExprDefault(prop.Visible, triY)
			if visible == triN {
				continue
			}
			value = configScalarValue(sym.Type, r.exprValue(prop.Expr))
			write = true
			break
		}
		if !write && r.promptVisibility(sym) != triN {
			write = true
		}
	}
	if value == "" {
		value = defaultScalarValue(sym.Type)
	}
	return r.clampScalarRange(sym, value), write
}

type scalarRangeBound struct {
	value int64
	text  string
}

func (r *configResolver) clampScalarRange(sym *Symbol, value string) string {
	if sym.Type != SymbolInt && sym.Type != SymbolHex {
		return value
	}
	parsed, ok := parseScalarNumber(sym.Type, value)
	if !ok {
		value = defaultScalarValue(sym.Type)
		parsed, ok = parseScalarNumber(sym.Type, value)
		if !ok {
			return value
		}
	}
	lo, hi, ok := r.visibleScalarRange(sym)
	if !ok {
		return normalizeScalarValue(sym.Type, value)
	}
	if parsed < lo.value {
		return normalizeScalarValue(sym.Type, lo.text)
	}
	if parsed > hi.value {
		return normalizeScalarValue(sym.Type, hi.text)
	}
	return normalizeScalarValue(sym.Type, value)
}

func (r *configResolver) visibleScalarRange(sym *Symbol) (scalarRangeBound, scalarRangeBound, bool) {
	for _, prop := range sym.Properties {
		if prop.Type != PropertyRange {
			continue
		}
		if r.evalDepExprDefault(prop.Visible, triY) == triN {
			continue
		}
		expr, ok := prop.Expr.(*CompareExpr)
		if !ok || expr.Op != ".." {
			continue
		}
		lo, loOK := r.scalarRangeBound(sym.Type, expr.Left)
		hi, hiOK := r.scalarRangeBound(sym.Type, expr.Right)
		if loOK && hiOK {
			return lo, hi, true
		}
	}
	return scalarRangeBound{}, scalarRangeBound{}, false
}

func (r *configResolver) scalarRangeBound(targetType SymbolType, expr Expr) (scalarRangeBound, bool) {
	symExpr, ok := expr.(*SymbolExpr)
	if !ok || symExpr.Symbol == nil {
		return scalarRangeBound{}, false
	}
	sym := symExpr.Symbol
	typ := targetType
	if !sym.Const && (sym.Type == SymbolInt || sym.Type == SymbolHex) {
		typ = sym.Type
	}
	text := sym.Name
	if !sym.Const {
		if value, ok := r.values[sym]; ok {
			text = value
		} else {
			text = defaultScalarValue(sym.Type)
		}
		if text == "" && (sym.Type == SymbolInt || sym.Type == SymbolHex) {
			text = defaultScalarValue(sym.Type)
		}
	}
	value, ok := parseScalarNumber(typ, text)
	if !ok {
		return scalarRangeBound{}, false
	}
	return scalarRangeBound{value: value, text: text}, true
}

func (r *configResolver) evalExprDefault(expr Expr, def triValue) triValue {
	return r.evalExprDefaultMode(expr, def, false)
}

func (r *configResolver) evalDepExprDefault(expr Expr, def triValue) triValue {
	return r.evalExprDefaultMode(expr, def, true)
}

func (r *configResolver) evalExprDefaultMode(expr Expr, def triValue, gateModConst bool) triValue {
	if expr == nil {
		return def
	}
	value, ok := r.evalExprMode(expr, gateModConst)
	if !ok {
		return triN
	}
	return value
}

func (r *configResolver) evalExpr(expr Expr) (triValue, bool) {
	return r.evalExprMode(expr, false)
}

func (r *configResolver) evalExprMode(expr Expr, gateModConst bool) (triValue, bool) {
	switch x := expr.(type) {
	case nil:
		return triY, true
	case *SymbolExpr:
		return r.evalSymbol(x.Symbol, gateModConst)
	case *UnaryExpr:
		if x.Op != "!" {
			return triN, false
		}
		value, ok := r.evalExprMode(x.X, gateModConst)
		if !ok {
			return triN, false
		}
		return triY - value, true
	case *BinaryExpr:
		left, ok := r.evalExprMode(x.Left, gateModConst)
		if !ok {
			return triN, false
		}
		right, ok := r.evalExprMode(x.Right, gateModConst)
		if !ok {
			return triN, false
		}
		switch x.Op {
		case "&&":
			return minResolvedTri(left, right), true
		case "||":
			return maxResolvedTri(left, right), true
		default:
			return triN, false
		}
	case *CompareExpr:
		return boolTri(r.evalCompare(x)), true
	default:
		return triN, false
	}
}

func (r *configResolver) evalSymbol(sym *Symbol, gateModConst bool) (triValue, bool) {
	if sym == nil {
		return triN, false
	}
	if sym.Const {
		if value, ok := triFromConst(sym.Name); ok {
			if gateModConst && sym.Name == "m" {
				return minResolvedTri(value, r.modulesTri()), true
			}
			return value, true
		}
		if sym.Name == "" {
			return triN, true
		}
		return triY, true
	}
	if len(sym.Menus) == 0 {
		return triN, true
	}
	switch sym.Type {
	case SymbolBool, SymbolTristate:
		return r.effective[sym], true
	case SymbolString:
		if r.values[sym] == "" || r.values[sym] == `""` {
			return triN, true
		}
		return triY, true
	case SymbolInt, SymbolHex, SymbolUnknown:
		if parseConfigInt(r.values[sym]) == 0 {
			return triN, true
		}
		return triY, true
	default:
		return triN, false
	}
}

func (r *configResolver) evalCompare(x *CompareExpr) bool {
	if isTriExpression(x.Left) && isTriExpression(x.Right) {
		leftTri, leftTriOK := r.evalExpr(x.Left)
		rightTri, rightTriOK := r.evalExpr(x.Right)
		return leftTriOK && rightTriOK && compareTri(leftTri, rightTri, x.Op)
	}

	left := r.exprValue(x.Left)
	right := r.exprValue(x.Right)
	leftInt, leftIntOK := parseConfigIntOK(left)
	rightInt, rightIntOK := parseConfigIntOK(right)
	if leftIntOK && rightIntOK {
		return compareOrdered(leftInt, rightInt, x.Op)
	}
	return compareOrdered(left, right, x.Op)
}

func (r *configResolver) exprValue(expr Expr) string {
	symExpr, ok := expr.(*SymbolExpr)
	if !ok || symExpr.Symbol == nil {
		value, _ := r.evalExpr(expr)
		return triName(value)
	}
	sym := symExpr.Symbol
	if sym.Const {
		return sym.Name
	}
	if value, ok := r.values[sym]; ok {
		value = strings.Trim(value, `"`)
		if value == "" && (sym.Type == SymbolInt || sym.Type == SymbolHex) {
			return defaultScalarValue(sym.Type)
		}
		return value
	}
	return "n"
}

func configTriValue(typ SymbolType, value string) triValue {
	switch value {
	case "y":
		return triY
	case "m":
		if typ == SymbolTristate {
			return triM
		}
		return triY
	default:
		return triN
	}
}

func (r *configResolver) clampSymbolTri(sym *Symbol, value triValue) triValue {
	if sym == nil || value != triM {
		return value
	}
	if sym.Type == SymbolBool {
		return triY
	}
	if sym.Type == SymbolTristate && sym.Choice == nil && !r.modulesEnabled() {
		return triY
	}
	return value
}

func (r *configResolver) modulesEnabled() bool {
	return r.modulesTri() != triN
}

func (r *configResolver) modulesTri() triValue {
	if r.tree.modulesSym == nil || r.tree.modulesSym.Const {
		return triN
	}
	return r.effective[r.tree.modulesSym]
}

func configScalarValue(typ SymbolType, value string) string {
	if typ == SymbolString {
		return strconv.Quote(value)
	}
	return value
}

func normalizeScalarValue(typ SymbolType, value string) string {
	if typ != SymbolHex || !validConfigHex(value) {
		return value
	}
	value = strings.Trim(value, `"`)
	if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
		return value
	}
	return "0x" + value
}

func defaultScalarValue(typ SymbolType) string {
	switch typ {
	case SymbolInt:
		return "0"
	case SymbolHex:
		return "0x0"
	default:
		return ""
	}
}

func parseScalarNumber(typ SymbolType, value string) (int64, bool) {
	value = strings.Trim(value, `"`)
	switch typ {
	case SymbolInt:
		if !validConfigInt(value) {
			return 0, false
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err == nil
	case SymbolHex:
		if !validConfigHex(value) {
			return 0, false
		}
		value = strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
		parsed, err := strconv.ParseInt(value, 16, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func validConfigInt(value string) bool {
	value = strings.Trim(value, `"`)
	if value == "" {
		return false
	}
	if value[0] == '-' {
		value = value[1:]
	}
	if value == "" {
		return false
	}
	if value[0] == '0' && len(value) != 1 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func validConfigHex(value string) bool {
	value = strings.Trim(value, `"`)
	if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
		value = value[2:]
	}
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

func minResolvedTri(a, b triValue) triValue {
	if a < b {
		return a
	}
	return b
}

func maxResolvedTri(a, b triValue) triValue {
	if a > b {
		return a
	}
	return b
}

func parseConfigInt(value string) int64 {
	parsed, _ := parseConfigIntOK(value)
	return parsed
}

func parseConfigIntOK(value string) (int64, bool) {
	value = strings.Trim(value, `"`)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 0, 64)
	return parsed, err == nil
}

type ordered interface {
	~int64 | ~string
}

func compareOrdered[T ordered](left, right T, op string) bool {
	switch op {
	case "=":
		return left == right
	case "!=":
		return left != right
	case "<":
		return left < right
	case "<=":
		return left <= right
	case ">":
		return left > right
	case ">=":
		return left >= right
	default:
		return false
	}
}

func sortedConfigKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
