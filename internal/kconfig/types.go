package kconfig

import (
	"context"
	"fmt"
	"io"
)

type Options struct {
	RootDir         string
	SourceRoots     map[string]string
	Variables       map[string]string
	Env             map[string]string
	UseHostEnv      bool
	AllowShell      bool
	Shell           func(context.Context, string) (string, error)
	MaxIncludeDepth int
}

type Position struct {
	Filename string `json:"filename"`
	Line     int    `json:"line"`
}

func (p Position) String() string {
	if p.Filename == "" {
		return fmt.Sprintf("line %d", p.Line)
	}
	return fmt.Sprintf("%s:%d", p.Filename, p.Line)
}

type Diagnostic struct {
	Position Position `json:"position"`
	Message  string   `json:"message"`
}

type SymbolType string

const (
	SymbolUnknown  SymbolType = "unknown"
	SymbolBool     SymbolType = "bool"
	SymbolTristate SymbolType = "tristate"
	SymbolInt      SymbolType = "int"
	SymbolHex      SymbolType = "hex"
	SymbolString   SymbolType = "string"
)

type MenuType string

const (
	MenuRoot    MenuType = "root"
	MenuNormal  MenuType = "normal"
	MenuMenu    MenuType = "menu"
	MenuChoice  MenuType = "choice"
	MenuComment MenuType = "comment"
	MenuIf      MenuType = "if"
)

type PropertyType string

const (
	PropertyPrompt  PropertyType = "prompt"
	PropertyMenu    PropertyType = "menu"
	PropertyComment PropertyType = "comment"
	PropertyDefault PropertyType = "default"
	PropertySelect  PropertyType = "select"
	PropertyImply   PropertyType = "imply"
	PropertyRange   PropertyType = "range"
)

type Tree struct {
	Root        *Menu
	Symbols     map[string]*Symbol
	Sources     []Source
	Diagnostics []Diagnostic

	constSymbols map[string]*Symbol
	anonID       int
	modulesSym   *Symbol
}

type Source struct {
	From Position `json:"from"`
	Path string   `json:"path"`
}

type Symbol struct {
	Name          string
	Type          SymbolType
	Const         bool
	Transitional  bool
	Menus         []*Menu
	Properties    []*Property
	Choice        *Symbol
	ChoiceMembers []*Symbol
	DirDep        Expr
	RevDep        Expr
	Implied       Expr
}

type Property struct {
	Type     PropertyType
	Text     string
	Expr     Expr
	Visible  Expr
	Menu     *Menu
	Position Position
}

type Menu struct {
	Type       MenuType
	Symbol     *Symbol
	Prompt     *Property
	Help       string
	Dep        Expr
	Visibility Expr
	Properties []*Property
	Children   []*Menu
	Parent     *Menu
	Position   Position
}

func newTree() *Tree {
	t := &Tree{
		Symbols:      map[string]*Symbol{},
		constSymbols: map[string]*Symbol{},
	}
	t.Root = &Menu{Type: MenuRoot}
	t.symbol("y", true).Type = SymbolTristate
	t.symbol("m", true).Type = SymbolTristate
	t.symbol("n", true).Type = SymbolTristate
	return t
}

func (t *Tree) symbol(name string, constant bool) *Symbol {
	if constant {
		if sym := t.constSymbols[name]; sym != nil {
			return sym
		}
		sym := &Symbol{Name: name, Type: SymbolUnknown, Const: true}
		t.constSymbols[name] = sym
		return sym
	}
	if sym := t.Symbols[name]; sym != nil {
		return sym
	}
	sym := &Symbol{Name: name, Type: SymbolUnknown}
	t.Symbols[name] = sym
	return sym
}

func (t *Tree) anonymousSymbol(pos Position) *Symbol {
	t.anonID++
	return &Symbol{Name: fmt.Sprintf("<choice@%s:%d:%d>", pos.Filename, pos.Line, t.anonID), Type: SymbolBool}
}

func (m *Menu) addChild(child *Menu) {
	child.Parent = m
	m.Children = append(m.Children, child)
	if child.Symbol != nil {
		child.Symbol.Menus = append(child.Symbol.Menus, child)
	}
}

func (m *Menu) addProperty(prop *Property) {
	prop.Menu = m
	m.Properties = append(m.Properties, prop)
	if m.Symbol != nil {
		m.Symbol.Properties = append(m.Symbol.Properties, prop)
	}
	if prop.Type == PropertyPrompt || prop.Type == PropertyMenu || prop.Type == PropertyComment {
		m.Prompt = prop
	}
}

func Parse(ctx context.Context, r io.Reader, filename string, opts Options) (*Tree, error) {
	p, err := newParser(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := p.parseReader(r, filename, p.tree.Root); err != nil {
		return nil, err
	}
	p.finalize()
	return p.tree, nil
}

func ParseFile(ctx context.Context, rootPath string, opts Options) (*Tree, error) {
	p, err := newParser(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := p.parseFile(rootPath, p.tree.Root, Position{}); err != nil {
		return nil, err
	}
	p.finalize()
	return p.tree, nil
}
