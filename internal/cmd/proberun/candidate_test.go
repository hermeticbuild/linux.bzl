package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func TestValidateAndRewriteProbeCandidateArgumentsUsesTypedPaths(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.setProjectionRoot(filepath.Join(t.TempDir(), "projection")); err != nil {
		t.Fatal(err)
	}
	workingDirectory := t.TempDir()
	arguments := []string{
		"-x", "c",
		"-G", "0",
		"-Iexternal/toolchain/include",
		"-include", "external/toolchain/include/a.h",
		"-Wa,-I,external/toolchain/sysroot/usr/include",
		"-E", "-",
	}
	candidates := arguments[2:8]
	indexes := []int{2, 3, 4, 5, 6, 7}
	got, err := validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCC, "",
		arguments, candidates, indexes,
		workingDirectory, workingDirectory, fixture.execroot,
		nil, nil, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "-x" || got[1] != "c" || got[8] != "-E" || got[9] != "-" {
		t.Fatalf("managed arguments changed: %q", got)
	}
	if got[2] != "-G" || got[3] != "0" {
		t.Fatalf("semantic candidate arguments changed: %q", got[2:4])
	}
	if want := "-I" + filepath.Dir(fixture.header); got[4] != want {
		t.Fatalf("joined include = %q, want %q", got[4], want)
	}
	if got[5] != "-include" || got[6] != fixture.header {
		t.Fatalf("forced include = %q, want split path %q", got[5:7], fixture.header)
	}
	if want := "-Wa,-I," + filepath.Join(fixture.tree, "usr", "include"); got[7] != want {
		t.Fatalf("forwarded include = %q, want %q", got[7], want)
	}
}

func TestValidateAndRewriteProbeCandidateArgumentsRejectsRenderedAuthority(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range [][]string{
		{"@dynamic.params"},
		{"-o", "escape.o"},
		{"-fplugin=escape.so"},
		{"-Wl,--script,escape.ld"},
	} {
		indexes := make([]int, len(candidate))
		for index := range indexes {
			indexes[index] = index
		}
		if _, err := validateAndRewriteProbeCandidateArguments(
			"compile", kconfig.ProbeCandidatePolicyCCLink, "",
			candidate, candidate, indexes,
			fixture.root, fixture.root, fixture.execroot,
			nil, nil, resolver,
		); err == nil {
			t.Fatalf("rendered candidate %q was accepted", candidate)
		}
	}
}

func TestCompilerLinkCandidateBindsDeclaredHostLibraries(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	hostRoot := filepath.Join(t.TempDir(), "host-deps")
	libraryDir := filepath.Join(hostRoot, "lib")
	if err := os.MkdirAll(libraryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(libraryDir, "libelf.a")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	arguments := []string{
		"-L__LINUX_BZL_HOST_DEPS__/lib", "-lelf",
		"__LINUX_BZL_HOST_DEPS__/lib/libelf.a",
	}
	indexes := []int{0, 1, 2}
	got, err := validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCCLink, "",
		arguments, arguments, indexes, scratch, scratch, fixture.execroot,
		nil, map[string]string{"host_deps": hostRoot}, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-L" + libraryDir, "-lelf", archive}; !slices.Equal(got, want) {
		t.Fatalf("bound host linker argv = %q, want %q", got, want)
	}
	forwarded := "-Wl,__LINUX_BZL_HOST_DEPS__/lib/libelf.a"
	got, err = validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCCLink, "",
		[]string{forwarded}, []string{forwarded}, []int{0},
		scratch, scratch, fixture.execroot,
		nil, map[string]string{"host_deps": hostRoot}, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-Wl," + archive}; !slices.Equal(got, want) {
		t.Fatalf("bound forwarded host archive = %q, want %q", got, want)
	}
	if _, err := validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCCLink, "",
		[]string{forwarded}, []string{forwarded}, []int{0},
		scratch, scratch, fixture.execroot,
		nil, nil, resolver,
	); err == nil {
		t.Fatal("forwarded host archive with no declared host dependency root was accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "libelf.a"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(hostRoot, "escaped")); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		"-L__LINUX_BZL_HOST_DEPS__/../outside",
		"-L__LINUX_BZL_HOST_DEPS__/escaped",
		"__LINUX_BZL_HOST_DEPS__/escaped/libelf.a",
		"-Wl,__LINUX_BZL_HOST_DEPS__/escaped/libelf.a",
		"-Wl,__LINUX_BZL_HOST_DEPS__/../outside/libelf.a",
		"-Wl,/unbound/libelf.a",
	} {
		if _, err := validateAndRewriteProbeCandidateArguments(
			"compile", kconfig.ProbeCandidatePolicyCCLink, "",
			[]string{candidate}, []string{candidate}, []int{0},
			scratch, scratch, fixture.execroot,
			nil, map[string]string{"host_deps": hostRoot}, resolver,
		); err == nil {
			t.Errorf("undeclared host library path %q was accepted", candidate)
		}
	}
}

func TestValidateAndRewriteProbeCandidateArgumentsConfinesSplitDwarf(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	arguments := []string{"-gsplit-dwarf"}
	if _, err := validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCC, "",
		arguments, arguments, []int{0},
		scratch, scratch, fixture.execroot,
		nil, nil, resolver,
	); err != nil {
		t.Fatalf("private split-DWARF candidate: %v", err)
	}
	if _, err := validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCC, "",
		arguments, arguments, []int{0},
		scratch, t.TempDir(), fixture.execroot,
		nil, nil, resolver,
	); err == nil || !strings.Contains(err.Error(), "private scratch working directory") {
		t.Fatalf("non-private split-DWARF candidate error = %v", err)
	}
}

