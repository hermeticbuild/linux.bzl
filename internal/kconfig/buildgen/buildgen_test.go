package buildgen

import (
	"strings"
	"testing"
)

func TestBuildFile(t *testing.T) {
	f := NewBuildFile("BUILD.bazel", "# generated")
	f.AddLoad("@rules//:defs.bzl", "z_rule", "a_rule")
	f.AddPackage([]string{"//visibility:public"})
	f.AddExportsFiles([]string{"metadata.json"})
	r := f.AddRule("a_rule", "thing")
	r.SetAttr("tags", []string{"manual"})
	r.SetAttr("values", map[string]string{"b": "2", "a": "1"})

	got := string(f.Format())
	for _, want := range []string{
		"# generated\n\n",
		`load("@rules//:defs.bzl", "a_rule", "z_rule")`,
		`package(default_visibility = ["//visibility:public"])`,
		`exports_files(["metadata.json"])`,
		`tags = ["manual"]`,
		`"a": "1"`,
		`"b": "2"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted BUILD missing %q:\n%s", want, got)
		}
	}
}

func TestBzlFile(t *testing.T) {
	stmts, err := ParseBzlStmts("helpers.bzl", `def helper():
    return True
`)
	if err != nil {
		t.Fatal(err)
	}
	f := NewBzlFile("defs.bzl", "# generated")
	f.AddLoad("//:helpers.bzl", "helper")
	f.AddAssign("VALUES", Expr([]string{"a", "b"}))
	f.AddAssign("BOTH", Binary(Ident("VALUES"), "+", Expr([]string{"c"})))
	f.AddStmts(stmts)

	got := string(f.Format())
	for _, want := range []string{
		"# generated\n\n",
		`load("//:helpers.bzl", "helper")`,
		`VALUES = ["a", "b"]`,
		`BOTH = VALUES + ["c"]`,
		`def helper():`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted bzl missing %q:\n%s", want, got)
		}
	}
}
