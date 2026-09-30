package kconfig

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"testing"
)

func sourceLookupProfileForTest(t *testing.T, roots map[string]string, locationSet bool) CompactKbuildProfile {
	t.Helper()
	parser := newKbuildParser(nil, t.TempDir())
	parser.sourceRoots = maps.Clone(roots)
	profile := CompactKbuildProfile{Name: "lookup", Directory: "external/module", evaluator: newKbuildTargetEvaluator(parser)}
	if locationSet {
		if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{
			Tree: CompactKbuildInvocationObjectTree, Directory: "build/subdir",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return profile
}

func TestConfigDependencySourceLookupMatchesProfileWithoutRuleIndexes(t *testing.T) {
	for _, mode := range []string{"kernel", "overlay", "direct", "ambiguous", "invalid-overlay", "missing-location", "missing-template"} {
		t.Run(mode, func(t *testing.T) {
			roots := map[string]string{"__LINUX_BZL_SOURCE_TREE__": "/source/linux"}
			switch mode {
			case "overlay", "ambiguous", "missing-location":
				roots["__LINUX_BZL_SOURCE_TREE__/external/module"] = "/source/module"
			case "direct":
				roots["external/module"] = "/source/module"
			case "invalid-overlay":
				roots["__LINUX_BZL_SOURCE_TREE__/../escape"] = "/source/invalid"
			}
			if mode == "ambiguous" {
				roots["external/module"] = "/source/conflict"
				roots["__LINUX_BZL_SOURCE_TREE__/external/module/nested"] = "/source/nested"
			}
			profile := sourceLookupProfileForTest(t, roots, mode != "missing-location")
			if mode == "missing-template" {
				profile.evaluator = nil
			}
			lookup := newConfigDependencySourceLookup(profile)
			for _, logical := range []string{"include/linux/types.h", "external/module/file.c", "external/module/nested/a.h", "external/module-other/a.h", "../escape.c", "/absolute.c", ""} {
				got, ok := lookup.sourcePath(logical)
				// Use the old linear oracle, not the shared indexed resolver.
				want, wantOK := resolveCompactKbuildProfileSourcePathByScanForTest(profile, logical)
				// The historical linear oracle can reject an ambiguous ancestor
				// before visiting a more-specific mount. Pin these cases directly.
				if mode == "ambiguous" {
					switch logical {
					case "external/module/file.c":
						want, wantOK = "", false
					case "external/module/nested/a.h":
						want, wantOK = "/source/nested/a.h", true
					}
				}
				if ok != wantOK || filepath.Clean(got) != filepath.Clean(want) {
					t.Errorf("source %q = (%q, %t), want (%q, %t)", logical, got, ok, want, wantOK)
				}
				gotOverlay, gotErr := lookup.usesSourceOverlay(logical)
				wantOverlay, wantErr := compactKbuildGraphPathUsesSourceOverlay(profile, logical)
				if gotOverlay != wantOverlay || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
					t.Errorf("overlay %q = (%t, %v), want (%t, %v)", logical, gotOverlay, gotErr, wantOverlay, wantErr)
				}
			}
			for _, operand := range []string{"include", "../include", "../../../escape", "__LINUX_BZL_SOURCE_TREE__/include", "${work:root}/include", "${tree:kernel}", "/system/include", "$(unknown)", "=sysroot", "bad\\separator"} {
				got, relative, ok, err := lookup.includePath(operand)
				want, wantRelative, wantOK, wantErr := ResolveCompactKbuildCompilerIncludePath(profile, operand)
				if got != want || relative != wantRelative || ok != wantOK || fmt.Sprint(err) != fmt.Sprint(wantErr) {
					t.Errorf("include %q changed lookup result", operand)
				}
			}
			if profile.evaluator != nil && len(profile.evaluator.plannerRuntimes) != 0 {
				t.Fatal("source-only lookup constructed Make rule indexes")
			}
		})
	}
}

func TestConfigDependencySourceLookupOwnsItsSnapshot(t *testing.T) {
	profile := sourceLookupProfileForTest(t, map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":                 "/source/linux",
		"__LINUX_BZL_SOURCE_TREE__/external/module": "/source/module",
	}, true)
	lookup := newConfigDependencySourceLookup(profile)
	key := configDependencySourceIncludeCacheKeyForLookup(lookup, "include")
	profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__/external/module"] = "/changed/module"
	profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__"] = "/changed/linux"
	profile.Name = "changed"
	profile.Directory = "different/module"
	if err := SetCompactKbuildProfileInvocationLocation(&profile, CompactKbuildInvocationLocation{Tree: CompactKbuildInvocationSourceTree}); err != nil {
		t.Fatal(err)
	}
	if got, ok := lookup.sourcePath("external/module/file.c"); !ok || filepath.ToSlash(got) != "/source/module/file.c" {
		t.Fatalf("source roots escaped snapshot ownership: %q, %t", got, ok)
	}
	if overlay, err := lookup.usesSourceOverlay("external/module/file.c"); !overlay || err != nil {
		t.Fatalf("overlay context escaped snapshot ownership: %t, %v", overlay, err)
	}
	if location, relative, ok, err := lookup.includePath("../include"); !ok || !relative || err != nil || location.Tree != CompactKbuildInvocationObjectTree || location.Directory != "build/include" {
		t.Fatalf("invocation location escaped snapshot ownership: %#v, %t, %t, %v", location, relative, ok, err)
	}
	if lookup.profileName != "lookup" || configDependencySourceIncludeCacheKeyForLookup(lookup, "include") != key {
		t.Fatal("profile identity or source binding identity changed after capture")
	}
	fresh := newConfigDependencySourceLookup(profile)
	if got, ok := fresh.sourcePath("external/module/file.c"); !ok || filepath.ToSlash(got) != "/changed/module/file.c" {
		t.Fatalf("fresh snapshot ignored changed roots: %q, %t", got, ok)
	}
	if configDependencySourceIncludeCacheKeyForLookup(fresh, "include") == key {
		t.Fatal("changed roots retained the old directory-cache identity")
	}
}

