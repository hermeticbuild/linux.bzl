// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
)

// dnfTerm encodes a single Bazel config condition label.
type dnfTerm struct {
	Label     string
	Kind      string
	Symbol    *Symbol
	State     string
	Derived   *derivedComparison
	Condition *conditionState
}

const (
	termRaw       = "raw"
	termEffective = "effective"
	termDerived   = "derived"
	termEnabled   = "enabled"
	termDisabled  = "disabled"
	termCondition = "condition"
)

type derivedComparison struct {
	Name  string
	Mode  string
	Left  derivedOperand
	Op    string
	Right derivedOperand
}

type derivedOperand struct {
	Kind  string
	Key   string
	Value string
	Type  string
}

type conditionState struct {
	Name  string
	Key   string
	Expr  Expr
	State triValue
}

// dnfClause is a conjunction of dnfTerm (logical AND).
type dnfClause []dnfTerm

// DNF is a disjunction of dnfClause (logical OR). The empty DNF means
// "always false"; a DNF containing an empty clause means "always true".
type DNF []dnfClause

// dnfClauseLimit bounds intermediate DNF results so pathological expressions
// cannot expand without bound. Real Linux Kconfig files contain wide OR
// fanouts, so this must be large enough for ordinary generated groups.
const dnfClauseLimit = 16384

func alwaysTrue() DNF  { return DNF{{}} }
func alwaysFalse() DNF { return DNF{} }

// IsAlwaysTrue reports whether the DNF is satisfied unconditionally.
func (d DNF) IsAlwaysTrue() bool {
	for _, c := range d {
		if len(c) == 0 {
			return true
		}
	}
	return false
}

// IsAlwaysFalse reports whether the DNF is unsatisfiable.
func (d DNF) IsAlwaysFalse() bool { return len(d) == 0 }

// encodeEnabled returns a DNF that is true iff the Kconfig expression
// evaluates to a non-n value. Compound expressions are represented by
// generated condition terms instead of being recursively expanded, which keeps
// wide Kconfig OR/AND trees exact without exponential DNF growth.
func encodeEnabled(e Expr) (DNF, bool) {
	if e == nil {
		return alwaysTrue(), true
	}
	if invariant, ok := encodeInvariantEnabled(e); ok {
		return invariant, true
	}
	switch x := e.(type) {
	case *SymbolExpr:
		return enabledTermOfSymbol(x.Symbol)
	case *UnaryExpr:
		if x.Op != "!" {
			return nil, false
		}
		if sym, ok := x.X.(*SymbolExpr); ok && sym.Symbol != nil && sym.Symbol.Type == SymbolBool {
			return encodeDisabled(x.X)
		}
	}
	y, ok := encodeState(e, triY)
	if !ok {
		return nil, false
	}
	m, ok := encodeState(e, triM)
	if !ok {
		return nil, false
	}
	out := orDNF(y, m)
	if out == nil {
		return nil, false
	}
	return out, true
}

type triValue int

const (
	triN triValue = iota
	triM
	triY
)

var triStates = []triValue{triN, triM, triY}

// encodeInvariantEnabled recognizes expressions whose enabled-ness is constant
// for every possible value of a single bool/tristate symbol. This handles
// Kconfig idioms like `FOO || !FOO` and `FOO || FOO = n` without requiring a
// general complement of `FOO__enabled`.
func encodeInvariantEnabled(e Expr) (DNF, bool) {
	symbols := map[*Symbol]struct{}{}
	if !collectEvalSymbols(e, symbols) || len(symbols) > 1 {
		return nil, false
	}

	var sym *Symbol
	for candidate := range symbols {
		sym = candidate
	}

	states := []triValue{triN}
	if sym != nil && len(sym.Menus) != 0 {
		switch sym.Type {
		case SymbolBool:
			states = []triValue{triN, triY}
		case SymbolTristate:
			states = []triValue{triN, triM, triY}
		default:
			return nil, false
		}
	}

	var wantEnabled bool
	for i, state := range states {
		value, ok := evalTri(e, sym, state)
		if !ok {
			return nil, false
		}
		enabled := value != triN
		if i == 0 {
			wantEnabled = enabled
			continue
		}
		if enabled != wantEnabled {
			return nil, false
		}
	}
	if wantEnabled {
		return alwaysTrue(), true
	}
	return alwaysFalse(), true
}

