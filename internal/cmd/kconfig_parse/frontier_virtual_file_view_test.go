package main

import (
	"slices"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestKbuildFrontierVirtualFileViewMatchesOnlyQueriedPrefixes(t *testing.T) {
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "drivers/net/first.o", value: testKbuildFrontierValue("drivers/net/first.o", "first\n", true)},
		{path: "drivers/net/nested/second.o", value: testKbuildFrontierValue("drivers/net/nested/second.o", "second\n", true)},
		{path: "drivers/other/third.o", value: testKbuildFrontierValue("drivers/other/third.o", "", false)},
	})
	view := kbuildFrontierVirtualFileView{state: state, directory: "drivers/net"}

	for _, test := range []struct {
		pattern string
		want    []string
	}{
		{pattern: "*.o", want: []string{"first.o"}},
		{pattern: "nested/*.o", want: []string{"nested/second.o"}},
		{pattern: kbuildEvalObjectTree + "/drivers/net/*.o", want: []string{kbuildEvalObjectTree + "/drivers/net/first.o"}},
		{pattern: "../other/*.o", want: []string{"../other/third.o"}},
		{pattern: "missing/*.o"},
	} {
		t.Run(test.pattern, func(t *testing.T) {
			if got := view.Match(test.pattern); !slices.Equal(got, test.want) {
				t.Fatalf("Match(%q) = %q, want %q", test.pattern, got, test.want)
			}
		})
	}
}

func TestKbuildFrontierVirtualFileViewReadsExactAndOpaqueFiles(t *testing.T) {
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "drivers/net/exact.order", value: testKbuildFrontierValue("drivers/net/exact.order", "first.o\n", true)},
		{path: "drivers/net/opaque.order", value: testKbuildFrontierValue("drivers/net/opaque.order", "", false)},
	})
	view := kbuildFrontierVirtualFileView{state: state, directory: "drivers/net"}

	if content, exists, exact, err := view.Read("exact.order"); err != nil || content != "first.o\n" || !exists || !exact {
		t.Fatalf("relative exact read = (%q, %t, %t, %v)", content, exists, exact, err)
	}
	if content, exists, exact, err := view.Read(kbuildEvalObjectTree + "/drivers/net/opaque.order"); err != nil || content != "" || !exists || exact {
		t.Fatalf("rooted opaque read = (%q, %t, %t, %v)", content, exists, exact, err)
	}
	if _, exists, _, err := view.Read("missing.order"); err != nil || exists {
		t.Fatal("missing read unexpectedly exists")
	}
}

func TestKbuildFrontierVirtualFileViewMatchesBothAliasCoordinateSystems(t *testing.T) {
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "foo.o", value: testKbuildFrontierValue("foo.o", "", false)},
		{path: "drivers/local.o", value: testKbuildFrontierValue("drivers/local.o", "", false)},
	})
	view := kbuildFrontierVirtualFileView{state: state, directory: "drivers"}
	if got, want := view.Match("*/*.o"), []string{
		"../foo.o",
		kbuildEvalObjectTree + "/foo.o",
	}; !slices.Equal(got, want) {
		t.Fatalf("dual-coordinate Match() = %q, want %q", got, want)
	}
}

func TestKbuildFrontierVirtualFileViewReadsLiteralMetacharacters(t *testing.T) {
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "drivers/generated[1].order", value: testKbuildFrontierValue("drivers/generated[1].order", "bracket\n", true)},
		{path: "drivers/literal*.order", value: testKbuildFrontierValue("drivers/literal*.order", "star\n", true)},
	})
	view := kbuildFrontierVirtualFileView{state: state, directory: "drivers"}
	for path, want := range map[string]string{
		"generated[1].order": "bracket\n",
		"literal*.order":     "star\n",
	} {
		content, exists, exact, err := view.Read(path)
		if err != nil || !exists || !exact || content != want {
			t.Fatalf("Read(%q) = (%q, %t, %t, %v), want (%q, true, true, nil)", path, content, exists, exact, err, want)
		}
	}
}

func TestKbuildFrontierVirtualFileViewRejectsConflictingAliasContents(t *testing.T) {
	nested := "drivers/" + kbuildEvalObjectTree + "/foo.order"
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "foo.order", value: testKbuildFrontierValue("foo.order", "root\n", true)},
		{path: nested, value: testKbuildFrontierValue(nested, "relative\n", true)},
	})
	view := kbuildFrontierVirtualFileView{state: state, directory: "drivers"}
	if _, _, _, err := view.Read(kbuildEvalObjectTree + "/foo.order"); err == nil {
		t.Fatal("conflicting direct/relative aliases unexpectedly succeeded")
	}
}

func TestKbuildFrontierArtifactView(t *testing.T) {
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "drivers/first.o", value: testKbuildFrontierValue("drivers/first.o", "", false)},
		{path: "drivers/net/second.o", value: testKbuildFrontierValue("drivers/net/second.o", "", false)},
		{path: "scripts/third.o", value: testKbuildFrontierValue("scripts/third.o", "", false)},
	})
	view := kbuildFrontierArtifactView{state: state}
	if got, want := view.Len(), 3; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	if got, ok := view.Get("drivers/net/second.o"); !ok || got.Path != "drivers/net/second.o" {
		t.Fatalf("Get() = (%#v, %t)", got, ok)
	}
	got := []string{}
	view.Range("drivers/", func(artifact kconfig.CompactKbuildVisibleArtifact) bool {
		got = append(got, artifact.Path)
		return true
	})
	if want := []string{"drivers/first.o", "drivers/net/second.o"}; !slices.Equal(got, want) {
		t.Fatalf("Range() = %q, want %q", got, want)
	}
}

func testKbuildFrontierValue(path, content string, exact bool) kbuildFrontierValue {
	return kbuildFrontierValue{
		artifact: kconfig.CompactKbuildVisibleArtifact{Path: path, Target: path},
		content:  content,
		exact:    exact,
	}
}
