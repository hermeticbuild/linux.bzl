package kconfig

import (
	"slices"
	"testing"
)

func TestCompactKbuildGeneratedTextProjectionGroup(t *testing.T) {
	files := map[string]string{
		"drivers/first/modules.order":               "drivers/first/one.o\n",
		"${tree:prep}/drivers/second/modules.order": "drivers/second/two.o\n",
	}
	recipe := `{ echo ${tree:prep}/drivers/local.o; cat ${tree:prep}/drivers/first/modules.order drivers/second/modules.order ${tree:prep}/drivers/first/modules.order; printf '%s\n' ${tree:prep}/drivers/final.o drivers/tail.o; :; } > ${tree:prep}/drivers/modules.order`
	want := "drivers/local.o\n" +
		"drivers/first/one.o\n" +
		"drivers/second/two.o\n" +
		"drivers/first/one.o\n" +
		"drivers/final.o\n" +
		"drivers/tail.o\n"
	got, exact := CompactKbuildGeneratedTextProjection(recipe, "drivers/modules.order", files)
	if !exact || got != want {
		t.Fatalf("CompactKbuildGeneratedTextProjection() = (%q, %t), want (%q, true)", got, exact, want)
	}
}

func TestCompactKbuildGeneratedTextProjectionDirectForms(t *testing.T) {
	for _, test := range []struct {
		name   string
		recipe string
		files  map[string]string
		want   string
	}{
		{name: "echo", recipe: `echo one 'two words' > ${tree:prep}/modules.order`, want: "one two words\n"},
		{name: "cat aliases", recipe: `/bin/cat ${tree:prep}/first.order second.order first.order > modules.order`, files: map[string]string{
			"first.order":               "first",
			"${tree:prep}/second.order": "second\n",
		}, want: "firstsecond\nfirst"},
		{name: "printf", recipe: `/usr/bin/printf '%s\n' first second '$literal' > modules.order;`, want: "first\nsecond\n$literal\n"},
		{name: "printf missing argument", recipe: `printf '%s\n' > modules.order`, want: "\n"},
		{name: "empty no-op", recipe: `: > modules.order`},
		{name: "empty group", recipe: `{ :; } > modules.order`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, exact := CompactKbuildGeneratedTextProjection(test.recipe, "${tree:prep}/modules.order", test.files)
			if !exact || got != test.want {
				t.Fatalf("CompactKbuildGeneratedTextProjection(%q) = (%q, %t), want (%q, true)", test.recipe, got, exact, test.want)
			}
		})
	}
}

func TestCompactKbuildGeneratedTextProjectionIfChangedWrapper(t *testing.T) {
	recipe := "@set -e;   \t\ttrap 'rm -f modules.order; trap - HUP; kill -s HUP $$' HUP; trap 'rm -f modules.order; trap - INT; kill -s INT $$' INT; trap 'rm -f modules.order; trap - QUIT; kill -s QUIT $$' QUIT; trap 'rm -f modules.order; trap - TERM; kill -s TERM $$' TERM; trap 'rm -f modules.order; trap - PIPE; kill -s PIPE $$' PIPE; { echo first.o; echo second.o; :; } \t> modules.order; \tprintf '%s\\n' 'savedcmd_modules.order := { echo first.o; } \t> modules.order' > ./.modules.order.cmd"
	got, exact := CompactKbuildGeneratedTextProjection(recipe, "modules.order", nil)
	if want := "first.o\nsecond.o\n"; !exact || got != want {
		tokens, lexErr := lexCompactKbuildRecipe(recipe)
		segments, segmented := compactKbuildGeneratedTextCommandSegments(recipe, tokens)
		t.Fatalf("if_changed projection = (%q, %t), want (%q, true); lex=%v segmented=%t segments=%#v", got, exact, want, lexErr, segmented, segments)
	}
}

func TestCompactKbuildGeneratedTextProjectionAliases(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string]string
		want  string
		exact bool
	}{
		{
			name: "matching aliases",
			files: map[string]string{
				"leaf.order":              "leaf.o\n",
				"${tree:prep}/leaf.order": "leaf.o\n",
			},
			want:  "leaf.o\n",
			exact: true,
		},
		{
			name: "conflicting aliases",
			files: map[string]string{
				"leaf.order":              "first.o\n",
				"${tree:prep}/leaf.order": "second.o\n",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, exact := CompactKbuildGeneratedTextProjection(`cat leaf.order > modules.order`, "modules.order", test.files)
			if exact != test.exact || got != test.want {
				t.Fatalf("CompactKbuildGeneratedTextProjection() = (%q, %t), want (%q, %t)", got, exact, test.want, test.exact)
			}
		})
	}

	got, exact := CompactKbuildGeneratedTextProjection(`cat selected.order > modules.order`, "modules.order", map[string]string{
		"selected.order":               "selected.o\n",
		"unused.order":                 "first.o\n",
		"${tree:prep}/unused.order":    "second.o\n",
		"${tree:kernel}/ignored.order": "ignored.o\n",
	})
	if !exact || got != "selected.o\n" {
		t.Fatalf("unused ambiguity changed projection: (%q, %t)", got, exact)
	}
}