func collectEvalSymbols(e Expr, symbols map[*Symbol]struct{}) bool {
	switch x := e.(type) {
	case nil:
		return true
	case *SymbolExpr:
		if x.Symbol == nil {
			return false
		}
		if x.Symbol.Const {
			_, ok := triFromConst(x.Symbol.Name)
			return ok
		}
		symbols[x.Symbol] = struct{}{}
		return true
	case *UnaryExpr:
		return x.Op == "!" && collectEvalSymbols(x.X, symbols)
	case *BinaryExpr:
		return (x.Op == "&&" || x.Op == "||") &&
			collectEvalSymbols(x.Left, symbols) &&
			collectEvalSymbols(x.Right, symbols)
	case *CompareExpr:
		return (x.Op == "=" || x.Op == "!=") &&
			collectEvalSymbols(x.Left, symbols) &&
			collectEvalSymbols(x.Right, symbols)
	default:
		return false
	}
}

func evalTri(e Expr, sym *Symbol, state triValue) (triValue, bool) {
	switch x := e.(type) {
	case nil:
		return triY, true
	case *SymbolExpr:
		if x.Symbol == nil {
			return triN, false
		}
		if x.Symbol.Const {
			return triFromConst(x.Symbol.Name)
		}
		if x.Symbol != sym {
			return triN, false
		}
		return state, true
	case *UnaryExpr:
		if x.Op != "!" {
			return triN, false
		}
		value, ok := evalTri(x.X, sym, state)
		if !ok {
			return triN, false
		}
		return triY - value, true
	case *BinaryExpr:
		left, ok := evalTri(x.Left, sym, state)
		if !ok {
			return triN, false
		}
		right, ok := evalTri(x.Right, sym, state)
		if !ok {
			return triN, false
		}
		switch x.Op {
		case "&&":
			if left < right {
				return left, true
			}
			return right, true
		case "||":
			if left > right {
				return left, true
			}
			return right, true
		default:
			return triN, false
		}
	case *CompareExpr:
		left, ok := evalTri(x.Left, sym, state)
		if !ok {
			return triN, false
		}
		right, ok := evalTri(x.Right, sym, state)
		if !ok {
			return triN, false
		}
		equal := left == right
		switch x.Op {
		case "=":
			return boolTri(equal), true
		case "!=":
			return boolTri(!equal), true
		default:
			return triN, false
		}
	default:
		return triN, false
	}
}

func triFromConst(name string) (triValue, bool) {
	switch name {
	case "n":
		return triN, true
	case "m":
		return triM, true
	case "y":
		return triY, true
	default:
		return triN, false
	}
}

func boolTri(value bool) triValue {
	if value {
		return triY
	}
	return triN
}

func encodeState(e Expr, state triValue) (DNF, bool) {
	if needsConditionTerm(e) {
		return DNF{{conditionStateTerm(e, state)}}, true
	}
	return encodeStateBody(e, state)
}

func encodeStateBody(e Expr, state triValue) (DNF, bool) {
	if e == nil {
		if state == triY {
			return alwaysTrue(), true
		}
		return alwaysFalse(), true
	}
	switch x := e.(type) {
	case *SymbolExpr:
		return symbolState(x.Symbol, state)
	case *UnaryExpr:
		if x.Op != "!" {
			return nil, false
		}
		return encodeStateRef(x.X, triY-state)
	case *BinaryExpr:
		switch x.Op {
		case "&&":
			return encodeAndStateBody(x.Left, x.Right, state)
		case "||":
			return encodeOrStateBody(x.Left, x.Right, state)
		default:
			return nil, false
		}
	case *CompareExpr:
		return encodeCompareState(x, state)
	default:
		return nil, false
	}
}

