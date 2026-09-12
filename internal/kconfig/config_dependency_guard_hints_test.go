package kconfig

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestConfigDependencyCompilerGuardHintsLiteralOperands(t *testing.T) {
	for _, test := range []struct {
		name, source string
		want         []string
	}{
		{
			name: "canonical distinct reserved names",
			source: "#ifdef _Z\n#ifndef __A\n#if defined(_Z) || defined _B\n" +
				"#elif (defined(__A) && !defined(_C))\n#ifdef _\n",
			want: []string{"_", "_B", "_C", "_Z", "__A"},
		},
		{
			name: "ignore ordinary and CONFIG names",
			source: "#ifdef ordinary\n#ifndef CONFIG_TEST\n#if defined(CONFIG_OTHER) || defined name\n" +
				"#if _DIRECT_RESERVED_VALUE\n#define _REPLACEMENT defined(_NOT_LITERAL)\n" +
				"int value = defined(_NOT_A_DIRECTIVE);\n",
		},
		{
			name: "comments and digraph logical lines",
			source: "/* #ifdef _COMMENT */\n%: ifdef /* ignored */ _DIGRAPH\n" +
				"#if de\\\nfined(/* across\n lines */_SPLICED) // defined(_COMMENT)\n" +
				"#elif defined \\\r\n _CRLF\r\n# ifndef _TRAILING /* comment */\n",
			want: []string{"_CRLF", "_DIGRAPH", "_SPLICED", "_TRAILING"},
		},
		{
			name:   "quoted text is not an operand",
			source: "#if \"defined(_STRING)\" || 'defined(_CHARACTER)' || defined(_REAL)\n",
			want:   []string{"_REAL"},
		},
		{
			name: "exact ifdef identifiers",
			source: "#ifdef 9_INVALID\n#ifdef _BAD suffix\n#ifndef _BAD()\n" +
				"#ifdef _BAD$\n#ifndef _BAD-hyphen\n#ifdef _BADé\n" +
				"#ifdef _GOOD_123\n#ifdefined(_BAD)\n%:%:ifdef _BAD\n",
			want: []string{"_GOOD_123"},
		},
		{
			name: "malformed defined operands discard expression prefix",
			source: "#if defined(_PREFIX) || defined()\n" +
				"#if defined(_BAD + 1)\n#if defined(9_BAD)\n" +
				"#if defined((_BAD))\n#if defined(_BAD\n#if defined\n" +
				"#if defined(_BAD) )\n#if (defined(_BAD)\n" +
				"#ifdef _GOOD\n",
			want: []string{"_GOOD"},
		},
		{
			name:   "unsupported conditional syntax omits that expression",
			source: "#if defined(_BAD) || $dialect\n#if defined(_BAD) || L\"unsupported\"\n#ifdef _GOOD\n",
			want:   []string{"_GOOD"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := configDependencyCompilerGuardHintsForContents([]byte(test.source))
			if got.truncated || !slices.Equal(got.names, test.want) {
				t.Fatalf("guard hints = %+v; want names %v without truncation", got, test.want)
			}
		})
	}
}

func TestConfigDependencyCompilerGuardHintsCompleteReachedFile(t *testing.T) {
	source := strings.Repeat("/* ordinary source text */\n", 400) +
		"#ifndef __latent_entropy\n#if defined(__ASM_OFFSETS_H__)\n"
	if strings.Index(source, "__latent_entropy") <= 8192 {
		t.Fatal("fixture does not place the guard beyond the old inventory prefix")
	}
	got := configDependencyCompilerGuardHintsForContents([]byte(source))
	want := []string{"__ASM_OFFSETS_H__", "__latent_entropy"}
	if got.truncated || !slices.Equal(got.names, want) {
		t.Fatalf("complete-file guard hints = %+v; want %v", got, want)
	}
}

func TestConfigDependencyCompilerGuardHintsUnsupportedNormalization(t *testing.T) {
	for _, source := range []string{
		"#ifdef _PREFIX\n/* unterminated",
		"#ifdef _PREFIX\r#ifdef _OTHER\n",
		"#ifdef _PREFIX\n#if defined \\ \n _OTHER\n",
		"#ifdef _PREFIX\n??=ifdef _OTHER\n",
	} {
		got := configDependencyCompilerGuardHintsForContents([]byte(source))
		if got.truncated || len(got.names) != 0 {
			t.Fatalf("unsupported source %q produced %+v", source, got)
		}
	}
}

func TestConfigDependencyCompilerGuardHintsInputByteBoundary(t *testing.T) {
	prefix := "#ifdef _AT_BOUNDARY\n"
	source := prefix + strings.Repeat(" ", configDependencyCompilerGuardHintsMaximumBytes-len(prefix))
	got := configDependencyCompilerGuardHintsForContents([]byte(source))
	if got.truncated || !slices.Equal(got.names, []string{"_AT_BOUNDARY"}) {
		t.Fatalf("exact byte boundary produced %+v", got)
	}
	got = configDependencyCompilerGuardHintsForContents([]byte(source + " "))
	if !got.truncated || len(got.names) != 0 {
		t.Fatalf("input overflow retained partial hints: %+v", got)
	}
}

func TestConfigDependencyCompilerGuardHintsNameCountBoundary(t *testing.T) {
	var source strings.Builder
	want := make([]string, configDependencyCompilerGuardHintsMaximumNames)
	for index := range want {
		want[index] = fmt.Sprintf("_%04d", index)
		fmt.Fprintf(&source, "#ifdef %s\n", want[index])
	}
	// Repeated occurrences do not consume the distinct-name or retained-byte
	// budget, including at the exact limit.
	source.WriteString("#if defined(_0000) || defined(_4095)\n")
	got := configDependencyCompilerGuardHintsForContents([]byte(source.String()))
	if got.truncated || !slices.Equal(got.names, want) {
		t.Fatalf("exact name-count boundary: %d names, truncated=%v", len(got.names), got.truncated)
	}
	source.WriteString("#ifndef _overflow\n")
	got = configDependencyCompilerGuardHintsForContents([]byte(source.String()))
	if !got.truncated || len(got.names) != 0 {
		t.Fatalf("name-count overflow retained %d partial hints, truncated=%v", len(got.names), got.truncated)
	}
}

func TestConfigDependencyCompilerGuardHintsRetainedByteBoundary(t *testing.T) {
	first := "_small"
	last := "_" + strings.Repeat("a", configDependencyCompilerGuardHintsMaximumNameBytes-len(first)-1)
	source := "#ifdef " + first + "\n#ifndef " + last + "\n#ifdef " + first + "\n"
	got := configDependencyCompilerGuardHintsForContents([]byte(source))
	if got.truncated || !slices.Equal(got.names, []string{last, first}) {
		t.Fatalf("exact retained-byte boundary: %d names, truncated=%v", len(got.names), got.truncated)
	}
	got = configDependencyCompilerGuardHintsForContents([]byte(strings.Replace(source, last, last+"a", 1)))
	if !got.truncated || len(got.names) != 0 {
		t.Fatalf("retained-byte overflow retained %d partial hints, truncated=%v", len(got.names), got.truncated)
	}
}
