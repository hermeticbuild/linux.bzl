package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

func TestExpandProbeParameterFiles(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.params")
	writeToolsetTestFile(t, first, "--scope\ntarget value\n", 0o600)
	second := filepath.Join(dir, "second.params")
	writeToolsetTestFile(t, second, " --preserved exactly \n--last=value\n", 0o600)
	empty := filepath.Join(dir, "empty.params")
	writeToolsetTestFile(t, empty, "", 0o600)

	got, err := expandProbeParameterFiles([]string{
		"literal",
		"@" + first,
		"middle",
		"@" + empty,
		"@" + second,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"literal",
		"--scope",
		"target value",
		"middle",
		" --preserved exactly ",
		"--last=value",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expandProbeParameterFiles() = %#v, want %#v", got, want)
	}
}

func TestExpandProbeParameterFilesRejectsUnsafeInput(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank.params")
	writeToolsetTestFile(t, blank, "first\n\nlast\n", 0o600)
	nested := filepath.Join(dir, "nested.params")
	writeToolsetTestFile(t, nested, "@other.params\n", 0o600)
	carriageReturn := filepath.Join(dir, "cr.params")
	writeToolsetTestFile(t, carriageReturn, "first\r\n", 0o600)

	tests := []struct {
		name      string
		arguments []string
		wantError string
	}{
		{name: "empty filename", arguments: []string{"@"}, wantError: "empty probe parameter-file path"},
		{name: "missing file", arguments: []string{"@" + filepath.Join(dir, "missing.params")}, wantError: "open probe parameter file"},
		{name: "empty argument", arguments: []string{"@" + blank}, wantError: "empty, nested, or CR-bearing"},
		{name: "nested parameter file", arguments: []string{"@" + nested}, wantError: "empty, nested, or CR-bearing"},
		{name: "carriage return", arguments: []string{"@" + carriageReturn}, wantError: "empty, nested, or CR-bearing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := expandProbeParameterFiles(test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expandProbeParameterFiles(%q) = %#v, %v; want error containing %q", test.arguments, got, err, test.wantError)
			}
		})
	}
}

func TestCanonicalActionArtifactPathMapsBazelSpellings(t *testing.T) {
	root := t.TempDir()
	execroot := filepath.Join(root, "execroot")
	if err := os.MkdirAll(execroot, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{
			name:     "execroot relative input",
			filename: filepath.Join(execroot, "external", "toolchain", "include", "stddef.h"),
			want:     "external/toolchain/include/stddef.h",
		},
		{
			name:     "mapped bin output",
			filename: filepath.Join(execroot, "bazel-out", "k8-fastbuild", "bin", "external", "toolchain", "bin", "cc"),
			want:     "external/toolchain/bin/cc",
		},
		{
			name:     "mapped genfiles output",
			filename: filepath.Join(execroot, "bazel-out", "host-opt", "genfiles", "generated", "version.h"),
			want:     "generated/version.h",
		},
		{
			name:     "sibling repository",
			filename: filepath.Join(root, "toolchain", "include", "limits.h"),
			want:     "external/toolchain/include/limits.h",
		},
		{
			name:     "relative mapped output",
			filename: filepath.Join("bazel-out", "arm64-opt", "bin", "tools", "objtool"),
			want:     "tools/objtool",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalActionArtifactPath(execroot, test.filename)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("canonicalActionArtifactPath(%q) = %q, want %q", test.filename, got, test.want)
			}
		})
	}

	for _, filename := range []string{
		"",
		execroot,
		filepath.Join(execroot, "bazel-out", "malformed", "output"),
		filepath.Join(filepath.Dir(root), "outside"),
	} {
		if got, err := canonicalActionArtifactPath(execroot, filename); err == nil {
			t.Errorf("canonicalActionArtifactPath(%q) = %q, want error", filename, got)
		}
	}
}

func TestLoadToolsetPathResolverBindsExactManifestMarkerAndClosure(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot,
		fixture.manifest.Scope,
		fixture.identity,
		fixture.manifestFilename,
		fixture.files,
	)
	if err != nil {
		t.Fatal(err)
	}

	assertToolsetPathResolves(t, resolver, "external/toolchain/bin/cc", fixture.tool)
	assertToolsetPathResolves(t, resolver, "external/toolchain/include/a.h", fixture.header)
	assertToolsetPathResolves(t, resolver, "external/toolchain/sysroot", fixture.tree)
	if err := resolver.verifyTool("cc", fixture.tool); err != nil {
		t.Fatalf("verifyTool(cc): %v", err)
	}
	if _, err := resolver.resolve("external/unselected/tool"); err == nil || !strings.Contains(err.Error(), "outside the identity-bound target toolset closure") {
		t.Fatalf("resolve(outside closure) error = %v", err)
	}
}