func encodeStateRef(e Expr, state triValue) (DNF, bool) {
	if needsConditionTerm(e) {
		return DNF{{conditionStateTerm(e, state)}}, true
	}
	return encodeStateBody(e, state)
}

func needsConditionTerm(e Expr) bool {
	switch x := e.(type) {
	case *BinaryExpr:
		return true
	case *UnaryExpr:
		return x.Op == "!" && needsConditionTerm(x.X)
	case *CompareExpr:
		return needsConditionTerm(x.Left) || needsConditionTerm(x.Right)
	default:
		return false
	}
}

func encodeAndStateBody(left, right Expr, want triValue) (DNF, bool) {
	switch want {
	case triN:
		return orRefs(left, triN, right, triN)
	case triM:
		leftMRightMOrY, ok := andRefWithAny(right, []triValue{triM, triY}, left, triM)
		if !ok {
			return nil, false
		}
		leftYRightM, ok := andRefs(left, triY, right, triM)
		if !ok {
			return nil, false
		}
		out := orDNF(leftMRightMOrY, leftYRightM)
		if out == nil {
			return nil, false
		}
		return out, true
	case triY:
		return andRefs(left, triY, right, triY)
	default:
		return nil, false
	}
}

func encodeOrStateBody(left, right Expr, want triValue) (DNF, bool) {
	switch want {
	case triN:
		return andRefs(left, triN, right, triN)
	case triM:
		leftMRightNOrM, ok := andRefWithAny(right, []triValue{triN, triM}, left, triM)
		if !ok {
			return nil, false
		}
		leftNRightM, ok := andRefs(left, triN, right, triM)
		if !ok {
			return nil, false
		}
		out := orDNF(leftMRightNOrM, leftNRightM)
		if out == nil {
			return nil, false
		}
		return out, true
	case triY:
		return orRefs(left, triY, right, triY)
	default:
		return nil, false
	}
}

func andRefs(left Expr, leftState triValue, right Expr, rightState triValue) (DNF, bool) {
	l, ok := encodeStateRef(left, leftState)
	if !ok {
		return nil, false
	}
	r, ok := encodeStateRef(right, rightState)
	if !ok {
		return nil, false
	}
	out := andDNF(l, r)
	if out == nil {
		return nil, false
	}
	return out, true
}

func orRefs(left Expr, leftState triValue, right Expr, rightState triValue) (DNF, bool) {
	l, ok := encodeStateRef(left, leftState)
	if !ok {
		return nil, false
	}
	r, ok := encodeStateRef(right, rightState)
	if !ok {
		return nil, false
	}
	out := orDNF(l, r)
	if out == nil {
		return nil, false
	}
	return out, true
}

func anyStateRef(expr Expr, states []triValue) (DNF, bool) {
	out := alwaysFalse()
	for _, state := range states {
		d, ok := encodeStateRef(expr, state)
		if !ok {
			return nil, false
		}
		out = orDNF(out, d)
		if out == nil {
			return nil, false
		}
	}
	return out, true
}

func andRefWithAny(anyExpr Expr, anyStates []triValue, exactExpr Expr, exactState triValue) (DNF, bool) {
	any, ok := anyStateRef(anyExpr, anyStates)
	if !ok {
		return nil, false
	}
	exact, ok := encodeStateRef(exactExpr, exactState)
	if !ok {
		return nil, false
	}
	out := andDNF(any, exact)
	if out == nil {
		return nil, false
	}
	return out, true
}

