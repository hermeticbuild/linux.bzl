package kconfig

import (
	"slices"
	"strings"
	"testing"
)

func TestParseProbeSourceShellWordsPreservesCompilerArgv(t *testing.T) {
	for _, test := range []struct {
		name, text string
		want       []string
	}{
		{"empty expansion", " \t ", []string{}},
		{"filtered generator flags", "-DKBUILD_MODFILE='\"" + compactKbuildActionObjectTreeMarker + "/asm-offsets\"' -O2", []string{`-DKBUILD_MODFILE="__LINUX_BZL_OBJECT_TREE__/asm-offsets"`, "-O2"}},
		{"source root macro", "-DPATH='\"" + compactKbuildActionSourceTreeMarker + "/a.c\"'", []string{`-DPATH="__LINUX_BZL_SOURCE_TREE__/a.c"`}},
		{"absolute object root", "-DPATH='\"" + compactKbuildActionAbsoluteObjectTreeMarker + "/a\"'", []string{`-DPATH="__LINUX_BZL_OBJECT_TREE__/a"`}},
		{"nested root joins", "-DPATH='\"" + compactKbuildActionObjectTreeMarker + "/" + compactKbuildActionObjectTreeMarker + "/a\"'", []string{`-DPATH="__LINUX_BZL_OBJECT_TREE__/a"`}},
		{"quoted spaces", `-DPATH='"dir/hello world"' -D OTHER='"second path"'`, []string{`-DPATH="dir/hello world"`, "-D", `OTHER="second path"`}},
		{"escaped double quotes", `-DPATH=\"dir/file\"`, []string{`-DPATH="dir/file"`}},
		{"single quoted literal dollar", `-DPATH='"$literal/$(not_executed)/@file"'`, []string{`-DPATH="$literal/$(not_executed)/@file"`}},
		{"double quoted escaped dollar", `-DPATH="\$literal/file"`, []string{`-DPATH=$literal/file`}},
		{"quoted backslash", `-DPATH='"C:\\literal\\file"'`, []string{`-DPATH="C:\\literal\\file"`}},
		{"escaped space", `-DNAME=one\ two -O2`, []string{"-DNAME=one two", "-O2"}},
		{"line continuation", "-O2 \\\n-g", []string{"-O2", "-g"}},
		{"empty quoted word retained", `-O2 '' -g`, []string{"-O2", "", "-g"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseProbeSourceShellWords(test.text)
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("source words = %q, %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestParseProbeSourceShellWordsKeepsProtectedSourceLiteralsInert(t *testing.T) {
	source := `-DLITERAL='"__LINUX_BZL_OBJECT_TREE__/literal"' -DTREE='$${tree:kernel}'`
	protected, err := protectCompactKbuildSourceLiteralActionMarkers(source)
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary Make expansion removes the first dollar of an escaped pair.
	protected = strings.ReplaceAll(protected, "$"+compactKbuildLiteralTreeEscapeByte, compactKbuildLiteralTreeEscapeByte)
	got, err := ParseProbeSourceShellWords(protected)
	want := []string{`-DLITERAL="__LINUX_BZL_OBJECT_TREE__/literal"`, `-DTREE=${tree:kernel}`}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("protected source literals = %q, %v, want %q", got, err, want)
	}
}

func TestParseProbeSourceShellWordsRejectsShellAuthority(t *testing.T) {
	for _, text := range []string{
		`-O2; echo injected`, `-O2 | other`, `-O2 && other`, `-O2 > file`, "-O2\nother",
		`-DNAME=$(other)`, "-DNAME=`other`", `-DNAME="$VARIABLE"`, `-DNAME=$VARIABLE`,
		`*.c`, `a?.c`, `[ab].c`, `~/.config`, `{a,b}`, `# comment`,
		`-DNAME='unterminated`, `-DNAME="unterminated`, `-DNAME=trailing\`,
		"-DNAME=\x00value", "-DNAME=\x01unknown\x02", "-DNAME=" + compactKbuildLiteralDollarToken,
		"-DNAME=\x03broken", "-DNAME=\x04broken", "-DNAME=\rvalue", "-DNAME=\x7fvalue",
		"-DNAME='quoted\tcontrol'", "-DNAME='quoted\ncontrol'",
		strings.Repeat("a", MaxProbeInterpolatedBytes+1), strings.Repeat("a ", MaxProbeDynamicArgumentWords+1),
	} {
		if got, err := ParseProbeSourceShellWords(text); err == nil || got != nil {
			t.Fatalf("accepted unsafe source-shell words of length %d: %q, %v", len(text), got, err)
		}
	}
}

func TestParseProbeSourceShellWordsFollowsMakeTransforms(t *testing.T) {
	// Finalization before filter-out would match this public-looking suffix
	// and incorrectly remove the private-root operand. Apply Make first.
	input := "-DPATH='\"" + compactKbuildActionObjectTreeMarker + "/asm-offsets\"'"
	transform := ProbeValueTransform{Function: "filter-out", Arguments: []string{`%OBJECT_TREE__/asm-offsets"'`, ""}, InputArgument: 1}
	filtered, err := ApplyProbeValueTransform(transform, input)
	if err != nil || filtered != input {
		t.Fatalf("private-root Make filter = %q, %v", filtered, err)
	}
	got, err := ParseProbeSourceShellWords(filtered)
	want := []string{`-DPATH="__LINUX_BZL_OBJECT_TREE__/asm-offsets"`}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("post-transform source words = %q, %v", got, err)
	}
}

func TestValidateProbeSourceShellLiteralRequiresCompletePrivateMarkers(t *testing.T) {
	for _, value := range []string{
		"", "-O2\t-g\n", compactKbuildActionObjectTreeMarker + "/file",
		compactKbuildActionSourceTreeMarker, compactKbuildActionAbsoluteObjectTreeMarker,
		compactKbuildActionHostDepsTreeMarker,
		compactKbuildLiteralSentinelEscapeByte + "_LINUX_BZL_OBJECT_TREE__/literal",
		compactKbuildLiteralTreeEscapeByte + "{tree:kernel}",
	} {
		if err := ValidateProbeSourceShellLiteral(value); err != nil {
			t.Fatalf("valid complete source literal rejected: %v", err)
		}
	}
	for _, value := range []string{
		"\x01", "\x02", "\x01linux-bzl-action-", "object-tree\x02", "\x03{tree:", "\x04_LINUX_BZL_",
		compactKbuildLiteralDollarToken, "\x00", "\r", "\x7f",
	} {
		if err := ValidateProbeSourceShellLiteral(value); err == nil {
			t.Fatal("partial private marker or control byte admitted as a source literal")
		}
	}
}
