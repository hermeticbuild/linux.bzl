package toolsetpath

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

type resolverFixture struct {
	execroot     string
	projection   string
	identities   []string
	manifests    []string
	anchors      []string
	physical     map[string]string
	documents    map[string]toolaction.KbuildToolsetManifest
	manifestPath map[string]string
}

func TestResolverDerivesMappedRootsAndProjectsCanonicalAncestors(t *testing.T) {
	fixture := newResolverFixture(t, true)
	resolver, err := LoadFlags(fixture.execroot, fixture.projection, fixture.identities, fixture.manifests, fixture.anchors)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		scope, canonical, want string
	}{
		{"target", "external/toolchain/bin/cc", fixture.physical["target:cc"]},
		{"target", "external/toolchain/include/generated.h", fixture.physical["target:header"]},
		{"target", "external/toolchain/source.txt", fixture.physical["target:source"]},
		{"target", "include", fixture.physical["target:include"]},
		{"host", "external/toolchain/bin/cc", fixture.physical["host:cc"]},
	} {
		got, err := resolver.Resolve(test.scope, test.canonical)
		if err != nil {
			t.Fatalf("Resolve(%s, %s): %v", test.scope, test.canonical, err)
		}
		if filepath.Clean(got) != filepath.Clean(test.want) {
			t.Fatalf("Resolve(%s, %s) = %q, want %q", test.scope, test.canonical, got, test.want)
		}
	}

	treeHeader := filepath.Join(fixture.physical["target:tree"], "usr", "include", "tree.h")
	got, err := resolver.Resolve("target", "external/toolchain/sysroot/usr/include/tree.h")
	if err != nil {
		t.Fatal(err)
	}
	if got != treeHeader {
		t.Fatalf("tree descendant = %q, want %q", got, treeHeader)
	}

	projected, err := resolver.Resolve("target", "external/toolchain")
	if err != nil {
		t.Fatal(err)
	}
	if !pathWithin(filepath.Join(fixture.projection, "target"), projected) {
		t.Fatalf("projection %q is outside private root", projected)
	}
	assertResolvedPath(t, filepath.Join(projected, "bin", "cc"), fixture.physical["target:cc"])
	assertResolvedPath(t, filepath.Join(projected, "include", "generated.h"), fixture.physical["target:header"])
	assertResolvedPath(t, filepath.Join(projected, "source.txt"), fixture.physical["target:source"])
	assertResolvedPath(t, filepath.Join(projected, "sysroot", "usr", "include", "tree.h"), treeHeader)

	// A narrower projection after its ancestor reuses only identical links.
	nested, err := resolver.Resolve("target", "external/toolchain/include")
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedPath(t, filepath.Join(nested, "generated.h"), fixture.physical["target:header"])
}

