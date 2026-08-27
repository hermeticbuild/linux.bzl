package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func testCompactKbuildAutomaticContext() compactKbuildAutomaticContext {
	return compactKbuildAutomaticContext{
		target: "build/out.o",
		stem:   "drivers/net/example",
		normal: []string{"src/a.c", "include/b.h", "src/a.c"},
		order:  []string{"generated/stamp", "include/config.h", "generated/stamp"},
	}
}

func TestExpandKbuildAutomaticCommandFieldExactPrerequisiteLists(t *testing.T) {
	context := testCompactKbuildAutomaticContext()
	for _, test := range []struct {
		field string
		want  []string
	}{
		{field: "$^", want: []string{"src/a.c", "include/b.h"}},
		{field: "$+", want: []string{"src/a.c", "include/b.h", "src/a.c"}},
		{field: "$?", want: []string{"src/a.c", "include/b.h"}},
		{field: "$|", want: []string{"generated/stamp", "include/config.h"}},
	} {
		t.Run(test.field, func(t *testing.T) {
			got, err := expandKbuildAutomaticCommandField(test.field, context)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("expansion = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExpandKbuildAutomaticCommandFieldDirectoryAndFilenameVariants(t *testing.T) {
	context := testCompactKbuildAutomaticContext()
	for _, test := range []struct {
		field string
		want  []string
	}{
		{field: "$(@D)", want: []string{"build"}},
		{field: "${@F}", want: []string{"out.o"}},
		{field: "$(<D)", want: []string{"src"}},
		{field: "${<F}", want: []string{"a.c"}},
		{field: "$(^D)", want: []string{"src", "include"}},
		{field: "${^F}", want: []string{"a.c", "b.h"}},
		{field: "$(+D)", want: []string{"src", "include", "src"}},
		{field: "${+F}", want: []string{"a.c", "b.h", "a.c"}},
		{field: "$(?D)", want: []string{"src", "include"}},
		{field: "${?F}", want: []string{"a.c", "b.h"}},
		{field: "$(|D)", want: []string{"generated", "include"}},
		{field: "${|F}", want: []string{"stamp", "config.h"}},
		{field: "$(*D)", want: []string{"drivers/net"}},
		{field: "${*F}", want: []string{"example"}},
	} {
		t.Run(test.field, func(t *testing.T) {
			got, err := expandKbuildAutomaticCommandField(test.field, context)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("expansion = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExpandKbuildAutomaticCommandFieldEmbeddedSingleValues(t *testing.T) {
	context := testCompactKbuildAutomaticContext()
	for _, test := range []struct {
		field string
		want  string
	}{
		{field: "--output=$@", want: "--output=build/out.o"},
		{field: "${@D}/copy-$(@F)", want: "build/copy-out.o"},
		{field: "first=$<-again=$<", want: "first=src/a.c-again=src/a.c"},
		{field: "stem=$*", want: "stem=drivers/net/example"},
		{field: "$(@F):$(@F):$@", want: "out.o:out.o:build/out.o"},
	} {
		t.Run(test.field, func(t *testing.T) {
			got, err := expandKbuildAutomaticCommandField(test.field, context)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{test.want}; !slices.Equal(got, want) {
				t.Fatalf("expansion = %q, want %q", got, want)
			}
		})
	}
}

func TestExpandKbuildAutomaticCommandFieldRejectsUnsupportedReferences(t *testing.T) {
	context := testCompactKbuildAutomaticContext()
	for _, field := range []string{"$%", "$(%D)", "$(@Q)", "prefix-$%"} {
		t.Run(field, func(t *testing.T) {
			_, err := expandKbuildAutomaticCommandField(field, context)
			if err == nil || !strings.Contains(err.Error(), "unsupported Kbuild automatic variable") {
				t.Fatalf("error = %v, want unsupported automatic variable", err)
			}
		})
	}
}

func TestExpandKbuildAutomaticCommandFieldRejectsEmbeddedList(t *testing.T) {
	context := testCompactKbuildAutomaticContext()
	_, err := expandKbuildAutomaticCommandField("inputs=$^", context)
	if err == nil || !strings.Contains(err.Error(), "expands to multiple argv fields") {
		t.Fatalf("error = %v, want embedded multi-field expansion error", err)
	}
}

func TestExpandKbuildAutomaticCommandFieldDoesNotAliasContextOrResults(t *testing.T) {
	context := testCompactKbuildAutomaticContext()
	wantNormal := slices.Clone(context.normal)
	wantOrder := slices.Clone(context.order)

	first, err := expandKbuildAutomaticCommandField("$+", context)
	if err != nil {
		t.Fatal(err)
	}
	first[0] = "mutated/result"
	if !slices.Equal(context.normal, wantNormal) || !slices.Equal(context.order, wantOrder) {
		t.Fatalf("context mutated: normal=%q order=%q", context.normal, context.order)
	}

	second, err := expandKbuildAutomaticCommandField("$+", context)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second, wantNormal) {
		t.Fatalf("subsequent expansion = %q, want unaliased %q", second, wantNormal)
	}
	second[1] = "mutated/again"
	if first[1] != wantNormal[1] {
		t.Fatalf("independent result slices alias: first=%q second=%q", first, second)
	}
	if !slices.Equal(context.normal, wantNormal) || !slices.Equal(context.order, wantOrder) {
		t.Fatalf("result mutation changed context: normal=%q order=%q", context.normal, context.order)
	}
}