func symbolState(sym *Symbol, state triValue) (DNF, bool) {
	if sym == nil {
		return nil, false
	}
	if sym.Const {
		value, ok := triFromConst(sym.Name)
		if !ok {
			return nil, false
		}
		if value == state {
			return alwaysTrue(), true
		}
		return alwaysFalse(), true
	}
	if len(sym.Menus) == 0 {
		if state == triN {
			return alwaysTrue(), true
		}
		return alwaysFalse(), true
	}
	switch sym.Type {
	case SymbolBool:
		switch state {
		case triY, triN:
			return DNF{{effectiveStateTerm(sym, state)}}, true
		case triM:
			return alwaysFalse(), true
		default:
			return nil, false
		}
	case SymbolTristate:
		return DNF{{effectiveStateTerm(sym, state)}}, true
	case SymbolInt, SymbolHex, SymbolString, SymbolUnknown:
		if state == triN {
			return alwaysTrue(), true
		}
		return alwaysFalse(), true
	default:
		return nil, false
	}
}

func encodeDisabled(e Expr) (DNF, bool) {
	if e == nil {
		return alwaysFalse(), true
	}
	sym, ok := e.(*SymbolExpr)
	if !ok {
		return encodeState(e, triN)
	}
	if sym.Symbol == nil {
		return nil, false
	}
	if sym.Symbol.Const {
		switch sym.Symbol.Name {
		case "y", "m":
			return alwaysFalse(), true
		case "n":
			return alwaysTrue(), true
		}
		return nil, false
	}
	if len(sym.Symbol.Menus) == 0 {
		// Undefined symbol defaults to n, so !X is always true.
		return alwaysTrue(), true
	}
	switch sym.Symbol.Type {
	case SymbolBool, SymbolTristate:
		return DNF{{disabledTerm(sym.Symbol)}}, true
	}
	return encodeState(e, triN)
}

func enabledTermOfSymbol(sym *Symbol) (DNF, bool) {
	if sym == nil {
		return nil, false
	}
	if sym.Const {
		switch sym.Name {
		case "y", "m":
			return alwaysTrue(), true
		case "n":
			return alwaysFalse(), true
		}
		return nil, false
	}
	if len(sym.Menus) == 0 {
		return alwaysFalse(), true
	}
	switch sym.Type {
	case SymbolBool, SymbolTristate:
		return DNF{{enabledTerm(sym)}}, true
	}
	return nil, false
}

func encodeCompareState(x *CompareExpr, state triValue) (DNF, bool) {
	if state == triM {
		return alwaysFalse(), true
	}
	if isTriExpression(x.Left) && isTriExpression(x.Right) {
		return encodeTriCompareState(x, state == triY)
	}
	derived, ok := derivedComparisonFor(x)
	if !ok {
		return nil, false
	}
	return DNF{{derivedComparisonStateTerm(derived, triName(state))}}, true
}

func encodeCompareTrue(x *CompareExpr) (DNF, bool) {
	return encodeCompareState(x, triY)
}

func encodeTriCompareState(x *CompareExpr, want bool) (DNF, bool) {
	left, ok := triExpressionStateRefs(x.Left)
	if !ok {
		return nil, false
	}
	right, ok := triExpressionStateRefs(x.Right)
	if !ok {
		return nil, false
	}
	out := alwaysFalse()
	for _, lState := range triStates {
		for _, rState := range triStates {
			if compareTri(lState, rState, x.Op) != want {
				continue
			}
			merged := andDNF(left[lState], right[rState])
			if merged == nil {
				return nil, false
			}
			out = orDNF(out, merged)
			if out == nil {
				return nil, false
			}
		}
	}
	return out, true
}

func triExpressionStates(e Expr) (map[triValue]DNF, bool) {
	return triExpressionStateRefs(e)
}

func triExpressionStateRefs(e Expr) (map[triValue]DNF, bool) {
	if !isTriExpression(e) {
		return nil, false
	}
	out := map[triValue]DNF{
		triN: alwaysFalse(),
		triM: alwaysFalse(),
		triY: alwaysFalse(),
	}
	for _, state := range triStates {
		encoded, ok := encodeStateRef(e, state)
		if !ok {
			return nil, false
		}
		out[state] = encoded
	}
	return out, true
}

