package kconfig

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const compactActionPlanMetadataSchema = "compact-v8-adaptive-content-graph"

type compactActionPlanEntry struct {
	path string
	data []byte
}

// WriteCompactActionPlan writes a versioned action-plan tree for one measured,
// adaptive compact configuration. The directory layout intentionally carries
// every value needed by a map_directory callback: callbacks can inspect a tree
// artifact's child paths, but cannot read child contents during expansion.
// Compile marker contents retain the validated recipe for ordinary actions and
// future expansion stages while the callback selects work from marker paths.
func (m *CompactMetadata) WriteCompactActionPlan(outputDir string) error {
	entries, err := m.compactActionPlanEntries()
	if err != nil {
		return err
	}
	if strings.TrimSpace(outputDir) == "" {
		return fmt.Errorf("compact action plan output directory must not be empty")
	}
	outputDir = filepath.Clean(outputDir)
	precreated := false
	if info, err := os.Lstat(outputDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("compact action plan output %q exists and is not a directory", outputDir)
		}
		children, err := os.ReadDir(outputDir)
		if err != nil {
			return fmt.Errorf("inspect compact action plan output directory %q: %w", outputDir, err)
		}
		if len(children) != 0 {
			return fmt.Errorf("compact action plan output directory %q is not empty", outputDir)
		}
		precreated = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect compact action plan output directory %q: %w", outputDir, err)
	}

	parent := filepath.Dir(outputDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create compact action plan parent %q: %w", parent, err)
	}
	temporary, err := os.MkdirTemp(parent, "."+filepath.Base(outputDir)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary compact action plan: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(temporary)
		}
	}()

	for _, entry := range entries {
		filename := filepath.Join(temporary, filepath.FromSlash(entry.path))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			return fmt.Errorf("create compact action plan directory for %q: %w", entry.path, err)
		}
		if err := os.WriteFile(filename, entry.data, 0o644); err != nil {
			return fmt.Errorf("write compact action plan entry %q: %w", entry.path, err)
		}
	}
	// Bazel precreates an empty directory for a declared TreeArtifact. Replace
	// that exact empty directory only after the complete plan has been staged.
	if precreated {
		if err := os.Remove(outputDir); err != nil {
			return fmt.Errorf("replace empty compact action plan output directory %q: %w", outputDir, err)
		}
	}
	if err := os.Rename(temporary, outputDir); err != nil {
		return fmt.Errorf("publish compact action plan %q: %w", outputDir, err)
	}
	published = true
	return nil
}

