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

func TestKbuildFrontierVirtualFileViewReadsImmutableBaselineUntilFrontierReplacesIt(t *testing.T) {
	const releasePath = "include/config/kernel.release"
	baseline := map[string]string{releasePath: "6.18.39-baseline\n"}
	view := kbuildFrontierVirtualFileView{
		directory:         "scripts",
		immutableContents: baseline,
	}

	rooted := kbuildEvalObjectTree + "/" + releasePath
	for _, pathname := range []string{rooted, "../" + releasePath} {
		content, exists, exact, err := view.Read(pathname)
		if err != nil || !exists || !exact || content != baseline[releasePath] {
			t.Fatalf("immutable Read(%q) = (%q, %t, %t, %v)", pathname, content, exists, exact, err)
		}
	}
	if got, want := view.Match(kbuildEvalObjectTree+"/include/config/kernel.*"), []string{rooted}; !slices.Equal(got, want) {
		t.Fatalf("immutable Match() = %q, want %q", got, want)
	}

	view.state = kbuildFrontierSet(view.state, releasePath, testKbuildFrontierValue(releasePath, "6.18.39-generated\n", true))
	content, exists, exact, err := view.Read(rooted)
	if err != nil || !exists || !exact || content != "6.18.39-generated\n" {
		t.Fatalf("frontier replacement read = (%q, %t, %t, %v)", content, exists, exact, err)
	}
}

func TestKbuildFrontierVirtualFileViewReadsAndMatchesNestedSourceOverlay(t *testing.T) {
	const overlay = ".linux-bzl/external/demo"
	embeddedSourceMarker := overlay + "/" + kbuildEvalSourceTree + "/drivers/net/generated"
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: overlay + "/modules.order", value: testKbuildFrontierValue(overlay+"/modules.order", overlay+"/demo.o\n", true)},
		{path: embeddedSourceMarker, value: testKbuildFrontierValue(embeddedSourceMarker, "generated\n", true)},
		{path: "drivers/net/modules.order", value: testKbuildFrontierValue("drivers/net/modules.order", "drivers/net/demo.o\n", true)},
	})
	view := kbuildFrontierVirtualFileView{
		state: state, directory: overlay,
		sourceOverlayDirectories: []string{overlay},
	}

	sourcePath := kbuildEvalSourceTree + "/" + overlay + "/modules.order"
	content, exists, exact, err := view.Read(sourcePath)
	if err != nil || !exists || !exact || content != overlay+"/demo.o\n" {
		t.Fatalf("nested source-overlay read = (%q, %t, %t, %v)", content, exists, exact, err)
	}
	if got, want := view.Match(kbuildEvalSourceTree+"/"+overlay+"/*.order"), []string{sourcePath}; !slices.Equal(got, want) {
		t.Fatalf("nested source-overlay Match() = %q, want %q", got, want)
	}

	ordinarySourcePath := kbuildEvalSourceTree + "/drivers/net/modules.order"
	if _, exists, _, err := view.Read(ordinarySourcePath); err != nil || exists {
		t.Fatalf("ordinary source-tree read unexpectedly resolved through object frontier: exists=%t err=%v", exists, err)
	}
	if got := view.Match(kbuildEvalSourceTree + "/drivers/net/*.order"); len(got) != 0 {
		t.Fatalf("ordinary source-tree Match() unexpectedly resolved through object frontier: %q", got)
	}
	ordinaryGeneratedSourcePath := kbuildEvalSourceTree + "/drivers/net/generated"
	if _, exists, _, err := view.Read(ordinaryGeneratedSourcePath); err != nil || exists {
		t.Fatalf("source-tree read fell through to embedded marker path: exists=%t err=%v", exists, err)
	}
	if got := view.Match(ordinaryGeneratedSourcePath); len(got) != 0 {
		t.Fatalf("source-tree Match() fell through to embedded marker path: %q", got)
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

func TestKbuildFrontierVirtualFileViewTreatsObjectMarkerAsRooted(t *testing.T) {
	nested := "drivers/" + kbuildEvalObjectTree + "/foo.order"
	state := newKbuildFrontierStateFromSorted([]kbuildFrontierEntry{
		{path: "foo.order", value: testKbuildFrontierValue("foo.order", "root\n", true)},
		{path: nested, value: testKbuildFrontierValue(nested, "relative\n", true)},
	})
	view := kbuildFrontierVirtualFileView{state: state, directory: "drivers"}
	if content, exists, exact, err := view.Read(kbuildEvalObjectTree + "/foo.order"); err != nil || !exists || !exact || content != "root\n" {
		t.Fatalf("rooted object-tree read = (%q, %t, %t, %v)", content, exists, exact, err)
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