func TestConfigDependencySourceLookupDoesNotTurnDirectRootsIntoOverlays(t *testing.T) {
	for _, nested := range []bool{false, true} {
		marker := "external/module"
		if nested {
			marker = "__LINUX_BZL_SOURCE_TREE__/" + marker
		}
		lookup := newConfigDependencySourceLookup(sourceLookupProfileForTest(t, map[string]string{marker: "/source/module"}, true))
		if got, ok := lookup.sourcePath("external/module/file.c"); !ok || filepath.ToSlash(got) != "/source/module/file.c" {
			t.Fatalf("configured source root did not resolve: %q, %t", got, ok)
		}
		if overlay, err := lookup.usesSourceOverlay("external/module/file.c"); err != nil || overlay != nested {
			t.Fatalf("nested=%t: overlay=%t, err=%v", nested, overlay, err)
		}
	}
}

func TestConfigDependencySourceLookupDirectorySnapshotPreservesMountOwnership(t *testing.T) {
	root, nested, replacement := t.TempDir(), t.TempDir(), t.TempDir()
	mustWriteSource(t, root, "include/base.h", "base\n")
	mustWriteSource(t, root, "include/vendor/shadowed.h", "shadowed\n")
	mustWriteSource(t, nested, "owned.h", "owned\n")
	mustWriteSource(t, replacement, "changed.h", "changed\n")
	profile := sourceLookupProfileForTest(t, map[string]string{
		"__LINUX_BZL_SOURCE_TREE__":                root,
		"__LINUX_BZL_SOURCE_TREE__/include/vendor": nested,
	}, true)
	lookup := newConfigDependencySourceLookup(profile)
	profile.evaluator.template.sourceRoots["__LINUX_BZL_SOURCE_TREE__/include/vendor"] = replacement
	cache := newConfigDependencySourceIncludeCache()
	want := []string{"include/base.h", "include/vendor/owned.h"}
	paths, err := configDependencyImmutableSourceIncludePaths(lookup, "include", cache)
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("captured mount enumeration = %v, %v; want %v", paths, err, want)
	}
	paths[0] = "caller-mutation"
	paths, err = configDependencyImmutableSourceIncludePaths(lookup, "include", cache)
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("cached snapshot lost ownership = %v, %v", paths, err)
	}
	paths, err = configDependencyImmutableSourceIncludePaths(newConfigDependencySourceLookup(profile), "include", cache)
	if err != nil || !slices.Equal(paths, []string{"include/base.h", "include/vendor/changed.h"}) {
		t.Fatalf("fresh mount used incompatible cache = %v, %v", paths, err)
	}
}