func TestCompactKbuildGeneratedTextProjectionWithResolverReadsOnlyCatOperands(t *testing.T) {
	contents := map[string]string{
		"${tree:prep}/first.order": "first.o\n",
		"second.order":             "second.o\n",
		"first.order":              "first.o\n",
		"unused.order":             "unused.o\n",
	}
	called := []string{}
	got, exact := CompactKbuildGeneratedTextProjectionWithResolver(
		`{ echo local.o; cat ${tree:prep}/first.order second.order first.order; printf '%s\n' final.o; :; } > modules.order`,
		"${tree:prep}/modules.order",
		func(path string) (string, bool) {
			called = append(called, path)
			content, ok := contents[path]
			return content, ok
		},
	)
	want := "local.o\nfirst.o\nsecond.o\nfirst.o\nfinal.o\n"
	if !exact || got != want {
		t.Fatalf("CompactKbuildGeneratedTextProjectionWithResolver() = (%q, %t), want (%q, true)", got, exact, want)
	}
	wantCalled := []string{"${tree:prep}/first.order", "second.order", "first.order"}
	if !slices.Equal(called, wantCalled) {
		t.Fatalf("resolver calls = %q, want cat operands only in order %q", called, wantCalled)
	}
}

func TestCompactKbuildGeneratedTextProjectionWithResolverPreservesNestedPrepRoot(t *testing.T) {
	contents := map[string]string{
		"${tree:prep}/drivers/demo/leaf.order": "rooted.o\n",
		"local.order":                          "local.o\n",
	}
	called := []string{}
	got, exact := CompactKbuildGeneratedTextProjectionWithResolver(
		`cat ${tree:prep}/drivers/demo/leaf.order local.order > ${tree:prep}/drivers/demo/modules.order`,
		"drivers/demo/modules.order",
		func(path string) (string, bool) {
			called = append(called, path)
			content, ok := contents[path]
			return content, ok
		},
	)
	if want := "rooted.o\nlocal.o\n"; !exact || got != want {
		t.Fatalf("nested prep-rooted projection = (%q, %t), want (%q, true)", got, exact, want)
	}
	wantCalled := []string{"${tree:prep}/drivers/demo/leaf.order", "local.order"}
	if !slices.Equal(called, wantCalled) {
		t.Fatalf("nested resolver calls = %q, want %q", called, wantCalled)
	}
}

func TestCompactKbuildGeneratedTextProjectionWithResolverCanonicalizesPrepRootedSelfRead(t *testing.T) {
	calls := 0
	got, exact := CompactKbuildGeneratedTextProjectionWithResolver(
		`cat ${tree:prep}/drivers/demo/modules.order > ${tree:prep}/drivers/demo/modules.order`,
		"drivers/demo/modules.order",
		func(string) (string, bool) {
			calls++
			return "stale\n", true
		},
	)
	if exact || got != "" {
		t.Fatalf("prep-rooted self-read projection = (%q, %t), want (\"\", false)", got, exact)
	}
	if calls != 0 {
		t.Fatalf("prep-rooted self-read called resolver %d times, want zero", calls)
	}
}

func TestCompactKbuildGeneratedTextProjectionWithResolverDataOnlyCommandsDoNotResolve(t *testing.T) {
	for _, test := range []struct {
		name   string
		recipe string
		want   string
	}{
		{name: "echo", recipe: `echo one two > modules.order`, want: "one two\n"},
		{name: "printf", recipe: `printf '%s\n' one two > modules.order`, want: "one\ntwo\n"},
		{name: "no-op", recipe: `: > modules.order`},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			got, exact := CompactKbuildGeneratedTextProjectionWithResolver(
				test.recipe,
				"modules.order",
				func(string) (string, bool) {
					calls++
					return "unexpected", true
				},
			)
			if !exact || got != test.want {
				t.Fatalf("CompactKbuildGeneratedTextProjectionWithResolver() = (%q, %t), want (%q, true)", got, exact, test.want)
			}
			if calls != 0 {
				t.Fatalf("resolver called %d times, want zero", calls)
			}
		})
	}
}