func isTriExpression(e Expr) bool {
	switch x := e.(type) {
	case nil:
		return true
	case *SymbolExpr:
		return isTriSymbolExpr(x)
	case *UnaryExpr:
		return x.Op == "!" && isTriExpression(x.X)
	case *BinaryExpr:
		return (x.Op == "&&" || x.Op == "||") && isTriExpression(x.Left) && isTriExpression(x.Right)
	case *CompareExpr:
		if !validCompareOp(x.Op) {
			return false
		}
		if derived, ok := derivedComparisonFor(x); ok {
			return derived != nil
		}
		return isTriExpression(x.Left) && isTriExpression(x.Right)
	default:
		return false
	}
}

func isTriSymbolExpr(e *SymbolExpr) bool {
	if e.Symbol == nil {
		return false
	}
	sym := e.Symbol
	if sym.Const {
		_, ok := triFromConst(sym.Name)
		return ok
	}
	if len(sym.Menus) == 0 {
		return true
	}
	switch sym.Type {
	case SymbolBool, SymbolTristate:
		return true
	default:
		return false
	}
}

func compareTri(left, right triValue, op string) bool {
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

func validCompareOp(op string) bool {
	switch op {
	case "=", "!=", "<", "<=", ">", ">=":
		return true
	default:
		return false
	}
}

func derivedComparisonFor(x *CompareExpr) (*derivedComparison, bool) {
	left, ok := derivedOperandFor(x.Left)
	if !ok {
		return nil, false
	}
	right, ok := derivedOperandFor(x.Right)
	if !ok {
		return nil, false
	}
	mode, ok := derivedCompareMode(left, right)
	if !ok {
		return nil, false
	}
	cmp := &derivedComparison{
		Mode:  mode,
		Left:  left,
		Op:    x.Op,
		Right: right,
	}
	cmp.Name = derivedComparisonName(cmp)
	return cmp, true
}

func derivedOperandFor(e Expr) (derivedOperand, bool) {
	symExpr, ok := e.(*SymbolExpr)
	if !ok || symExpr.Symbol == nil {
		return derivedOperand{}, false
	}
	sym := symExpr.Symbol
	if sym.Const {
		return derivedOperand{
			Kind:  "const",
			Value: sym.Name,
			Type:  string(sym.Type),
		}, true
	}
	if len(sym.Menus) == 0 {
		return derivedOperand{
			Kind:  "const",
			Value: "n",
			Type:  string(SymbolTristate),
		}, true
	}
	return derivedOperand{
		Kind: "flag",
		Key:  "CONFIG_" + sym.Name,
		Type: string(sym.Type),
	}, true
}

func derivedCompareMode(left, right derivedOperand) (string, bool) {
	if left.Type == string(SymbolBool) || left.Type == string(SymbolTristate) ||
		right.Type == string(SymbolBool) || right.Type == string(SymbolTristate) {
		return "", false
	}
	if left.Type == string(SymbolString) || right.Type == string(SymbolString) {
		return "string", true
	}
	if left.Type == string(SymbolHex) || right.Type == string(SymbolHex) {
		return "unsigned", true
	}
	return "signed", true
}

func derivedComparisonName(cmp *derivedComparison) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
		cmp.Mode,
		cmp.Left.Kind,
		cmp.Left.Key,
		cmp.Left.Value,
		cmp.Left.Type,
		cmp.Op,
		cmp.Right.Kind,
		cmp.Right.Key,
		cmp.Right.Value,
		cmp.Right.Type,
	)))
	return "__kconfig_cmp_" + hex.EncodeToString(sum[:])[:16]
}

func rawStateTerm(sym *Symbol, state string) dnfTerm {
	return dnfTerm{
		Label:  ":CONFIG_" + sym.Name + "__" + state,
		Kind:   termRaw,
		Symbol: sym,
		State:  state,
	}
}

func derivedComparisonStateTerm(cmp *derivedComparison, state string) dnfTerm {
	return dnfTerm{
		Label:   ":" + cmp.Name + "__" + state,
		Kind:    termDerived,
		State:   state,
		Derived: cmp,
	}
}

