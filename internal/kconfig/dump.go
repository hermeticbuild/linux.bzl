package kconfig

import "sort"

type Dump struct {
	Sources     []Source     `json:"sources,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	Symbols     []DumpSymbol `json:"symbols"`
	Menus       []DumpMenu   `json:"menus"`
}

type DumpSymbol struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Transitional  bool     `json:"transitional,omitempty"`
	ChoiceMembers []string `json:"choice_members,omitempty"`
	DirDep        string   `json:"dir_dep,omitempty"`
	RevDep        string   `json:"rev_dep,omitempty"`
	Implied       string   `json:"implied,omitempty"`
}

type DumpMenu struct {
	Type       string         `json:"type"`
	Symbol     string         `json:"symbol,omitempty"`
	Prompt     string         `json:"prompt,omitempty"`
	Help       string         `json:"help,omitempty"`
	Dep        string         `json:"dep,omitempty"`
	Visibility string         `json:"visibility,omitempty"`
	Position   Position       `json:"position"`
	Properties []DumpProperty `json:"properties,omitempty"`
	Children   []DumpMenu     `json:"children,omitempty"`
}

type DumpProperty struct {
	Type     string   `json:"type"`
	Text     string   `json:"text,omitempty"`
	Expr     string   `json:"expr,omitempty"`
	Visible  string   `json:"visible,omitempty"`
	Position Position `json:"position"`
}

func (t *Tree) Dump() Dump {
	symbols := make([]*Symbol, 0, len(t.Symbols))
	for _, sym := range t.Symbols {
		symbols = append(symbols, sym)
	}
	sort.Slice(symbols, func(i, j int) bool {
		return symbols[i].Name < symbols[j].Name
	})

	out := Dump{
		Sources:     append([]Source(nil), t.Sources...),
		Diagnostics: append([]Diagnostic(nil), t.Diagnostics...),
		Symbols:     make([]DumpSymbol, 0, len(symbols)),
	}
	for _, sym := range symbols {
		memberNames := make([]string, 0, len(sym.ChoiceMembers))
		for _, member := range sym.ChoiceMembers {
			memberNames = append(memberNames, member.Name)
		}
		out.Symbols = append(out.Symbols, DumpSymbol{
			Name:          sym.Name,
			Type:          string(sym.Type),
			Transitional:  sym.Transitional,
			ChoiceMembers: memberNames,
			DirDep:        exprString(sym.DirDep),
			RevDep:        exprString(sym.RevDep),
			Implied:       exprString(sym.Implied),
		})
	}
	for _, child := range t.Root.Children {
		out.Menus = append(out.Menus, dumpMenu(child))
	}
	return out
}

func dumpMenu(menu *Menu) DumpMenu {
	out := DumpMenu{
		Type:       string(menu.Type),
		Dep:        exprString(menu.Dep),
		Visibility: exprString(menu.Visibility),
		Position:   menu.Position,
	}
	if menu.Symbol != nil {
		out.Symbol = menu.Symbol.Name
	}
	if menu.Prompt != nil {
		out.Prompt = menu.Prompt.Text
	}
	if menu.Help != "" {
		out.Help = menu.Help
	}
	for _, prop := range menu.Properties {
		out.Properties = append(out.Properties, DumpProperty{
			Type:     string(prop.Type),
			Text:     prop.Text,
			Expr:     exprString(prop.Expr),
			Visible:  exprString(prop.Visible),
			Position: prop.Position,
		})
	}
	for _, child := range menu.Children {
		out.Children = append(out.Children, dumpMenu(child))
	}
	return out
}

func exprString(expr Expr) string {
	if expr == nil {
		return ""
	}
	return expr.String()
}