func TestCompactKbuildGeneratedTextProjectionWithResolverFailsOnMissingOperand(t *testing.T) {
	called := []string{}
	got, exact := CompactKbuildGeneratedTextProjectionWithResolver(
		`cat first.order missing.order later.order > modules.order`,
		"modules.order",
		func(path string) (string, bool) {
			called = append(called, path)
			if path == "first.order" {
				return "first.o\n", true
			}
			return "", false
		},
	)
	if exact || got != "" {
		t.Fatalf("missing resolver operand projection = (%q, %t), want (\"\", false)", got, exact)
	}
	wantCalled := []string{"first.order", "missing.order"}
	if !slices.Equal(called, wantCalled) {
		t.Fatalf("resolver calls = %q, want fail-fast calls %q", called, wantCalled)
	}
}

func TestCompactKbuildGeneratedTextProjectionFailsClosed(t *testing.T) {
	files := map[string]string{
		"leaf.order":                 "leaf.o\n",
		"${tree:prep}/modules.order": "old.o\n",
		"-n":                         "option-shaped\n",
		"*.order":                    "globbed\n",
	}
	for _, test := range []struct {
		name   string
		recipe string
		target string
	}{
		{name: "unknown cat input", recipe: `cat missing.order > modules.order`},
		{name: "cat option", recipe: `cat -n > modules.order`},
		{name: "cat self", recipe: `cat ${tree:prep}/modules.order > modules.order`},
		{name: "unknown command", recipe: `sed -n p leaf.order > modules.order`},
		{name: "wrong target", recipe: `echo leaf.o > other.order`},
		{name: "pipe", recipe: `cat leaf.order | sort > modules.order`},
		{name: "conditional", recipe: `cat leaf.order && echo leaf.o > modules.order`},
		{name: "append", recipe: `echo leaf.o >> modules.order`},
		{name: "input redirect", recipe: `cat < leaf.order > modules.order`},
		{name: "unsafe printf format", recipe: `printf '%s' leaf.o > modules.order`},
		{name: "echo option", recipe: `echo -n leaf.o > modules.order`},
		{name: "echo escape", recipe: `echo 'leaf\n.o' > modules.order`},
		{name: "double quoted echo escape", recipe: `echo "leaf\n.o" > modules.order`},
		{name: "double quoted printf argument escape", recipe: `printf '%s\n' "leaf\n.o" > modules.order`},
		{name: "stderr redirect", recipe: `echo leaf.o 2> modules.order`},
		{name: "environment", recipe: `LC_ALL=C echo leaf.o > modules.order`},
		{name: "nested group", recipe: `{ echo leaf.o; { :; }; } > modules.order`},
		{name: "unterminated group command", recipe: `{ echo leaf.o } > modules.order`},
		{name: "opaque command in group", recipe: `{ echo leaf.o; sort leaf.order; } > modules.order`},
		{name: "kernel tree input", recipe: `cat ${tree:kernel}/leaf.order > modules.order`},
		{name: "parent traversal", recipe: `cat ../leaf.order > modules.order`},
		{name: "glob input", recipe: `cat '*.order' > modules.order`},
		{name: "raw variable", recipe: `echo "$VALUE" > modules.order`},
		{name: "ignored failure prefix", recipe: `-echo leaf.o > modules.order`},
		{name: "unknown wrapper command", recipe: `set -e; rm -f modules.order; echo leaf.o > modules.order`},
		{name: "success-path trap", recipe: `set -e; trap 'rm -f modules.order' EXIT; echo leaf.o > modules.order`},
		{name: "two wrapper writes", recipe: `set -e; echo first.o > modules.order; echo second.o > modules.order`},
		{name: "comment", recipe: `echo leaf.o # no redirect > modules.order`},
		{name: "newline command separator", recipe: "echo setup\necho leaf.o > modules.order"},
		{name: "invalid target", recipe: `echo leaf.o > modules.order`, target: "../modules.order"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := test.target
			if target == "" {
				target = "modules.order"
			}
			if got, exact := CompactKbuildGeneratedTextProjection(test.recipe, target, files); exact {
				t.Fatalf("CompactKbuildGeneratedTextProjection(%q) = (%q, true), want inexact", test.recipe, got)
			}
		})
	}
}