func conditionStateTerm(expr Expr, state triValue) dnfTerm {
	cond := conditionStateFor(expr, state)
	return dnfTerm{
		Label:     ":" + cond.Name,
		Kind:      termCondition,
		State:     triName(state),
		Condition: cond,
	}
}

func conditionStateFor(expr Expr, state triValue) *conditionState {
	key := conditionExprKey(expr)
	return &conditionState{
		Name:  conditionStateName(key, state),
		Key:   key,
		Expr:  expr,
		State: state,
	}
}

func conditionStateName(exprKey string, state triValue) string {
	sum := sha256.Sum256([]byte(exprKey + "|" + triName(state)))
	return "__kconfig_expr_" + hex.EncodeToString(sum[:])[:16] + "__" + triName(state)
}

func conditionExprKey(expr Expr) string {
	switch x := expr.(type) {
	case nil:
		return hashExprKey("nil")
	case *SymbolExpr:
		if x.conditionKey != "" {
			return x.conditionKey
		}
		kind := "nil"
		name := ""
		if x.Symbol != nil {
			name = x.Symbol.Name
			if x.Symbol.Const {
				kind = "const"
			} else {
				kind = "config"
			}
		}
		x.conditionKey = hashExprKey("symbol", kind, name)
		return x.conditionKey
	case *UnaryExpr:
		if x.conditionKey != "" {
			return x.conditionKey
		}
		x.conditionKey = hashExprKey("unary", x.Op, conditionExprKey(x.X))
		return x.conditionKey
	case *BinaryExpr:
		if x.conditionKey != "" {
			return x.conditionKey
		}
		x.conditionKey = hashExprKey("binary", x.Op, conditionExprKey(x.Left), conditionExprKey(x.Right))
		return x.conditionKey
	case *CompareExpr:
		if x.conditionKey != "" {
			return x.conditionKey
		}
		x.conditionKey = hashExprKey("compare", x.Op, conditionExprKey(x.Left), conditionExprKey(x.Right))
		return x.conditionKey
	default:
		return hashExprKey("unknown")
	}
}

func hashExprKey(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		writeKeyString(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeKeyString(h hash.Hash, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{';'})
}

func effectiveStateTerm(sym *Symbol, state triValue) dnfTerm {
	return dnfTerm{
		Label:  ":CONFIG_" + sym.Name + "__effective_" + triName(state),
		Kind:   termEffective,
		Symbol: sym,
		State:  triName(state),
	}
}

func enabledTerm(sym *Symbol) dnfTerm {
	return dnfTerm{
		Label:  ":CONFIG_" + sym.Name + "__enabled",
		Kind:   termEnabled,
		Symbol: sym,
	}
}

func triName(state triValue) string {
	switch state {
	case triN:
		return "n"
	case triM:
		return "m"
	case triY:
		return "y"
	default:
		return "unknown"
	}
}

func disabledTerm(sym *Symbol) dnfTerm {
	return dnfTerm{
		Label:  ":CONFIG_" + sym.Name + "__disabled",
		Kind:   termDisabled,
		Symbol: sym,
	}
}

func splitConstCompare(left, right Expr) (*Symbol, string, bool) {
	if l, ok := left.(*SymbolExpr); ok && l.Symbol != nil && !l.Symbol.Const {
		if r, ok := right.(*SymbolExpr); ok && r.Symbol != nil && r.Symbol.Const {
			return l.Symbol, r.Symbol.Name, true
		}
	}
	if r, ok := right.(*SymbolExpr); ok && r.Symbol != nil && !r.Symbol.Const {
		if l, ok := left.(*SymbolExpr); ok && l.Symbol != nil && l.Symbol.Const {
			return r.Symbol, l.Symbol.Name, true
		}
	}
	return nil, "", false
}

// andDNF returns the DNF of (a AND b). Contradictory term pairings (e.g.
// X=y AND X=n) are dropped; the empty result encodes "always false". Returns
// nil if the resulting clause count exceeds [dnfClauseLimit].
func andDNF(a, b DNF) DNF {
	if a.IsAlwaysFalse() || b.IsAlwaysFalse() {
		return alwaysFalse()
	}
	out := DNF{}
	for _, ca := range a {
		for _, cb := range b {
			merged := mergeClauses(ca, cb)
			if merged == nil {
				continue
			}
			out = appendDNFClause(out, merged)
			if len(out) > dnfClauseLimit {
				return nil
			}
		}
	}
	return out
}

// orDNF returns the DNF of (a OR b). Duplicate clauses are collapsed.
// Returns nil if the resulting clause count exceeds [dnfClauseLimit].
func orDNF(a, b DNF) DNF {
	if a.IsAlwaysTrue() || b.IsAlwaysTrue() {
		return alwaysTrue()
	}
	out := DNF{}
	for _, c := range a {
		out = appendDNFClause(out, c)
	}
	for _, c := range b {
		out = appendDNFClause(out, c)
		if len(out) > dnfClauseLimit {
			return nil
		}
	}
	return out
}

func appendDNFClause(out DNF, clause dnfClause) DNF {
	clause = sortedClause(clause)
	filtered := out[:0]
	for _, existing := range out {
		if clauseSubsumes(existing, clause) {
			return out
		}
		if clauseSubsumes(clause, existing) {
			continue
		}
		filtered = append(filtered, existing)
	}
	return append(filtered, clause)
}

func sortedClause(clause dnfClause) dnfClause {
	out := append(dnfClause(nil), clause...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Label < out[j].Label
	})
	return out
}