func TestValidateAndRewriteProbeCandidateArgumentsBindsSanitizerIgnorelist(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	argument := "-fsanitize-ignorelist=/dev/null"
	got, err := validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCC, "",
		[]string{argument}, []string{argument}, []int{0},
		scratch, scratch, fixture.execroot,
		nil, nil, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	empty := strings.TrimPrefix(got[0], "-fsanitize-ignorelist=")
	info, err := os.Stat(empty)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || !probePhysicalPathWithin(scratch, empty) {
		t.Fatalf("private empty ignorelist = %q, info=%v, err=%v", empty, info, err)
	}

	sourceRoot := t.TempDir()
	ignorelists := []string{
		filepath.Join(sourceRoot, "scripts", "ignore,first.scl"),
		filepath.Join(sourceRoot, "scripts", "ignore,second.scl"),
	}
	if err := os.MkdirAll(filepath.Dir(ignorelists[0]), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, ignorelist := range ignorelists {
		if err := os.WriteFile(ignorelist, []byte("fun:source_owned\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	arguments := []string{
		"-fsanitize-ignorelist=" + ignorelists[0],
		"-fsanitize-blacklist=" + ignorelists[1],
	}
	got, err = validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCC, "",
		arguments, arguments, []int{0, 1},
		scratch, scratch, fixture.execroot,
		nil, map[string]string{"linux": sourceRoot}, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range arguments {
		if got[index] != want {
			t.Fatalf("source sanitizer list %d = %q, want %q", index, got[index], want)
		}
	}
}

func TestResolveProbeCandidatePathUsesDeclaredSourceRootAndRejectsEscape(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(t.TempDir(), "linux")
	include := filepath.Join(sourceRoot, "include")
	if err := os.MkdirAll(include, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveProbeCandidatePath(
		kconfig.ProbeCandidatePathInclude, "include", t.TempDir(), sourceRoot, fixture.execroot,
		nil, map[string]string{"linux": sourceRoot}, resolver,
	)
	if err != nil || filepath.Clean(got) != filepath.Clean(include) {
		t.Fatalf("resolve source include = %q, %v; want %q", got, err, include)
	}
	alias := filepath.Join(sourceRoot, "include-alias")
	if err := os.Symlink("include", alias); err != nil {
		t.Fatal(err)
	}
	got, err = resolveProbeCandidatePath(
		kconfig.ProbeCandidatePathInclude, "include-alias", t.TempDir(), sourceRoot, fixture.execroot,
		nil, map[string]string{"linux": sourceRoot}, resolver,
	)
	if err != nil || filepath.Clean(got) != filepath.Clean(include) {
		t.Fatalf("resolved source include symlink = %q, %v; want physical path %q", got, err, include)
	}

	outside := filepath.Join(filepath.Dir(sourceRoot), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProbeCandidatePath(
		kconfig.ProbeCandidatePathInclude, "../outside", t.TempDir(), sourceRoot, fixture.execroot,
		nil, map[string]string{"linux": sourceRoot}, resolver,
	); err == nil || !strings.Contains(err.Error(), "outside declared source") {
		t.Fatalf("source-root escape error = %v", err)
	}
}

func TestResolveProbeCandidatePathAllowsMissingIncludeInsidePrivateScratch(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	include := "__LINUX_BZL_OBJECT_TREE__/tools/objtool/libsubcmd/include"
	got, err := resolveProbeCandidatePath(
		kconfig.ProbeCandidatePathInclude, include,
		scratch, scratch, fixture.execroot,
		nil, nil, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(scratch, filepath.FromSlash(include))
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("missing object-tree include = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("missing object-tree include unexpectedly exists or cannot be inspected: %v", err)
	}
}

func TestResolveProbeCandidatePathRejectsMissingIncludeThroughEscapingSymlink(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(scratch, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveProbeCandidatePath(
		kconfig.ProbeCandidatePathInclude, "escape/not-yet/include",
		scratch, scratch, fixture.execroot,
		nil, nil, resolver,
	); err == nil || !strings.Contains(err.Error(), "outside declared source") {
		t.Fatalf("missing include symlink escape error = %v", err)
	}
}

func TestResolveProbeCandidatePathStillRejectsMissingForcedInclude(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if _, err := resolveProbeCandidatePath(
		kconfig.ProbeCandidatePathForcedInclude, "not-yet/generated.h",
		scratch, scratch, fixture.execroot,
		nil, nil, resolver,
	); err == nil || !strings.Contains(err.Error(), "resolve candidate path") {
		t.Fatalf("missing forced include error = %v", err)
	}
}

func TestValidateAndRewriteProbeCandidateArgumentsRejectsCommaBearingForwardedPath(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.anchors,
	)
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(t.TempDir(), "source,root")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "include"), 0o700); err != nil {
		t.Fatal(err)
	}
	argument := "-Wa,-I,include"
	if _, err := validateAndRewriteProbeCandidateArguments(
		"compile", kconfig.ProbeCandidatePolicyCC, "",
		[]string{argument}, []string{argument}, []int{0},
		t.TempDir(), sourceRoot, fixture.execroot,
		nil, map[string]string{"linux": sourceRoot}, resolver,
	); err == nil || !strings.Contains(err.Error(), "comma-bearing path") {
		t.Fatalf("forwarded comma path error = %v", err)
	}
}