func TestToolsetPathResolverResolvesTypedTreesAndAncestorDirectories(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot,
		"target",
		fixture.identity,
		fixture.manifestFilename,
		fixture.files,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.setProjectionRoot(filepath.Join(fixture.root, "projection")); err != nil {
		t.Fatal(err)
	}

	assertToolsetPathResolves(
		t,
		resolver,
		"external/toolchain/sysroot/usr/include/tree.h",
		fixture.treeHeader,
	)
	assertToolsetPathResolves(
		t,
		resolver,
		"external/toolchain/include",
		filepath.Dir(fixture.header),
	)
	assertToolsetPathResolves(
		t,
		resolver,
		"external/toolchain/include/nested",
		filepath.Dir(fixture.nestedHeader),
	)
	projected, err := resolver.resolve("external/toolchain")
	if err != nil {
		t.Fatal(err)
	}
	if projected == filepath.Dir(filepath.Dir(fixture.tool)) || !probePhysicalPathWithin(resolver.projectionRoot, projected) {
		t.Fatalf("resolve(toolchain ancestor) = %q, want private projection", projected)
	}
	projectedTool, err := filepath.EvalSymlinks(filepath.Join(projected, "bin", "cc"))
	if err != nil {
		t.Fatal(err)
	}
	wantTool, err := filepath.EvalSymlinks(fixture.tool)
	if err != nil {
		t.Fatal(err)
	}
	if projectedTool != wantTool {
		t.Fatalf("projected tool = %q, want %q", projectedTool, wantTool)
	}

	if _, err := resolver.resolve("external/toolchain/../outside"); err == nil || !strings.Contains(err.Error(), "invalid canonical toolset path") {
		t.Fatalf("resolve(non-canonical path) error = %v", err)
	}
}

