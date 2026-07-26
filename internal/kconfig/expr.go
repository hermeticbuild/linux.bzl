package kconfig

import "fmt"

type Expr interface {
	exprNode()
	String() string
}

type SymbolExpr struct {
	Symbol       *Symbol
	conditionKey string
}

type UnaryExpr struct {
	Op           string
	X            Expr
	conditionKey string
}

type BinaryExpr struct {
	Op           string
	Left, Right  Expr
	conditionKey string
}

type CompareExpr struct {
	Op           string
	Left, Right  Expr
	conditionKey string
}

func (*SymbolExpr) exprNode()  {}
func (*UnaryExpr) exprNode()   {}
func (*BinaryExpr) exprNode()  {}
func (*CompareExpr) exprNode() {}

func (e *SymbolExpr) String() string {
	if e == nil || e.Symbol == nil {
		return "<nil>"
	}
	if e.Symbol.Const {
		return fmt.Sprintf("%q", e.Symbol.Name)
	}
	return e.Symbol.Name
}

func (e *UnaryExpr) String() string {
	if e == nil {
		return "<nil>"
	}
	return e.Op + parenthesize(e.X)
}

func (e *BinaryExpr) String() string {
	if e == nil {
		return "<nil>"
	}
	return parenthesize(e.Left) + " " + e.Op + " " + parenthesize(e.Right)
}

func (e *CompareExpr) String() string {
	if e == nil {
		return "<nil>"
	}
	return parenthesize(e.Left) + " " + e.Op + " " + parenthesize(e.Right)
}

func symbolExpr(sym *Symbol) Expr {
	if sym == nil {
		return nil
	}
	return &SymbolExpr{Symbol: sym}
}

func andExpr(left, right Expr) Expr {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	if isConst(left, "y") {
		return right
	}
	if isConst(right, "y") {
		return left
	}
	if isConst(left, "n") || isConst(right, "n") {
		if isConst(left, "n") {
			return left
		}
		return right
	}
	return &BinaryExpr{Op: "&&", Left: left, Right: right}
}

func orExpr(left, right Expr) Expr {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	if isConst(left, "n") {
		return right
	}
	if isConst(right, "n") {
		return left
	}
	if isConst(left, "y") || isConst(right, "y") {
		if isConst(left, "y") {
			return left
		}
		return right
	}
	return &BinaryExpr{Op: "||", Left: left, Right: right}
}

func conditionalDep(dep, cond Expr, no *Symbol) Expr {
	if cond == nil {
		return dep
	}
	return orExpr(dep, &CompareExpr{
		Op:    "=",
		Left:  cond,
		Right: symbolExpr(no),
	})
}

func rewriteMExpr(e Expr, modules *Symbol) Expr {
	if e == nil || modules == nil {
		return e
	}
	switch x := e.(type) {
	case *SymbolExpr:
		if x.Symbol != nil && x.Symbol.Const && x.Symbol.Name == "m" {
			return andExpr(e, symbolExpr(modules))
		}
		return e
	case *UnaryExpr:
		return &UnaryExpr{Op: x.Op, X: rewriteMExpr(x.X, modules)}
	case *BinaryExpr:
		return &BinaryExpr{
			Op:    x.Op,
			Left:  rewriteMExpr(x.Left, modules),
			Right: rewriteMExpr(x.Right, modules),
		}
	default:
		return e
	}
}

func parenthesize(e Expr) string {
	switch e.(type) {
	case *BinaryExpr, *CompareExpr:
		return "(" + e.String() + ")"
	default:
		if e == nil {
			return "<nil>"
		}
		return e.String()
	}
}

func isConst(e Expr, name string) bool {
	sym, ok := e.(*SymbolExpr)
	return ok && sym.Symbol != nil && sym.Symbol.Const && sym.Symbol.Name == name
}

func exprIsYes(e Expr) bool {
	return e == nil || isConst(e, "y")
}

func exprContainsSymbol(e Expr, sym *Symbol) bool {
	if e == nil || sym == nil {
		return false
	}
	switch x := e.(type) {
	case *SymbolExpr:
		return x.Symbol == sym
	case *UnaryExpr:
		return exprContainsSymbol(x.X, sym)
	case *BinaryExpr:
		return exprContainsSymbol(x.Left, sym) || exprContainsSymbol(x.Right, sym)
	case *CompareExpr:
		return exprContainsSymbol(x.Left, sym) || exprContainsSymbol(x.Right, sym)
	default:
		return false
	}
}

func exprImpliesSymbol(e Expr, sym *Symbol) bool {
	if e == nil || sym == nil {
		return false
	}
	switch x := e.(type) {
	case *SymbolExpr:
		return x.Symbol == sym
	case *UnaryExpr:
		return false
	case *BinaryExpr:
		switch x.Op {
		case "&&":
			return exprImpliesSymbol(x.Left, sym) || exprImpliesSymbol(x.Right, sym)
		case "||":
			return exprImpliesSymbol(x.Left, sym) && exprImpliesSymbol(x.Right, sym)
		default:
			return false
		}
	default:
		return false
	}
}
