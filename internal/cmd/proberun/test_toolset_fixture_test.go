package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	return runTestProbeWithToolset(t, opts)
}

func runTestProbeWithToolset(t *testing.T, opts probeOptions, additionalToolsetFiles ...string) error {
	t.Helper()
	return runProbe(prepareTestProbeWithToolset(t, opts, additionalToolsetFiles...))
}

func prepareTestProbeWithToolset(t *testing.T, opts probeOptions, additionalToolsetFiles ...string) probeOptions {
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
	for _, filename := range additionalToolsetFiles {
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
	if roles["script-runtime"] == "" {
		multicall := filepath.Join(typedRoot, "script-runtime")
		if err := os.WriteFile(multicall, []byte("#!/bin/sh\nshift\nexec /bin/sh \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		roles["script-runtime"] = multicall
	}
	rolesByScope := map[string]map[string]string{opts.scope: {}}
	for binding, filename := range roles {
		scope, role, scoped, valid := toolaction.SplitBinding(binding)
		if !valid || scoped && (opts.scope != "target" || scope != "host") {
			t.Fatalf("invalid test tool binding %q for %s probe", binding, opts.scope)
		}
		if !scoped {
			scope, role = opts.scope, binding
		}
		if rolesByScope[scope] == nil {
			rolesByScope[scope] = map[string]string{}
		}
		rolesByScope[scope][role] = filename
	}
	for role, filename := range roles {
		opts.runtimeTools[role] = filename
	}

	additionalBoundFiles := make([]string, 0, len(additionalToolsetFiles))
	for index, filename := range additionalToolsetFiles {
		bound := bind(fmt.Sprintf("closure-%d", index), filename)
		additionalBoundFiles = append(additionalBoundFiles, bound)
	}
	identities := map[string]string{}
	if opts.toolsetMarkers == nil {
		opts.toolsetMarkers = map[string]string{}
	}
	for scope, scopedRoles := range rolesByScope {
		closureFiles := make([]string, 0, len(scopedRoles)+len(additionalBoundFiles))
		for _, filename := range scopedRoles {
			closureFiles = append(closureFiles, filename)
		}
		if scope == opts.scope {
			closureFiles = append(closureFiles, additionalBoundFiles...)
		}
		sort.Strings(closureFiles)
		closureFiles = slices.Compact(closureFiles)
		canonicalByFile := map[string]string{}
		artifactKinds := map[string]string{}
		closure := make([]string, 0, len(closureFiles))
		actionValueResolver := &toolsetPathResolver{}
		for _, filename := range closureFiles {
			canonical, err := canonicalActionArtifactPath(workingDirectory, filename)
			if err != nil {
				t.Fatalf("canonicalize %s test toolset artifact %q: %v", scope, filename, err)
			}
			canonicalByFile[filename] = canonical
			closure = append(closure, canonical)
			info, err := os.Stat(filename)
			if err != nil {
				t.Fatalf("inspect %s test toolset artifact %q: %v", scope, filename, err)
			}
			artifactKinds[canonical] = testToolsetArtifactKind(true, info.IsDir())
			actionValueResolver.ordered = append(actionValueResolver.ordered, toolsetArtifactBinding{
				canonical: canonical, path: filename, directory: info.IsDir(), source: true,
			})
		}
		sort.Strings(closure)
		artifactRoots, roots, anchors := testToolsetRootBindings(t, workingDirectory, closure, closureFiles)
		manifest := toolaction.KbuildToolsetManifest{
			Schema:        toolaction.KbuildToolsetManifestSchema,
			Scope:         scope,
			Actions:       map[string][]string{},
			Tools:         map[string]string{},
			Closure:       closure,
			ArtifactKinds: artifactKinds,
			ArtifactRoots: artifactRoots,
			Roots:         roots,
			Environments:  map[string]map[string]string{},
			MakeVariables: map[string]string{},
			Requirements:  map[string]map[string]string{},
		}
		roleNames := make([]string, 0, len(scopedRoles))
		for role := range scopedRoles {
			roleNames = append(roleNames, role)
		}
		sort.Strings(roleNames)
		for _, role := range roleNames {
			binding := role
			if scope != opts.scope {
				binding = scope + "@" + role
			}
			contract := opts.tools[binding]
			manifest.Actions[role] = make([]string, len(contract.arguments))
			for index, argument := range contract.arguments {
				manifest.Actions[role][index] = testManifestActionValue(t, actionValueResolver, argument)
			}
			manifest.Tools[role] = canonicalByFile[scopedRoles[role]]
			manifest.Environments[role] = map[string]string{}
			for name, value := range contract.environment {
				manifest.Environments[role][name] = testManifestActionValue(t, actionValueResolver, value)
			}
			manifest.Requirements[role] = map[string]string{}
		}
		identity, err := manifest.Identity()
		if err != nil {
			t.Fatalf("build %s test toolset manifest: %v", scope, err)
		}
		manifestData, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(typedRoot, scope+"-manifest.json")
		if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(typedRoot, identity)
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		identities[scope] = identity
		opts.toolsetMarkers[scope] = marker
		if scope == opts.scope {
			opts.toolsetManifest = manifestPath
			opts.toolsetAnchors = anchors
		} else {
			opts.hostToolsetManifest = manifestPath
			opts.hostToolsetAnchors = anchors
		}
	}

	for _, filename := range opts.inputs {
		input, err := kconfig.ReadProbeResult(filename)
		if err != nil {
			t.Fatalf("prepare test probe input: %v", err)
		}
		if identity := identities[input.Scope]; identity != "" {
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
	if identities["host"] != "" && opts.scope == "target" {
		data, err := os.ReadFile(opts.request)
		if err != nil {
			t.Fatal(err)
		}
		var request kconfig.ProbeRequest
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		selectedHostTool := slices.ContainsFunc(request.ToolRoles(), func(binding string) bool {
			scope, _, scoped, valid := toolaction.SplitBinding(binding)
			return valid && scoped && scope == "host"
		})
		if selectedHostTool {
			request.HostToolsetIdentity = identities["host"]
			canonical, err := request.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(opts.request, canonical, 0o600); err != nil {
				t.Fatal(err)
			}
			opts.requestID, err = request.ID()
			if err != nil {
				t.Fatal(err)
			}
			inputNodeIDs := make([]string, request.InputCount)
			for index := range inputNodeIDs {
				name := fmt.Sprintf("%08d", index)
				input, err := kconfig.ReadProbeResult(opts.inputs[name])
				if err != nil {
					t.Fatalf("prepare scoped test probe input %s: %v", name, err)
				}
				inputNodeIDs[index] = input.NodeID
			}
			opts.nodeID = (kconfig.ProbePlanNode{Scope: opts.scope, RequestID: opts.requestID, Inputs: inputNodeIDs}).ContentID()
		}
	}
	return opts
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

func testToolsetRootBindings(t *testing.T, execroot string, closure, files []string) (map[string]toolaction.KbuildToolsetArtifactRoot, map[string]string, map[string]string) {
	t.Helper()
	physicalByCanonical := make(map[string]string, len(files))
	rootByCanonical := make(map[string]string, len(files))
	relativeByCanonical := make(map[string]string, len(files))
	membersByRoot := map[string][]string{}
	for _, filename := range files {
		canonical, err := canonicalActionArtifactPath(execroot, filename)
		if err != nil {
			t.Fatalf("canonicalize test toolset root anchor %q: %v", filename, err)
		}
		if prior := physicalByCanonical[canonical]; prior != "" {
			t.Fatalf("test toolset root anchor %q repeats canonical artifact %q already bound by %q", filename, canonical, prior)
		}
		physical := filepath.Clean(filename)
		if !filepath.IsAbs(physical) {
			physical = filepath.Join(execroot, physical)
		}
		root := physical
		for range strings.Split(canonical, "/") {
			root = filepath.Dir(root)
		}
		relative := canonical
		if filepath.Clean(filepath.Join(root, filepath.FromSlash(relative))) != physical {
			root = filepath.Dir(physical)
			relative = filepath.Base(physical)
		}
		physicalByCanonical[canonical] = physical
		rootByCanonical[canonical] = root
		relativeByCanonical[canonical] = filepath.ToSlash(relative)
		membersByRoot[root] = append(membersByRoot[root], canonical)
	}
	groupsByFirstMember := map[string]string{}
	for physicalRoot, members := range membersByRoot {
		sort.Strings(members)
		groupsByFirstMember[members[0]] = physicalRoot
	}
	firstMembers := make([]string, 0, len(groupsByFirstMember))
	for first := range groupsByFirstMember {
		firstMembers = append(firstMembers, first)
	}
	sort.Strings(firstMembers)
	artifactRoots := make(map[string]toolaction.KbuildToolsetArtifactRoot, len(closure))
	roots := make(map[string]string, len(firstMembers))
	anchors := make(map[string]string, len(firstMembers))
	for index, first := range firstMembers {
		physicalRoot := groupsByFirstMember[first]
		root := fmt.Sprintf("root-%08d", index)
		roots[root] = first
		anchors[root] = physicalByCanonical[first]
		for _, canonical := range membersByRoot[physicalRoot] {
			artifactRoots[canonical] = toolaction.KbuildToolsetArtifactRoot{
				Root: root,
				Path: relativeByCanonical[canonical],
			}
		}
	}
	for _, canonical := range closure {
		if rootByCanonical[canonical] == "" {
			t.Fatalf("test toolset closure artifact %q has no physical binding", canonical)
		}
	}
	return artifactRoots, roots, anchors
}

func sortedActionContractRoles(values map[string]actionContract) []string {
	roles := make([]string, 0, len(values))
	for role := range values {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}