func clauseSubsumes(a, b dnfClause) bool {
	if len(a) > len(b) {
		return false
	}
	bLabels := map[string]bool{}
	for _, term := range b {
		bLabels[term.Label] = true
	}
	for _, term := range a {
		if !bLabels[term.Label] {
			return false
		}
	}
	return true
}

func notDNF(d DNF) (DNF, bool) {
	if d.IsAlwaysFalse() {
		return alwaysTrue(), true
	}
	if d.IsAlwaysTrue() {
		return alwaysFalse(), true
	}

	out := alwaysTrue()
	for _, clause := range d {
		negated, ok := notClause(clause)
		if !ok {
			return nil, false
		}
		out = andDNF(out, negated)
		if out == nil {
			return nil, false
		}
	}
	return out, true
}

func notClause(clause dnfClause) (DNF, bool) {
	if len(clause) == 0 {
		return alwaysFalse(), true
	}
	out := alwaysFalse()
	for _, term := range clause {
		negated, ok := notTerm(term)
		if !ok {
			return nil, false
		}
		out = orDNF(out, negated)
		if out == nil {
			return nil, false
		}
	}
	return out, true
}

func notTerm(term dnfTerm) (DNF, bool) {
	switch term.Kind {
	case termRaw:
		if term.Symbol == nil {
			return nil, false
		}
		states := allowedValues(term.Symbol.Type)
		if len(states) == 0 {
			return nil, false
		}
		out := DNF{}
		for _, state := range states {
			if state == term.State {
				continue
			}
			out = append(out, dnfClause{rawStateTerm(term.Symbol, state)})
		}
		return out, true
	case termEffective:
		if term.Symbol == nil {
			return nil, false
		}
		state, ok := triFromConst(term.State)
		if !ok {
			return nil, false
		}
		out := DNF{}
		for _, candidate := range effectiveStates(term.Symbol) {
			if candidate == state {
				continue
			}
			out = append(out, dnfClause{effectiveStateTerm(term.Symbol, candidate)})
		}
		return out, true
	case termDerived:
		if term.Derived == nil {
			return nil, false
		}
		switch term.State {
		case "y":
			return DNF{{derivedComparisonStateTerm(term.Derived, "n")}}, true
		case "n":
			return DNF{{derivedComparisonStateTerm(term.Derived, "y")}}, true
		default:
			return nil, false
		}
	case termEnabled:
		if term.Symbol == nil {
			return nil, false
		}
		return DNF{{disabledTerm(term.Symbol)}}, true
	case termDisabled:
		if term.Symbol == nil {
			return nil, false
		}
		return DNF{{enabledTerm(term.Symbol)}}, true
	case termCondition:
		if term.Condition == nil {
			return nil, false
		}
		state, ok := triFromConst(term.State)
		if !ok {
			return nil, false
		}
		out := DNF{}
		for _, candidate := range triStates {
			if candidate == state {
				continue
			}
			out = append(out, dnfClause{conditionStateTerm(term.Condition.Expr, candidate)})
		}
		return out, true
	default:
		return nil, false
	}
}