func (m *CompactMetadata) compactActionPlanEntries() ([]compactActionPlanEntry, error) {
	if m == nil {
		return nil, fmt.Errorf("compact action plan requires metadata")
	}
	if m.Schema != compactActionPlanMetadataSchema || m.Target == nil {
		return nil, fmt.Errorf("compact action plan requires measured adaptive compact metadata")
	}
	profile, err := LinuxTargetProfileByName(m.Target.Profile)
	if err != nil {
		return nil, fmt.Errorf("compact action plan target: %w", err)
	}
	if err := profile.ValidateTargetIdentity(m.Target.LinuxArch, m.Target.TargetTriple); err != nil {
		return nil, fmt.Errorf("compact action plan target: %w", err)
	}
	if m.Target.Profile != profile.Name ||
		m.Target.LinuxArch != profile.Arch ||
		m.Target.Srcarch != profile.Srcarch ||
		m.Target.UTSMachine != profile.UTSMachine ||
		m.Target.TargetTriple != profile.TargetTriple {
		return nil, fmt.Errorf("compact action plan has incomplete or inconsistent target identity %#v", m.Target)
	}
	if err := validateCompactActionPlanProbeIdentity(m.Target.ProbeIdentity); err != nil {
		return nil, err
	}
	if len(m.Configs) != 1 {
		return nil, fmt.Errorf("compact action plan requires exactly one config, got %d", len(m.Configs))
	}

	variants := make(map[string]CompactObjectVariant, len(m.ObjectVariants))
	for _, variant := range m.ObjectVariants {
		if _, exists := variants[variant.Target]; exists {
			return nil, fmt.Errorf("compact action plan has duplicate object target %q", variant.Target)
		}
		variants[variant.Target] = variant
	}
	config := m.Configs[0]
	selectedTargets := append([]string(nil), config.ObjectTargets...)
	selectedTargets = append(selectedTargets, config.ModuleObjectTargets...)
	sort.Strings(selectedTargets)

	selected := make([]CompactObjectVariant, 0, len(selectedTargets))
	seenTargets := make(map[string]bool, len(selectedTargets))
	for _, target := range selectedTargets {
		if seenTargets[target] {
			continue
		}
		seenTargets[target] = true
		variant, ok := variants[target]
		if !ok {
			return nil, fmt.Errorf("compact config %q selects missing object target %q", config.Name, target)
		}
		if err := validateCompactActionPlanVariant(variant); err != nil {
			return nil, err
		}
		selected = append(selected, variant)
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].Object != selected[j].Object {
			return selected[i].Object < selected[j].Object
		}
		return selected[i].ContentID < selected[j].ContentID
	})

	usedSourceIndices := map[int]bool{}
	primarySourceIndices := make(map[string]int, len(selected))
	for _, variant := range selected {
		if variant.SourceInputGroup <= 0 || variant.SourceInputGroup > len(m.SourceInputGroups) {
			return nil, fmt.Errorf(
				"object %q references source input group %d, out of range 1..%d",
				variant.Object,
				variant.SourceInputGroup,
				len(m.SourceInputGroups),
			)
		}
		indices, err := decodeCompactSourceInputGroup(
			m.SourceInputGroups[variant.SourceInputGroup-1],
			len(m.SourceFiles),
		)
		if err != nil {
			return nil, fmt.Errorf("object %q source inputs: %w", variant.Object, err)
		}
		primary := 0
		for _, index := range indices {
			input := m.SourceFiles[index-1]
			if err := validateCompactActionPlanRelativePath("source", input.Path); err != nil {
				return nil, err
			}
			if input.Path == variant.Source {
				primary = index
			}
			usedSourceIndices[index] = true
		}
		if primary == 0 {
			return nil, fmt.Errorf("object %q source input group omits primary source %q", variant.Object, variant.Source)
		}
		primarySourceIndices[variant.Target] = primary
	}

	// The compact graph generator already canonicalizes and hashes these fields.
	// Revalidate it before publishing recipes so hand-constructed or corrupted
	// metadata cannot become executable action descriptions.
	if err := m.validateContentIDs(); err != nil {
		return nil, fmt.Errorf("validate compact action plan metadata: %w", err)
	}

	entries := []compactActionPlanEntry{{
		path: path.Join("v1", "probe", m.Target.ProbeIdentity),
	}}
	indices := make([]int, 0, len(usedSourceIndices))
	for index := range usedSourceIndices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		if index > 99999999 {
			return nil, fmt.Errorf("compact action plan source index %d exceeds eight digits", index)
		}
		entries = append(entries, compactActionPlanEntry{
			path: path.Join("v1", "source", fmt.Sprintf("%08d", index), m.SourceFiles[index-1].Path),
		})
	}
	for _, variant := range selected {
		data, err := json.MarshalIndent(variant, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode compact action plan recipe for %q: %w", variant.Object, err)
		}
		data = append(data, '\n')
		entries = append(entries, compactActionPlanEntry{
			path: path.Join(
				"v1",
				"compile",
				variant.ContentID,
				fmt.Sprintf("%08d", primarySourceIndices[variant.Target]),
				variant.Object+".json",
			),
			data: data,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

func validateCompactActionPlanVariant(variant CompactObjectVariant) error {
	if err := validateCompactActionPlanDigest("object content ID", variant.ContentID); err != nil {
		return fmt.Errorf("object %q: %w", variant.Object, err)
	}
	if err := validateCompactActionPlanRelativePath("object", variant.Object); err != nil {
		return err
	}
	if len(variant.Members) != 0 {
		return fmt.Errorf("object %q has an unsupported composite recipe", variant.Object)
	}
	if len(variant.Deps) != 0 {
		return fmt.Errorf("object %q has unsupported generated-object dependencies", variant.Object)
	}
	if len(variant.RemoveFlags) != 0 {
		return fmt.Errorf("object %q has unsupported Kbuild remove flags", variant.Object)
	}
	if compactGroupedSpecialObjects[variant.Object] ||
		strings.HasSuffix(variant.Object, ".asn1.o") ||
		strings.HasSuffix(variant.Object, ".pi.o") ||
		strings.HasSuffix(variant.Object, ".stub.o") {
		return fmt.Errorf("object %q has an unsupported generated recipe", variant.Object)
	}
	if !strings.HasSuffix(variant.Object, ".o") {
		return fmt.Errorf("object %q is not a leaf object", variant.Object)
	}
	if variant.Mode != "y" || variant.ModuleRoot {
		return fmt.Errorf("object %q has unsupported non-built-in mode %q", variant.Object, variant.Mode)
	}
	if err := validateCompactActionPlanRelativePath("source", variant.Source); err != nil {
		return err
	}
	if compactSourceLanguage(variant.Source) != "c" {
		return fmt.Errorf("object %q has unsupported non-C source %q", variant.Object, variant.Source)
	}
	if variant.Symversions || variant.ObjtoolForce || len(variant.ObjtoolArgs) != 0 {
		return fmt.Errorf("object %q has unsupported generated or post-compile processing", variant.Object)
	}
	if reason := variant.sourceBuildError(); reason != "" {
		return fmt.Errorf("object %q cannot be compiled from source: %s", variant.Object, reason)
	}
	return nil
}

func validateCompactActionPlanProbeIdentity(identity string) error {
	digest, ok := strings.CutPrefix(identity, "sha256-")
	if !ok {
		return fmt.Errorf("compact action plan requires a measured sha256 probe identity, got %q", identity)
	}
	if err := validateCompactActionPlanDigest("probe identity", digest); err != nil {
		return err
	}
	return nil
}

func validateCompactActionPlanDigest(kind, value string) error {
	if len(value) != 64 || strings.ToLower(value) != value {
		return fmt.Errorf("%s %q is not a canonical SHA-256 digest", kind, value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s %q is not a canonical SHA-256 digest: %w", kind, value, err)
	}
	return nil
}

func validateCompactActionPlanRelativePath(kind, value string) error {
	if value == "" || strings.Contains(value, `\`) || strings.ContainsRune(value, 0) ||
		strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return fmt.Errorf("compact action plan %s path %q is not a canonical relative path", kind, value)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("compact action plan %s path %q is not a canonical relative path", kind, value)
		}
	}
	return nil
}
