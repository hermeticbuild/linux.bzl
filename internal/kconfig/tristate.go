package kconfig

// triValue is Kconfig's three-valued boolean domain.
type triValue int

const (
	triN triValue = iota
	triM
	triY
)

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

func isTriExpression(expr Expr) bool {
	switch x := expr.(type) {
	case nil:
		return true
	case *SymbolExpr:
		return isTriSymbolExpr(x)
	case *UnaryExpr:
		return x.Op == "!" && isTriExpression(x.X)
	case *BinaryExpr:
		return (x.Op == "&&" || x.Op == "||") &&
			isTriExpression(x.Left) && isTriExpression(x.Right)
	case *CompareExpr:
		if !validCompareOp(x.Op) {
			return false
		}
		if isScalarComparison(x.Left, x.Right) {
			return true
		}
		return isTriExpression(x.Left) && isTriExpression(x.Right)
	default:
		return false
	}
}

func isTriSymbolExpr(expr *SymbolExpr) bool {
	if expr.Symbol == nil {
		return false
	}
	sym := expr.Symbol
	if sym.Const {
		_, ok := triFromConst(sym.Name)
		return ok
	}
	if len(sym.Menus) == 0 {
		return true
	}
	return sym.Type == SymbolBool || sym.Type == SymbolTristate
}

// isScalarComparison mirrors Kconfig's scalar-comparison classification
// without retaining the old Bazel-label lowering representation.
func isScalarComparison(left, right Expr) bool {
	leftType, leftOK := comparisonOperandType(left)
	rightType, rightOK := comparisonOperandType(right)
	if !leftOK || !rightOK {
		return false
	}
	return leftType != SymbolBool && leftType != SymbolTristate &&
		rightType != SymbolBool && rightType != SymbolTristate
}

func comparisonOperandType(expr Expr) (SymbolType, bool) {
	symExpr, ok := expr.(*SymbolExpr)
	if !ok || symExpr.Symbol == nil {
		return "", false
	}
	sym := symExpr.Symbol
	if !sym.Const && len(sym.Menus) == 0 {
		return SymbolTristate, true
	}
	return sym.Type, true
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
