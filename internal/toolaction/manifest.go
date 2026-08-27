package toolaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const KbuildToolsetManifestSchema = "linux-kbuild-toolset-v5"

const (
	// Bazel's File.is_directory reports only declared TreeArtifacts and Filesets.
	// Legacy source-directory artifacts therefore cannot be distinguished from
	// source files during analysis. SourceArtifact records that exact Bazel type;
	// the consumer may inspect only the bound artifact itself to distinguish its
	// on-disk shape.
	KbuildToolsetArtifactSource             = "source-artifact"
	KbuildToolsetArtifactGeneratedFile      = "generated-file"
	KbuildToolsetArtifactGeneratedDirectory = "generated-directory"
)

// KbuildToolsetManifest is the canonical, identity-bound description of one
// configured Kbuild toolset. Closure entries use the stable logical artifact
// namespace shared by analysis and mapped execution actions.
type KbuildToolsetManifest struct {
	Schema        string                       `json:"schema"`
	Scope         string                       `json:"scope"`
	Actions       map[string][]string          `json:"actions"`
	Tools         map[string]string            `json:"tools"`
	Closure       []string                     `json:"closure"`
	ArtifactKinds map[string]string            `json:"artifact_kinds"`
	Environments  map[string]map[string]string `json:"environments"`
	MakeVariables map[string]string            `json:"make_variables"`
	Requirements  map[string]map[string]string `json:"requirements"`
}

// ReadKbuildToolsetManifest decodes exactly one canonical v5 manifest.
func ReadKbuildToolsetManifest(filename string) (KbuildToolsetManifest, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return KbuildToolsetManifest{}, fmt.Errorf("read toolset manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest KbuildToolsetManifest
	if err := decoder.Decode(&manifest); err != nil {
		return KbuildToolsetManifest{}, fmt.Errorf("decode toolset manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return KbuildToolsetManifest{}, fmt.Errorf("decode toolset manifest trailer: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return KbuildToolsetManifest{}, err
	}
	return manifest, nil
}

// Validate checks the schema and every identity-bearing structural invariant.
func (m KbuildToolsetManifest) Validate() error {
	if m.Schema != KbuildToolsetManifestSchema {
		return fmt.Errorf("toolset manifest schema is %q, want %q", m.Schema, KbuildToolsetManifestSchema)
	}
	if m.Scope != "target" && m.Scope != "host" {
		return fmt.Errorf("toolset manifest scope is %q, want target or host", m.Scope)
	}
	if len(m.Actions) == 0 {
		return errors.New("toolset manifest actions must not be empty")
	}
	if len(m.Actions) != len(m.Tools) || len(m.Actions) != len(m.Environments) || len(m.Actions) != len(m.Requirements) {
		return errors.New("toolset manifest actions, tools, environments, and requirements must have identical roles")
	}
	for role := range m.Actions {
		if !ValidRole(role) {
			return fmt.Errorf("toolset manifest has invalid action role %q", role)
		}
		if m.Tools[role] == "" {
			return fmt.Errorf("toolset manifest action role %q has no tool", role)
		}
		if m.Environments[role] == nil || m.Requirements[role] == nil {
			return fmt.Errorf("toolset manifest action role %q has no environment/requirements contract", role)
		}
	}
	for role := range m.Tools {
		if _, ok := m.Actions[role]; !ok {
			return fmt.Errorf("toolset manifest tool role %q has no action", role)
		}
	}
	for role := range m.Environments {
		if _, ok := m.Actions[role]; !ok {
			return fmt.Errorf("toolset manifest environment role %q has no action", role)
		}
	}
	for role := range m.Requirements {
		if _, ok := m.Actions[role]; !ok {
			return fmt.Errorf("toolset manifest requirements role %q has no action", role)
		}
	}
	for name, role := range m.MakeVariables {
		if name == "" || role == "" || strings.ContainsAny(name, "=\x00 \t\r\n") {
			return fmt.Errorf("toolset manifest has invalid Make variable binding %q=%q", name, role)
		}
		if _, ok := m.Actions[role]; !ok {
			return fmt.Errorf("toolset manifest Make variable %q references unknown action role %q", name, role)
		}
	}
	if !sort.StringsAreSorted(m.Closure) {
		return errors.New("toolset manifest closure paths must be sorted")
	}
	for index, path := range m.Closure {
		if err := ValidateCanonicalArtifactPath(path); err != nil {
			return fmt.Errorf("toolset manifest closure path %q: %w", path, err)
		}
		if index > 0 && path == m.Closure[index-1] {
			return fmt.Errorf("toolset manifest closure path %q is repeated", path)
		}
		kind, exists := m.ArtifactKinds[path]
		if !exists {
			return fmt.Errorf("toolset manifest closure path %q has no artifact kind", path)
		}
		if !ValidKbuildToolsetArtifactKind(kind) {
			return fmt.Errorf("toolset manifest closure path %q has invalid artifact kind %q", path, kind)
		}
	}
	if len(m.ArtifactKinds) != len(m.Closure) {
		return errors.New("toolset manifest artifact kinds must exactly cover the closure")
	}
	for path := range m.ArtifactKinds {
		if err := ValidateCanonicalArtifactPath(path); err != nil {
			return fmt.Errorf("toolset manifest artifact kind path %q: %w", path, err)
		}
		index := sort.SearchStrings(m.Closure, path)
		if index == len(m.Closure) || m.Closure[index] != path {
			return fmt.Errorf("toolset manifest artifact kind path %q is absent from its closure", path)
		}
	}
	for role, path := range m.Tools {
		if err := ValidateCanonicalArtifactPath(path); err != nil {
			return fmt.Errorf("toolset manifest tool %q path %q: %w", role, path, err)
		}
		index := sort.SearchStrings(m.Closure, path)
		if index == len(m.Closure) || m.Closure[index] != path {
			return fmt.Errorf("toolset manifest tool %q path %q is absent from its closure", role, path)
		}
	}
	return nil
}

func ValidKbuildToolsetArtifactKind(kind string) bool {
	switch kind {
	case KbuildToolsetArtifactSource,
		KbuildToolsetArtifactGeneratedFile,
		KbuildToolsetArtifactGeneratedDirectory:
		return true
	default:
		return false
	}
}

// Identity returns the digest encoded by a toolset identity marker.
func (m KbuildToolsetManifest) Identity() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode canonical toolset manifest: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return "sha256-" + hex.EncodeToString(digest[:]), nil
}

// ValidateCanonicalArtifactPath validates the stable logical artifact
// namespace used in a toolset manifest.
func ValidateCanonicalArtifactPath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "\\") {
		return errors.New("path is not canonical relative syntax")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("path contains an empty, dot, or parent component")
		}
	}
	return nil
}

// CanonicalArtifactPath maps Bazel's execution/output and sibling-repository
// spellings into the stable namespace stored in KbuildToolsetManifest.
func CanonicalArtifactPath(value string) (string, error) {
	value = strings.ReplaceAll(value, "\\", "/")
	if strings.HasPrefix(value, "../") {
		value = "external/" + strings.TrimPrefix(value, "../")
	}
	if strings.HasPrefix(value, "bazel-out/") {
		parts := strings.Split(value, "/")
		if len(parts) < 4 || (parts[2] != "bin" && parts[2] != "genfiles") {
			return "", fmt.Errorf("unrecognized Bazel output path %q", value)
		}
		value = strings.Join(parts[3:], "/")
	}
	if err := ValidateCanonicalArtifactPath(value); err != nil {
		return "", err
	}
	return value, nil
}