func effectiveStates(sym *Symbol) []triValue {
	if sym != nil && sym.Type == SymbolBool {
		return []triValue{triN, triY}
	}
	return triStates
}

// mergeClauses combines two AND-clauses into one, dropping the result if it
// contains contradictory state constraints on the same symbol.
func mergeClauses(a, b dnfClause) dnfClause {
	rawStates := map[*Symbol]string{}
	exactEffectiveStates := map[*Symbol]string{}
	effectiveRequirements := map[*Symbol]string{}
	derivedStates := map[string]string{}
	conditionStates := map[string]string{}
	labels := map[string]dnfTerm{}
	for _, t := range a {
		if !mergeTerm(rawStates, exactEffectiveStates, effectiveRequirements, derivedStates, conditionStates, t) {
			return nil
		}
		labels[t.Label] = t
	}
	for _, t := range b {
		if !mergeTerm(rawStates, exactEffectiveStates, effectiveRequirements, derivedStates, conditionStates, t) {
			return nil
		}
		labels[t.Label] = t
	}
	out := make(dnfClause, 0, len(labels))
	for _, term := range labels {
		out = append(out, term)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Label < out[j].Label
	})
	return out
}

func mergeTerm(rawStates, exactEffectiveStates, effectiveRequirements map[*Symbol]string, derivedStates, conditionStates map[string]string, term dnfTerm) bool {
	if term.Kind == termDerived {
		if term.Derived == nil {
			return false
		}
		if existing, ok := derivedStates[term.Derived.Name]; ok && existing != term.State {
			return false
		}
		derivedStates[term.Derived.Name] = term.State
		return true
	}
	if term.Kind == termCondition {
		if term.Condition == nil {
			return false
		}
		if existing, ok := conditionStates[term.Condition.Name]; ok && existing != term.State {
			return false
		}
		conditionStates[term.Condition.Name] = term.State
		return true
	}
	if term.Symbol == nil {
		return true
	}
	switch term.Kind {
	case termRaw:
		if existing, ok := rawStates[term.Symbol]; ok && existing != term.State {
			return false
		}
		rawStates[term.Symbol] = term.State
	case termEffective:
		if existing, ok := exactEffectiveStates[term.Symbol]; ok && existing != term.State {
			return false
		}
		requirement := effectiveRequirements[term.Symbol]
		if requirement == termEnabled && term.State == "n" {
			return false
		}
		if requirement == termDisabled && term.State != "n" {
			return false
		}
		exactEffectiveStates[term.Symbol] = term.State
	case termEnabled, termDisabled:
		if existing, ok := effectiveRequirements[term.Symbol]; ok && existing != term.Kind {
			return false
		}
		if state, ok := exactEffectiveStates[term.Symbol]; ok {
			if term.Kind == termEnabled && state == "n" {
				return false
			}
			if term.Kind == termDisabled && state != "n" {
				return false
			}
		}
		effectiveRequirements[term.Symbol] = term.Kind
	}
	return true
}

func clauseKey(c dnfClause) string {
	parts := make([]string, len(c))
	for i, t := range c {
		parts[i] = t.Label
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// sortDNF sorts a DNF in a deterministic, canonical order so that emission is
// reproducible across runs.
func sortDNF(d DNF) DNF {
	out := append(DNF(nil), d...)
	sort.Slice(out, func(i, j int) bool {
		return clauseKey(out[i]) < clauseKey(out[j])
	})
	return out
}