func TestLoadToolsetPathResolverRejectsUnboundInputs(t *testing.T) {
	t.Run("missing manifest argument", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		_, err := loadToolsetPathResolver(fixture.execroot, "target", fixture.identity, "", fixture.files)
		if err == nil || !strings.Contains(err.Error(), "has no target toolset manifest") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("missing manifest file", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		_, err := loadToolsetPathResolver(fixture.execroot, "target", fixture.identity, filepath.Join(fixture.root, "missing.json"), fixture.files)
		if err == nil || !strings.Contains(err.Error(), "read toolset manifest") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("wrong marker", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		wrongIdentity := "sha256-" + strings.Repeat("0", 64)
		_, err := loadToolsetPathResolver(fixture.execroot, "target", wrongIdentity, fixture.manifestFilename, fixture.files)
		if err == nil || !strings.Contains(err.Error(), "want marker") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("tampered manifest", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		originalIdentity := fixture.identity
		fixture.manifest.Actions["cc"] = []string{"tampered", toolaction.KbuildArgumentsSentinel}
		writeToolsetTestManifest(t, fixture.manifestFilename, fixture.manifest)
		_, err := loadToolsetPathResolver(fixture.execroot, "target", originalIdentity, fixture.manifestFilename, fixture.files)
		if err == nil || !strings.Contains(err.Error(), "want marker") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("wrong scope", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		fixture.manifest.Scope = "host"
		identity := writeToolsetTestManifest(t, fixture.manifestFilename, fixture.manifest)
		_, err := loadToolsetPathResolver(fixture.execroot, "target", identity, fixture.manifestFilename, fixture.files)
		if err == nil || !strings.Contains(err.Error(), `has scope "host"`) {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("closure cardinality mismatch", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		_, err := loadToolsetPathResolver(fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.files[:len(fixture.files)-1])
		if err == nil || !strings.Contains(err.Error(), "typed artifacts") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("missing typed artifact", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		if err := os.Remove(fixture.header); err != nil {
			t.Fatal(err)
		}
		_, err := loadToolsetPathResolver(fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.files)
		if err == nil || !strings.Contains(err.Error(), "inspect target toolset artifact") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("closure path mismatch", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		unexpected := filepath.Join(filepath.Dir(filepath.Dir(fixture.tool)), "other", "unexpected")
		writeToolsetTestFile(t, unexpected, "unexpected", 0o600)
		files := append([]string(nil), fixture.files...)
		for index, filename := range files {
			if filename == fixture.header {
				files[index] = unexpected
				break
			}
		}
		_, err := loadToolsetPathResolver(fixture.execroot, "target", fixture.identity, fixture.manifestFilename, files)
		if err == nil || !strings.Contains(err.Error(), "omits manifest artifact") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})

	t.Run("artifact kind mismatch", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		fixture.manifest.ArtifactKinds["external/toolchain/include/a.h"] = toolaction.KbuildToolsetArtifactGeneratedDirectory
		identity := writeToolsetTestManifest(t, fixture.manifestFilename, fixture.manifest)
		_, err := loadToolsetPathResolver(fixture.execroot, "target", identity, fixture.manifestFilename, fixture.files)
		if err == nil || !strings.Contains(err.Error(), "manifest binds kind") {
			t.Fatalf("loadToolsetPathResolver(artifact kind mismatch) error = %v", err)
		}
	})

	t.Run("repeated canonical artifact", func(t *testing.T) {
		fixture := newToolsetResolverFixture(t)
		alternateTool := filepath.Join(
			fixture.execroot,
			"bazel-out",
			"alternate-opt",
			"bin",
			"external",
			"toolchain",
			"bin",
			"cc",
		)
		writeToolsetTestFile(t, alternateTool, "alternate", 0o700)
		files := append([]string(nil), fixture.files...)
		for index, filename := range files {
			if filename == fixture.header {
				files[index] = alternateTool
				break
			}
		}
		_, err := loadToolsetPathResolver(fixture.execroot, "target", fixture.identity, fixture.manifestFilename, files)
		if err == nil || !strings.Contains(err.Error(), "repeats canonical artifact") {
			t.Fatalf("loadToolsetPathResolver() error = %v", err)
		}
	})
}

func TestToolsetPathResolverRejectsToolBindingMismatch(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot,
		"target",
		fixture.identity,
		fixture.manifestFilename,
		fixture.files,
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := resolver.verifyTool("ld", fixture.tool); err == nil || !strings.Contains(err.Error(), "absent from the target toolset manifest") {
		t.Fatalf("verifyTool(absent role) error = %v", err)
	}
	if err := resolver.verifyTool("cc", fixture.header); err == nil || !strings.Contains(err.Error(), "manifest selects") {
		t.Fatalf("verifyTool(wrong canonical path) error = %v", err)
	}

	alternateTool := filepath.Join(
		fixture.execroot,
		"bazel-out",
		"alternate-opt",
		"bin",
		"external",
		"toolchain",
		"bin",
		"cc",
	)
	writeToolsetTestFile(t, alternateTool, "different physical tool", 0o700)
	if err := resolver.verifyTool("cc", alternateTool); err == nil || !strings.Contains(err.Error(), "is not its typed manifest artifact") {
		t.Fatalf("verifyTool(wrong physical artifact) error = %v", err)
	}
}

func TestToolsetPathResolverVerifiesIdentityBoundActionContract(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	fixture.manifest.Actions["cc"] = []string{
		"-isystem", "external/toolchain/include", toolaction.KbuildArgumentsSentinel,
	}
	fixture.manifest.Environments["cc"] = map[string]string{
		"RESOURCE_HEADER": "external/toolchain/include/a.h",
	}
	fixture.identity = writeToolsetTestManifest(t, fixture.manifestFilename, fixture.manifest)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.files,
	)
	if err != nil {
		t.Fatal(err)
	}
	contract := actionContract{
		arguments: []string{
			"-isystem", filepath.Dir(fixture.header), toolaction.KbuildArgumentsSentinel,
		},
		environment: map[string]string{"RESOURCE_HEADER": fixture.header},
	}
	if err := resolver.verifyContract("cc", contract); err != nil {
		t.Fatalf("verifyContract(cc): %v", err)
	}

	wrongArguments := contract
	wrongArguments.arguments = append([]string(nil), contract.arguments...)
	wrongArguments.arguments[0] = "-iquote"
	if err := resolver.verifyContract("cc", wrongArguments); err == nil || !strings.Contains(err.Error(), "arguments do not match") {
		t.Fatalf("verifyContract(wrong arguments) error = %v", err)
	}
	wrongEnvironment := contract
	wrongEnvironment.environment = map[string]string{"RESOURCE_HEADER": filepath.Dir(fixture.header)}
	if err := resolver.verifyContract("cc", wrongEnvironment); err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("verifyContract(wrong environment) error = %v", err)
	}
	if err := resolver.verifyContract("ld", contract); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("verifyContract(absent role) error = %v", err)
	}
}

func TestToolsetPathResolverCanonicalizesOnlyRenderedPathOccurrences(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	resolver, err := loadToolsetPathResolver(
		fixture.execroot, "target", fixture.identity, fixture.manifestFilename, fixture.files,
	)
	if err != nil {
		t.Fatal(err)
	}

	headerDirectory := filepath.Dir(fixture.header)
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "exact", value: fixture.header, want: "external/toolchain/include/a.h"},
		{name: "attached option", value: "-I" + headerDirectory, want: "-Iexternal/toolchain/include"},
		{name: "delimited", value: "--header=" + fixture.header, want: "--header=external/toolchain/include/a.h"},
		{name: "ancestor descendant", value: headerDirectory + "/nested/b.h", want: "external/toolchain/include/nested/b.h"},
		{name: "runfiles", value: fixture.tool + ".runfiles/repo/data", want: "external/toolchain/bin/cc.runfiles/repo/data"},
		{name: "embedded prefix", value: "prefix" + fixture.header, want: "prefix" + fixture.header},
		{name: "slash in option prefix", value: "prefix/-I" + fixture.header, want: "prefix/-I" + fixture.header},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolver.canonicalActionValue(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("canonicalActionValue(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestToolsetPathResolverProjectsInconsistentPhysicalAncestors(t *testing.T) {
	root := t.TempDir()
	execroot := filepath.Join(root, "execroot")
	tool := filepath.Join(execroot, "bazel-out", "target-opt", "bin", "external", "toolchain", "bin", "cc")
	header := filepath.Join(execroot, "bazel-out", "host-opt", "bin", "external", "toolchain", "include", "header.h")
	writeToolsetTestFile(t, tool, "tool", 0o700)
	writeToolsetTestFile(t, header, "header", 0o600)
	manifest := testToolsetManifest("target", []string{
		"external/toolchain/bin/cc",
		"external/toolchain/include/header.h",
	})
	manifestFilename := filepath.Join(root, "manifest.json")
	identity := writeToolsetTestManifest(t, manifestFilename, manifest)
	resolver, err := loadToolsetPathResolver(execroot, "target", identity, manifestFilename, []string{tool, header})
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.setProjectionRoot(filepath.Join(root, "projection")); err != nil {
		t.Fatal(err)
	}

	projected, err := resolver.resolve("external/toolchain")
	if err != nil {
		t.Fatal(err)
	}
	for relative, want := range map[string]string{"bin/cc": tool, "include/header.h": header} {
		got, err := filepath.EvalSymlinks(filepath.Join(projected, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("resolve projected %s: %v", relative, err)
		}
		want, err = filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("projected %s = %q, want %q", relative, got, want)
		}
	}

	// Overlapping ancestor projections share one private tree. Resolving them
	// in either order must reuse identical bound symlinks instead of colliding.
	includeProjection, err := resolver.resolve("external/toolchain/include")
	if err != nil {
		t.Fatalf("resolve nested projection after ancestor: %v", err)
	}
	if got, err := filepath.EvalSymlinks(filepath.Join(includeProjection, "header.h")); err != nil || got != header {
		t.Fatalf("nested projection header = %q, %v; want %q", got, err, header)
	}

	reverse, err := loadToolsetPathResolver(execroot, "target", identity, manifestFilename, []string{tool, header})
	if err != nil {
		t.Fatal(err)
	}
	if err := reverse.setProjectionRoot(filepath.Join(root, "reverse-projection")); err != nil {
		t.Fatal(err)
	}
	if _, err := reverse.resolve("external/toolchain/include"); err != nil {
		t.Fatalf("resolve nested projection first: %v", err)
	}
	if _, err := reverse.resolve("external/toolchain"); err != nil {
		t.Fatalf("resolve ancestor projection after nested: %v", err)
	}
}

func TestToolsetPathResolverRejectsTypedDirectorySymlinkEscape(t *testing.T) {
	fixture := newToolsetResolverFixture(t)
	outside := filepath.Join(fixture.root, "outside", "secret.h")
	writeToolsetTestFile(t, outside, "secret", 0o600)
	escape := filepath.Join(fixture.tree, "escape.h")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("create symlink: %v", err)
	}

	resolver, err := loadToolsetPathResolver(
		fixture.execroot,
		"target",
		fixture.identity,
		fixture.manifestFilename,
		fixture.files,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.resolve("external/toolchain/sysroot/escape.h"); err == nil || !strings.Contains(err.Error(), "escapes typed directory") {
		t.Fatalf("resolve(symlink escape) error = %v", err)
	}
}

type toolsetResolverFixture struct {
	root             string
	execroot         string
	manifestFilename string
	manifest         toolaction.KbuildToolsetManifest
	identity         string
	files            []string
	tool             string
	header           string
	nestedHeader     string
	include          string
	tree             string
	treeHeader       string
}

func newToolsetResolverFixture(t *testing.T) toolsetResolverFixture {
	t.Helper()
	root := t.TempDir()
	execroot := filepath.Join(root, "execroot")
	outputRoot := filepath.Join(execroot, "bazel-out", "k8-fastbuild", "bin")
	fixture := toolsetResolverFixture{
		root:             root,
		execroot:         execroot,
		manifestFilename: filepath.Join(root, "manifest.json"),
		tool:             filepath.Join(outputRoot, "external", "toolchain", "bin", "cc"),
		header:           filepath.Join(outputRoot, "external", "toolchain", "include", "a.h"),
		nestedHeader:     filepath.Join(outputRoot, "external", "toolchain", "include", "nested", "b.h"),
		include:          filepath.Join(outputRoot, "external", "toolchain", "include"),
		tree:             filepath.Join(outputRoot, "external", "toolchain", "sysroot"),
	}
	fixture.treeHeader = filepath.Join(fixture.tree, "usr", "include", "tree.h")
	writeToolsetTestFile(t, fixture.tool, "tool", 0o700)
	writeToolsetTestFile(t, fixture.header, "header", 0o600)
	writeToolsetTestFile(t, fixture.nestedHeader, "nested header", 0o600)
	writeToolsetTestFile(t, fixture.treeHeader, "tree header", 0o600)
	fixture.manifest = testToolsetManifest("target", []string{
		"external/toolchain/bin/cc",
		"external/toolchain/include",
		"external/toolchain/include/a.h",
		"external/toolchain/include/nested/b.h",
		"external/toolchain/sysroot",
	})
	fixture.identity = writeToolsetTestManifest(t, fixture.manifestFilename, fixture.manifest)
	// Deliberately use a different order from the canonical manifest closure.
	fixture.files = []string{fixture.tree, fixture.nestedHeader, fixture.tool, fixture.include, fixture.header}
	return fixture
}

func testToolsetManifest(scope string, closure []string) toolaction.KbuildToolsetManifest {
	artifactKinds := make(map[string]string, len(closure))
	for _, path := range closure {
		artifactKinds[path] = toolaction.KbuildToolsetArtifactGeneratedFile
	}
	for _, path := range []string{"external/toolchain/include", "external/toolchain/sysroot"} {
		if _, exists := artifactKinds[path]; exists {
			artifactKinds[path] = toolaction.KbuildToolsetArtifactGeneratedDirectory
		}
	}
	return toolaction.KbuildToolsetManifest{
		Schema: toolaction.KbuildToolsetManifestSchema,
		Scope:  scope,
		Actions: map[string][]string{
			"cc": {toolaction.KbuildArgumentsSentinel},
		},
		Tools:         map[string]string{"cc": "external/toolchain/bin/cc"},
		Closure:       closure,
		ArtifactKinds: artifactKinds,
		Environments:  map[string]map[string]string{"cc": {}},
		MakeVariables: map[string]string{"CC": "cc"},
		Requirements:  map[string]map[string]string{"cc": {}},
	}
}

func writeToolsetTestManifest(t *testing.T, filename string, manifest toolaction.KbuildToolsetManifest) string {
	t.Helper()
	identity, err := manifest.Identity()
	if err != nil {
		t.Fatalf("manifest identity: %v", err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeToolsetTestFile(t, filename, string(append(data, '\n')), 0o600)
	return identity
}

func writeToolsetTestFile(t *testing.T, filename, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertToolsetPathResolves(t *testing.T, resolver *toolsetPathResolver, canonical, want string) {
	t.Helper()
	got, err := resolver.resolve(canonical)
	if err != nil {
		t.Fatalf("resolve(%q): %v", canonical, err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("resolve(%q) = %q, want %q", canonical, got, want)
	}
}