func TestResolverHandoffPreservesScopedAuthority(t *testing.T) {
	fixture := newResolverFixture(t, true)
	resolver, err := LoadFlags(fixture.execroot, fixture.projection, fixture.identities, fixture.manifests, fixture.anchors)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := resolver.CreateHandoff(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadHandoff(handoff, filepath.Join(t.TempDir(), "projection"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/toolchain/bin/cc")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Rewrite("cc="+token, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if want := "cc=" + fixture.physical["target:cc"]; got != want {
		t.Fatalf("Rewrite() = %q, want %q", got, want)
	}

	unknown, err := toolaction.EncodeExecutionRootProvenancePath("host", "external/toolchain/missing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Rewrite(unknown, reloaded); err == nil || !strings.Contains(err.Error(), "outside the identity-bound host toolset closure") {
		t.Fatalf("Rewrite(unknown) error = %v", err)
	}
}

func TestLoadFlagsRejectsMalformedOrIncompleteRootAnchors(t *testing.T) {
	fixture := newResolverFixture(t, false)
	tests := []struct {
		name                           string
		identities, manifests, anchors []string
		want                           string
	}{
		{name: "no scopes", want: "contain no scopes"},
		{name: "unknown identity scope", identities: []string{"build=x"}, want: "unknown scope"},
		{name: "unknown anchor scope", anchors: []string{"build=root=/tmp/a"}, want: "unknown scope"},
		{name: "malformed anchor", anchors: []string{"target=root"}, want: "SCOPE=ROOT=PATH"},
		{name: "invalid root", anchors: []string{"target=bad/root=/tmp/a"}, want: "canonical root ID"},
		{name: "missing manifest", identities: fixture.identities, anchors: fixture.anchors, want: "exactly one identity"},
		{name: "missing anchors", identities: fixture.identities, manifests: fixture.manifests, want: "one or more typed root anchors"},
		{name: "duplicate identity", identities: append(append([]string{}, fixture.identities...), fixture.identities[0]), manifests: fixture.manifests, anchors: fixture.anchors, want: "repeats identity"},
		{name: "duplicate anchor", identities: fixture.identities, manifests: fixture.manifests, anchors: append(append([]string{}, fixture.anchors...), fixture.anchors[0]), want: "repeats anchor root"},
		{name: "wrong anchor count", identities: fixture.identities, manifests: fixture.manifests, anchors: fixture.anchors[:len(fixture.anchors)-1], want: "typed root anchors"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadFlags(fixture.execroot, fixture.projection, test.identities, test.manifests, test.anchors)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadFlags() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestScopedRootValuesPreservesEqualsInPhysicalPath(t *testing.T) {
	got, err := scopedRootValues("anchor", []string{"target=bin=/tmp/mapped=config/tool"})
	if err != nil {
		t.Fatal(err)
	}
	if got["target"]["bin"] != "/tmp/mapped=config/tool" {
		t.Fatalf("scopedRootValues() = %#v", got)
	}
}

func TestLoadFlagsRejectsAnchorWithWrongCanonicalIdentity(t *testing.T) {
	fixture := newResolverFixture(t, false)
	unbound := filepath.Join(fixture.execroot, "bazel-out", "arm64-fastbuild", "bin", "external", "toolchain", "unbound")
	writeArtifact(t, unbound, false)
	anchors := append([]string{}, fixture.anchors...)
	for index, value := range anchors {
		if strings.HasPrefix(value, "target=bin=") {
			anchors[index] = "target=bin=" + unbound
		}
	}
	_, err := LoadFlags(fixture.execroot, fixture.projection, fixture.identities, fixture.manifests, anchors)
	if err == nil || !strings.Contains(err.Error(), "anchor resolves to") {
		t.Fatalf("LoadFlags(wrong anchor) error = %v", err)
	}
}

func TestLoadFlagsRejectsArtifactThatDoesNotReconstructToManifestCanonicalPath(t *testing.T) {
	fixture := newResolverFixture(t, false)
	manifest := fixture.documents["target"]
	manifest.ArtifactRoots["include"] = toolaction.KbuildToolsetArtifactRoot{Root: "bin", Path: "wrong/include"}
	fixture.updateManifest(t, "target", manifest)
	_, err := LoadFlags(fixture.execroot, fixture.projection, fixture.identities, fixture.manifests, fixture.anchors)
	if err == nil || !strings.Contains(err.Error(), "reconstructs as canonical path") {
		t.Fatalf("LoadFlags(inconsistent location) error = %v", err)
	}
}

func TestLoadFlagsStatsEveryReconstructedArtifact(t *testing.T) {
	fixture := newResolverFixture(t, false)
	if err := os.Remove(fixture.physical["target:include"]); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFlags(fixture.execroot, fixture.projection, fixture.identities, fixture.manifests, fixture.anchors)
	if err == nil || !strings.Contains(err.Error(), `inspect target toolset artifact "include"`) {
		t.Fatalf("LoadFlags(missing closure artifact) error = %v", err)
	}
}

func TestLoadFlagsChecksGeneratedKindsButAllowsOpaqueSourceDirectories(t *testing.T) {
	t.Run("generated kind mismatch", func(t *testing.T) {
		fixture := newResolverFixture(t, false)
		manifest := fixture.documents["target"]
		manifest.ArtifactKinds["include"] = toolaction.KbuildToolsetArtifactGeneratedDirectory
		fixture.updateManifest(t, "target", manifest)
		_, err := LoadFlags(fixture.execroot, fixture.projection, fixture.identities, fixture.manifests, fixture.anchors)
		if err == nil || !strings.Contains(err.Error(), `manifest binds kind "generated-directory"`) {
			t.Fatalf("LoadFlags(kind mismatch) error = %v", err)
		}
	})

	t.Run("source directory", func(t *testing.T) {
		fixture := newResolverFixture(t, false)
		source := fixture.physical["target:source"]
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
		writeArtifact(t, source, true)
		resolver, err := LoadFlags(fixture.execroot, fixture.projection, fixture.identities, fixture.manifests, fixture.anchors)
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolver.Resolve("target", "external/toolchain/source.txt")
		if err != nil || got != source {
			t.Fatalf("Resolve(source directory) = %q, %v; want %q", got, err, source)
		}
	})
}

func TestArtifactPhysicalRootStripsExactManifestSuffix(t *testing.T) {
	root := filepath.Join(t.TempDir(), "physical-root")
	anchor := filepath.Join(root, "external", "toolchain", "bin", "cc")
	got, err := artifactPhysicalRoot(anchor, "external/toolchain/bin/cc")
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("artifactPhysicalRoot() = %q, want %q", got, root)
	}
	for _, relative := range []string{"../cc", "external/toolchain/other"} {
		if _, err := artifactPhysicalRoot(anchor, relative); err == nil {
			t.Fatalf("artifactPhysicalRoot(%q) unexpectedly succeeded", relative)
		}
	}
}

func TestRewriteWithoutBoundScopeFailsClosedOnlyForTokens(t *testing.T) {
	if got, err := Rewrite("ordinary", nil); err != nil || got != "ordinary" {
		t.Fatalf("Rewrite(ordinary, nil) = %q, %v", got, err)
	}
	token, err := toolaction.EncodeExecutionRootProvenancePath("target", "external/toolchain/include")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Rewrite(token, nil); err == nil || !strings.Contains(err.Error(), "no bound toolset scopes") {
		t.Fatalf("Rewrite(token, nil) error = %v", err)
	}
}

func TestRewriteShellUsesDeterministicDescriptorAlias(t *testing.T) {
	const canonical = "external/toolchain/include/generated.h"
	physical := filepath.Join(t.TempDir(), "mapped $(touch should-not-run) `echo nope` root's files", "generated header.h")
	writeArtifact(t, physical, false)
	projectionRoot := filepath.Join(t.TempDir(), "projection")
	identity := "sha256-" + strings.Repeat("a", 64)
	binding := artifactBinding{canonical: canonical, path: physical}
	scope := &scopeResolver{
		scope:          "target",
		identity:       identity,
		exact:          map[string]artifactBinding{canonical: binding},
		ordered:        []artifactBinding{binding},
		projectionRoot: filepath.Join(projectionRoot, "target"),
		projections:    map[string]string{},
	}
	resolver := &Resolver{
		byScope:        map[string]*scopeResolver{"target": scope},
		projectionRoot: projectionRoot,
	}
	token, err := toolaction.EncodeExecutionRootProvenancePath("target", canonical)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RewriteShell("cc -I"+token+" input.c", resolver)
	if err != nil {
		t.Fatal(err)
	}
	alias := ShellAliasRootPath + "/aliases/target/" + identity + "/" + canonical
	want := "cc -I'" + alias + "' input.c"
	if got != want {
		t.Fatalf("RewriteShell() = %q, want %q", got, want)
	}
	if strings.Contains(got, physical) || strings.ContainsAny(alias, " '"+"\t\r\n$()`\\\"") {
		t.Fatalf("visible shell alias %q exposes or inherits hostile physical syntax", got)
	}
	backing := filepath.Join(projectionRoot, "aliases", "target", identity, filepath.FromSlash(canonical))
	assertResolvedPath(t, backing, physical)
	for _, test := range []struct {
		name, source, want string
	}{
		{
			name:   "single quoted fragment",
			source: "printf '%s' 'prefix " + token + " suffix'\n",
			want:   "printf '%s' 'prefix " + alias + " suffix'\n",
		},
		{
			name:   "double quoted fragment",
			source: "printf '%s' \"prefix " + token + " suffix\"\n",
			want:   "printf '%s' \"prefix " + alias + " suffix\"\n",
		},
		{
			name:   "after closed double quoted parameter expansion",
			source: "value=\"${name#prefix}\"\nprintf '%s' \"prefix " + token + " suffix\"\n",
			want:   "value=\"${name#prefix}\"\nprintf '%s' \"prefix " + alias + " suffix\"\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := RewriteShell(test.source, resolver)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("RewriteShell() = %q, want %q", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name, source, want string
	}{
		{name: "parameter expansion", source: "printf '%s' ${value:-" + token + " }\n", want: "parameter expansion"},
		{name: "parameter expansion after quoted brace", source: "printf '%s' ${value:-\"}\"" + token + " }\n", want: "parameter expansion"},
		{name: "double quoted parameter expansion", source: "printf '%s' \"${value:-" + token + " }\"\n", want: "parameter expansion"},
		{name: "nested double quoted parameter expansion", source: "printf '%s' \"${outer:-${inner:-" + token + " }}\"\n", want: "parameter expansion"},
		{name: "heredoc", source: "cat <<EOF\n" + token + "\nEOF\n", want: "heredoc"},
		{name: "continued heredoc", source: "cat <\\\n<EOF\n" + token + "\nEOF\n", want: "heredoc"},
		{name: "command substitution", source: "printf '%s' $(printf '%s' " + token + " )\n", want: "command or process substitution"},
		{name: "continued command substitution", source: "printf '%s' $\\\n(printf '%s' " + token + " )\n", want: "command or process substitution"},
		{name: "carriage return boundary", source: "printf '%s' " + token + "\r\n", want: "POSIX shell word boundary"},
		{name: "vertical tab boundary", source: "printf '%s' " + token + "\v", want: "POSIX shell word boundary"},
		{name: "form feed boundary", source: "printf '%s' " + token + "\f", want: "POSIX shell word boundary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RewriteShell(test.source, resolver); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RewriteShell() error = %v, want containing %q", err, test.want)
			}
		})
	}

	for _, source := range []string{
		"value=${name#prefix}\nprintf '%s' " + token + "\n",
		"value=literal#suffix\nprintf '%s' " + token + "\n",
	} {
		if _, err := RewriteShell(source, resolver); err != nil {
			t.Fatalf("RewriteShell(%q) rejected an ordinary number sign: %v", source, err)
		}
	}
}

func TestRewriteShellAliasPreservesClosureHierarchyAcrossPhysicalRoots(t *testing.T) {
	const launcher = "external/rules_rs++toolchains+stable/bin/rustc"
	const runfiles = launcher + ".runfiles"
	const resource = runfiles + "/rules_rs+/lib/data.txt"
	const unrelatedUnsafeSibling = "external/rules_python++python+python_3_11/lib/script (dev).tmpl"
	identity := "sha256-" + strings.Repeat("b", 64)
	token, err := toolaction.EncodeExecutionRootProvenancePath("target", launcher)
	if err != nil {
		t.Fatal(err)
	}

	type materialized struct {
		rewritten      string
		projectionRoot string
		launcher       string
		resource       string
	}
	makeResolver := func(hostile string) materialized {
		physicalRoot := filepath.Join(t.TempDir(), hostile)
		physicalLauncher := filepath.Join(physicalRoot, "rust compiler")
		physicalRunfiles := filepath.Join(physicalRoot, "rust compiler.runfiles")
		physicalResource := filepath.Join(physicalRunfiles, "rules_rs+", "lib", "data.txt")
		physicalUnsafeSibling := filepath.Join(physicalRoot, "script (dev).tmpl")
		writeArtifact(t, physicalLauncher, false)
		writeArtifact(t, physicalResource, false)
		writeArtifact(t, physicalUnsafeSibling, false)
		bindings := []artifactBinding{
			{canonical: launcher, path: physicalLauncher},
			{canonical: runfiles, path: physicalRunfiles, directory: true},
			{canonical: resource, path: physicalResource},
			{canonical: unrelatedUnsafeSibling, path: physicalUnsafeSibling},
		}
		exact := map[string]artifactBinding{}
		for _, binding := range bindings {
			exact[binding.canonical] = binding
		}
		projectionRoot := filepath.Join(t.TempDir(), "projection")
		resolver := &Resolver{
			projectionRoot: projectionRoot,
			byScope: map[string]*scopeResolver{"target": {
				scope:          "target",
				identity:       identity,
				exact:          exact,
				ordered:        bindings,
				projectionRoot: filepath.Join(projectionRoot, "target"),
				projections:    map[string]string{},
			}},
		}
		rewritten, err := RewriteShell("exec "+token+" --version\n", resolver)
		if err != nil {
			t.Fatal(err)
		}
		root, err := resolver.OpenShellAliasRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := root.Close(); err != nil {
			t.Fatal(err)
		}
		return materialized{
			rewritten:      rewritten,
			projectionRoot: projectionRoot,
			launcher:       physicalLauncher,
			resource:       physicalResource,
		}
	}

	first := makeResolver("mapped $(printf first) root's files")
	second := makeResolver("mapped `printf second` root's files")
	visible := ShellAliasRootPath + "/aliases/target/" + identity + "/" + launcher
	want := "exec '" + visible + "' --version\n"
	if first.rewritten != want || second.rewritten != want {
		t.Fatalf("physical-root-independent aliases = %q and %q, want %q", first.rewritten, second.rewritten, want)
	}
	for _, item := range []materialized{first, second} {
		backingRoot := filepath.Join(item.projectionRoot, "aliases", "target", identity)
		assertResolvedPath(t, filepath.Join(backingRoot, filepath.FromSlash(launcher)), item.launcher)
		data, err := os.ReadFile(filepath.Join(backingRoot, filepath.FromSlash(resource)))
		if err != nil {
			t.Fatalf("read runfiles resource through canonical alias hierarchy: %v", err)
		}
		if string(data) != item.resource {
			t.Fatalf("runfiles resource = %q, want %q", data, item.resource)
		}
	}
}

func TestRewriteShellRejectsUnsafeVisibleCanonicalComponent(t *testing.T) {
	const canonical = "external/toolchain/include/generated header.h"
	physical := filepath.Join(t.TempDir(), "generated header.h")
	writeArtifact(t, physical, false)
	binding := artifactBinding{canonical: canonical, path: physical}
	resolver := &Resolver{
		projectionRoot: filepath.Join(t.TempDir(), "projection"),
		byScope: map[string]*scopeResolver{"target": {
			scope: "target", identity: "sha256-" + strings.Repeat("c", 64),
			exact: map[string]artifactBinding{canonical: binding}, ordered: []artifactBinding{binding},
			projections: map[string]string{},
		}},
	}
	token, err := toolaction.EncodeExecutionRootProvenancePath("target", canonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RewriteShell("cat "+token+"\n", resolver); err == nil || !strings.Contains(err.Error(), "conservative shell alias alphabet") {
		t.Fatalf("RewriteShell(unsafe canonical path) error = %v", err)
	}
}

func newResolverFixture(t *testing.T, host bool) *resolverFixture {
	t.Helper()
	workspace := t.TempDir()
	execroot := filepath.Join(workspace, "execroot")
	if err := os.MkdirAll(execroot, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := &resolverFixture{
		execroot:     execroot,
		projection:   filepath.Join(workspace, "projection"),
		physical:     map[string]string{},
		documents:    map[string]toolaction.KbuildToolsetManifest{},
		manifestPath: map[string]string{},
	}
	addScope := func(scope, config string, complete bool) {
		binRoot := filepath.Join(execroot, "bazel-out", config, "bin")
		tool := filepath.Join(binRoot, "external", "toolchain", "bin", "cc")
		writeArtifact(t, tool, false)
		fixture.physical[scope+":cc"] = tool
		closure := []string{"external/toolchain/bin/cc"}
		kinds := map[string]string{"external/toolchain/bin/cc": toolaction.KbuildToolsetArtifactGeneratedFile}
		locations := map[string]toolaction.KbuildToolsetArtifactRoot{
			"external/toolchain/bin/cc": {Root: "bin", Path: "external/toolchain/bin/cc"},
		}
		roots := map[string]string{"bin": "external/toolchain/bin/cc"}
		anchors := map[string]string{"bin": tool}
		if complete {
			genfilesRoot := filepath.Join(execroot, "bazel-out", config, "genfiles")
			header := filepath.Join(genfilesRoot, "external", "toolchain", "include", "generated.h")
			tree := filepath.Join(binRoot, "external", "toolchain", "sysroot")
			include := filepath.Join(binRoot, "include")
			// A source artifact from a sibling repository deliberately has a
			// different root-relative spelling from its canonical namespace.
			sourceRoot := filepath.Join(workspace, "toolchain")
			source := filepath.Join(sourceRoot, "source.txt")
			writeArtifact(t, header, false)
			writeArtifact(t, filepath.Join(tree, "usr", "include", "tree.h"), false)
			writeArtifact(t, include, false)
			writeArtifact(t, source, false)
			fixture.physical[scope+":header"] = header
			fixture.physical[scope+":tree"] = tree
			fixture.physical[scope+":include"] = include
			fixture.physical[scope+":source"] = source
			closure = append(closure, "external/toolchain/include/generated.h", "external/toolchain/source.txt", "external/toolchain/sysroot", "include")
			kinds["external/toolchain/include/generated.h"] = toolaction.KbuildToolsetArtifactGeneratedFile
			kinds["external/toolchain/source.txt"] = toolaction.KbuildToolsetArtifactSource
			kinds["external/toolchain/sysroot"] = toolaction.KbuildToolsetArtifactGeneratedDirectory
			kinds["include"] = toolaction.KbuildToolsetArtifactGeneratedFile
			locations["external/toolchain/include/generated.h"] = toolaction.KbuildToolsetArtifactRoot{Root: "genfiles", Path: "external/toolchain/include/generated.h"}
			locations["external/toolchain/source.txt"] = toolaction.KbuildToolsetArtifactRoot{Root: "sibling", Path: "source.txt"}
			locations["external/toolchain/sysroot"] = toolaction.KbuildToolsetArtifactRoot{Root: "bin", Path: "external/toolchain/sysroot"}
			locations["include"] = toolaction.KbuildToolsetArtifactRoot{Root: "bin", Path: "include"}
			roots["genfiles"] = "external/toolchain/include/generated.h"
			roots["sibling"] = "external/toolchain/source.txt"
			anchors["genfiles"] = header
			anchors["sibling"] = source
		}
		sortStrings(closure)
		manifest := toolaction.KbuildToolsetManifest{
			Schema:        toolaction.KbuildToolsetManifestSchema,
			Scope:         scope,
			Actions:       map[string][]string{"cc": {toolaction.KbuildArgumentsSentinel}},
			Tools:         map[string]string{"cc": "external/toolchain/bin/cc"},
			Closure:       closure,
			ArtifactKinds: kinds,
			ArtifactRoots: locations,
			Roots:         roots,
			Environments:  map[string]map[string]string{"cc": {}},
			MakeVariables: map[string]string{},
			Requirements:  map[string]map[string]string{"cc": {}},
		}
		manifestFilename := filepath.Join(workspace, scope+".json")
		fixture.documents[scope] = manifest
		fixture.manifestPath[scope] = manifestFilename
		fixture.manifests = append(fixture.manifests, scope+"="+manifestFilename)
		fixture.writeManifest(t, scope)
		for root, filename := range anchors {
			fixture.anchors = append(fixture.anchors, scope+"="+root+"="+filename)
		}
	}
	addScope("target", "arm64-fastbuild", true)
	if host {
		addScope("host", "host-opt", false)
	}
	return fixture
}

func (f *resolverFixture) updateManifest(t *testing.T, scope string, manifest toolaction.KbuildToolsetManifest) {
	t.Helper()
	f.documents[scope] = manifest
	f.writeManifest(t, scope)
}

func (f *resolverFixture) writeManifest(t *testing.T, scope string) {
	t.Helper()
	manifest := f.documents[scope]
	identity, err := manifest.Identity()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.manifestPath[scope], data, 0o600); err != nil {
		t.Fatal(err)
	}
	prefix := scope + "="
	for index, value := range f.identities {
		if strings.HasPrefix(value, prefix) {
			f.identities[index] = prefix + identity
			return
		}
	}
	f.identities = append(f.identities, prefix+identity)
}

func writeArtifact(t *testing.T, filename string, directory bool) {
	t.Helper()
	if directory {
		if err := os.MkdirAll(filename, 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(filename), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertResolvedPath(t *testing.T, got, want string) {
	t.Helper()
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	if gotResolved != wantResolved {
		t.Fatalf("resolved path = %q, want %q", gotResolved, wantResolved)
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
