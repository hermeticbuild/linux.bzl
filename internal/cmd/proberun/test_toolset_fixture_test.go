package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
	"github.com/hermeticbuild/linux.bzl/internal/toolaction"
)

// runTestProbe supplies the identity-bound toolset artifacts that Bazel adds
// to production probe actions. Individual runner tests can stay focused on
// their protocol behavior without manufacturing the same manifest repeatedly.
func runTestProbe(t *testing.T, opts probeOptions) error {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	absolute := func(filename string) string {
		if filepath.IsAbs(filename) {
			return filepath.Clean(filename)
		}
		return filepath.Join(workingDirectory, filename)
	}
	descriptor := []string{"scope=" + opts.scope}
	for _, role := range sortedActionContractRoles(opts.tools) {
		descriptor = append(descriptor, "tool="+role+"="+absolute(opts.tools[role].path))
	}
	for _, role := range sortedStringKeys(opts.runtimeTools) {
		descriptor = append(descriptor, "runtime="+role+"="+absolute(opts.runtimeTools[role]))
	}
	for _, filename := range opts.toolsetFiles {
		descriptor = append(descriptor, "closure="+absolute(filename))
	}
	digest := sha256.Sum256([]byte(strings.Join(descriptor, "\n")))
	typedRoot := filepath.Join(workingDirectory, fmt.Sprintf(".proberun-test-toolset-%x", digest[:8]))
	if err := os.MkdirAll(typedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(typedRoot) })

	boundByPath := map[string]string{}
	bind := func(name, filename string) string {
		t.Helper()
		if filename == "" {
			t.Fatalf("empty test toolset artifact %s", name)
		}
		absolutePath := absolute(filename)
		if existing := boundByPath[absolutePath]; existing != "" {
			return existing
		}
		if probePhysicalPathWithin(workingDirectory, absolutePath) {
			boundByPath[absolutePath] = absolutePath
			return absolutePath
		}
		artifactDigest := sha256.Sum256([]byte(absolutePath))
		artifactRoot := filepath.Join(typedRoot, fmt.Sprintf("artifact-%x", artifactDigest[:8]))
		if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
			t.Fatalf("create test toolset artifact directory %s: %v", name, err)
		}
		bound := filepath.Join(artifactRoot, filepath.Base(absolutePath))
		if existing, err := filepath.EvalSymlinks(bound); err == nil {
			want, wantErr := filepath.EvalSymlinks(absolutePath)
			if wantErr != nil || filepath.Clean(existing) != filepath.Clean(want) {
				t.Fatalf("test toolset artifact %s conflicts with existing binding %q", name, bound)
			}
			boundByPath[absolutePath] = bound
			return bound
		}
		if err := os.Symlink(absolutePath, bound); err != nil {
			t.Fatalf("bind test toolset artifact %s: %v", name, err)
		}
		boundByPath[absolutePath] = bound
		return bound
	}

	roles := map[string]string{}
	if opts.runtimeTools == nil {
		opts.runtimeTools = map[string]string{}
	}
	for role, contract := range opts.tools {
		contract.path = bind("tool-"+role, contract.path)
		opts.tools[role] = contract
		roles[role] = contract.path
	}
	for role, filename := range opts.runtimeTools {
		bound := bind("runtime-"+role, filename)
		opts.runtimeTools[role] = bound
		if selected := roles[role]; selected != "" && selected != bound {
			t.Fatalf("test tool role %s and runtime tool select different artifacts", role)
		}
		roles[role] = bound
	}
	if len(roles) == 0 {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		roles["cc"] = bind("runtime-cc", executable)
	}
	for role, filename := range roles {
		opts.runtimeTools[role] = filename
	}

	closureFiles := make([]string, 0, len(boundByPath)+len(opts.toolsetFiles))
	closureFiles = append(closureFiles, sortedStringValues(boundByPath)...)
	for index, filename := range opts.toolsetFiles {
		bound := bind(fmt.Sprintf("closure-%d", index), filename)
		found := false
		for _, existing := range closureFiles {
			if existing == bound {
				found = true
				break
			}
		}
		if !found {
			closureFiles = append(closureFiles, bound)
		}
	}
	canonicalByFile := map[string]string{}
	artifactKinds := map[string]string{}
	closure := make([]string, 0, len(closureFiles))
	for _, filename := range closureFiles {
		canonical, err := canonicalActionArtifactPath(workingDirectory, filename)
		if err != nil {
			t.Fatalf("canonicalize test toolset artifact %q: %v", filename, err)
		}
		canonicalByFile[filename] = canonical
		closure = append(closure, canonical)
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatalf("inspect test toolset artifact %q: %v", filename, err)
		}
		artifactKinds[canonical] = testToolsetArtifactKind(true, info.IsDir())
	}
	sort.Strings(closure)
	actionValueResolver := &toolsetPathResolver{}
	for _, filename := range closureFiles {
		info, err := os.Stat(filename)
		if err != nil {
			t.Fatalf("inspect test toolset artifact %q: %v", filename, err)
		}
		actionValueResolver.ordered = append(actionValueResolver.ordered, toolsetArtifactBinding{
			canonical: canonicalByFile[filename],
			path:      filename,
			directory: info.IsDir(),
			source:    true,
		})
	}

	manifest := toolaction.KbuildToolsetManifest{
		Schema:        toolaction.KbuildToolsetManifestSchema,
		Scope:         opts.scope,
		Actions:       map[string][]string{},
		Tools:         map[string]string{},
		Closure:       closure,
		ArtifactKinds: artifactKinds,
		Environments:  map[string]map[string]string{},
		MakeVariables: map[string]string{},
		Requirements:  map[string]map[string]string{},
	}
	roleNames := make([]string, 0, len(roles))
	for role := range roles {
		roleNames = append(roleNames, role)
	}
	sort.Strings(roleNames)
	for _, role := range roleNames {
		contract := opts.tools[role]
		manifest.Actions[role] = make([]string, len(contract.arguments))
		for index, argument := range contract.arguments {
			manifest.Actions[role][index] = testManifestActionValue(t, actionValueResolver, argument)
		}
		manifest.Tools[role] = canonicalByFile[roles[role]]
		manifest.Environments[role] = map[string]string{}
		for name, value := range contract.environment {
			manifest.Environments[role][name] = testManifestActionValue(t, actionValueResolver, value)
		}
		manifest.Requirements[role] = map[string]string{}
	}
	identity, err := manifest.Identity()
	if err != nil {
		t.Fatalf("build test toolset manifest: %v", err)
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(typedRoot, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(typedRoot, identity)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	opts.toolsetManifest = manifestPath
	opts.toolsetFiles = closureFiles
	if opts.toolsetMarkers == nil {
		opts.toolsetMarkers = map[string]string{}
	}
	opts.toolsetMarkers[opts.scope] = marker

	for _, filename := range opts.inputs {
		input, err := kconfig.ReadProbeResult(filename)
		if err != nil {
			t.Fatalf("prepare test probe input: %v", err)
		}
		if input.Scope == opts.scope {
			input.ToolsetIdentity = identity
		} else {
			otherMarker := filepath.Join(typedRoot, input.ToolsetIdentity)
			if err := os.WriteFile(otherMarker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			opts.toolsetMarkers[input.Scope] = otherMarker
		}
		data, err := input.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return runProbe(opts)
}

func testToolsetArtifactKind(source, directory bool) string {
	if source {
		return toolaction.KbuildToolsetArtifactSource
	}
	if directory {
		return toolaction.KbuildToolsetArtifactGeneratedDirectory
	}
	return toolaction.KbuildToolsetArtifactGeneratedFile
}

func testManifestActionValue(t *testing.T, resolver *toolsetPathResolver, value string) string {
	t.Helper()
	value = strings.ReplaceAll(value, toolaction.ExecutionRootMarker+"/", "")
	canonical, err := resolver.canonicalActionValue(value)
	if err != nil {
		t.Fatalf("canonicalize test manifest action value: %v", err)
	}
	return canonical
}

func sortedActionContractRoles(values map[string]actionContract) []string {
	roles := make([]string, 0, len(values))
	for role := range values {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}
