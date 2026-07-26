package buildgen

import (
	"bytes"
	"maps"
	"slices"

	gazellerule "github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

// BuildFile is a small wrapper around Gazelle's rule API for generating BUILD
// files without assembling Starlark syntax with printf calls.
type BuildFile struct {
	file   *gazellerule.File
	header string
}

func NewBuildFile(path, header string) *BuildFile {
	return &BuildFile{
		file:   gazellerule.EmptyFile(path, ""),
		header: header,
	}
}

func (f *BuildFile) AddLoad(module string, symbols ...string) {
	load := gazellerule.NewLoad(module)
	for _, symbol := range sortedStrings(symbols) {
		load.Add(symbol)
	}
	load.Insert(f.file, 0)
}

func (f *BuildFile) AddPackage(visibility []string) {
	if len(visibility) == 0 {
		return
	}
	r := gazellerule.NewRule("package", "")
	r.SetAttr("default_visibility", visibility)
	r.Insert(f.file)
}

func (f *BuildFile) AddExportsFiles(files []string) {
	if len(files) == 0 {
		return
	}
	r := gazellerule.NewRule("exports_files", "")
	r.AddArg(Expr(files))
	r.Insert(f.file)
}

func (f *BuildFile) AddRule(kind, name string) *gazellerule.Rule {
	r := gazellerule.NewRule(kind, name)
	r.Insert(f.file)
	return r
}

func (f *BuildFile) Format() []byte {
	return withHeader(f.header, f.file.Format())
}

// BzlFile is a minimal buildtools AST wrapper for generated .bzl files. Gazelle's
// rule API is BUILD-oriented, while these files need top-level assignments and
// fixed helper functions.
type BzlFile struct {
	file   *bzl.File
	header string
}

func NewBzlFile(path, header string) *BzlFile {
	return &BzlFile{
		file: &bzl.File{
			Path: path,
			Type: bzl.TypeBzl,
		},
		header: header,
	}
}

func (f *BzlFile) AddLoad(module string, symbols ...string) {
	f.file.Stmt = append(f.file.Stmt, LoadStmt(module, symbols...))
}

func (f *BzlFile) AddAssign(name string, rhs bzl.Expr) {
	f.file.Stmt = append(f.file.Stmt, &bzl.AssignExpr{
		LHS: &bzl.Ident{Name: name},
		Op:  "=",
		RHS: rhs,
	})
}

func (f *BzlFile) AddStmt(stmt bzl.Expr) {
	f.file.Stmt = append(f.file.Stmt, stmt)
}

func (f *BzlFile) AddStmts(stmts []bzl.Expr) {
	f.file.Stmt = append(f.file.Stmt, stmts...)
}

func (f *BzlFile) Format() []byte {
	return withHeader(f.header, bzl.Format(f.file))
}

func Expr(value interface{}) bzl.Expr {
	return gazellerule.ExprFromValue(value)
}

func String(value string) bzl.Expr {
	return &bzl.StringExpr{Value: value}
}

func Ident(name string) bzl.Expr {
	return &bzl.Ident{Name: name}
}

func Tuple(values ...bzl.Expr) bzl.Expr {
	return &bzl.TupleExpr{
		List:         values,
		ForceCompact: true,
	}
}

func Dict(values map[string]bzl.Expr) bzl.Expr {
	keys := slices.Sorted(maps.Keys(values))
	items := make([]*bzl.KeyValueExpr, 0, len(keys))
	for _, key := range keys {
		items = append(items, &bzl.KeyValueExpr{
			Key:   String(key),
			Value: values[key],
		})
	}
	return &bzl.DictExpr{
		List:           items,
		ForceMultiLine: true,
	}
}

func StringDict(values map[string]string) bzl.Expr {
	exprs := make(map[string]bzl.Expr, len(values))
	for key, value := range values {
		exprs[key] = String(value)
	}
	keys := slices.Sorted(maps.Keys(exprs))
	items := make([]*bzl.KeyValueExpr, 0, len(keys))
	for _, key := range keys {
		items = append(items, &bzl.KeyValueExpr{
			Key:   String(key),
			Value: exprs[key],
		})
	}
	return &bzl.DictExpr{List: items}
}

func Binary(left bzl.Expr, op string, right bzl.Expr) bzl.Expr {
	return &bzl.BinaryExpr{
		X:  left,
		Op: op,
		Y:  right,
	}
}

func LoadStmt(module string, symbols ...string) bzl.Expr {
	stmt := &bzl.LoadStmt{
		Module:       &bzl.StringExpr{Value: module},
		ForceCompact: true,
	}
	for _, symbol := range sortedStrings(symbols) {
		ident := &bzl.Ident{Name: symbol}
		stmt.From = append(stmt.From, ident)
		stmt.To = append(stmt.To, ident)
	}
	return stmt
}

func ParseBzlStmts(filename, data string) ([]bzl.Expr, error) {
	file, err := bzl.ParseBzl(filename, []byte(data))
	if err != nil {
		return nil, err
	}
	return file.Stmt, nil
}

func withHeader(header string, data []byte) []byte {
	if header == "" {
		return data
	}
	var out bytes.Buffer
	out.WriteString(header)
	if len(header) < 2 || header[len(header)-2:] != "\n\n" {
		if len(header) == 0 || header[len(header)-1] != '\n' {
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}
	out.Write(data)
	return out.Bytes()
}

func sortedStrings(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}
