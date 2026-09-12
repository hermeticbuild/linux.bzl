package kconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
)

const (
	LinuxKernelFamilyObservedHeadersSchema   = "linux-kernel-family-observed-headers-v1"
	LinuxKernelObservedHeaderSourceNamespace = "observed-headers"
	MaxActionPlanFamilyObservedHeaderBytes   = 64 << 20
	MaxActionPlanFamilyObservedHeadersBytes  = 128 << 20
)

// ActionPlanFamilyObservedHeader identifies bytes read from an exact executed
// ordinary output. It is not a claim that those bytes are valid C, nor evidence
// that an arbitrary consumer can see the output: the include scanner must still
// prove the original producer/slot ownership and source/object shadow ordering.
type ActionPlanFamilyObservedHeader struct {
	NodeID         string           `json:"node_id"`
	Slot           int              `json:"slot"`
	Output         ActionPlanOutput `json:"output"`
	ContentID      string           `json:"content_id"`
	ExecutableMode uint32           `json:"executable_mode"`
}

type actionPlanFamilyObservedHeadersWire struct {
	Schema  string                           `json:"schema"`
	CutID   string                           `json:"cut_id"`
	Headers []ActionPlanFamilyObservedHeader `json:"headers"`
}

// ActionPlanFamilyObservedHeaders is a bounded immutable observation of actual
// execution-cut outputs. The cut's ObserveHeaders reads selected root outputs;
// executed artifacts' ObserveHeaders derives all ordinary output observations
// from bytes already read from that same cut. A caller-supplied digest or a plan
// marker cannot stand in for reading the executed file.
//
// This is not permission to emit an executable final family.
// The caller must authenticate the original replay invocation before exposing
// contents to analysis, and the execution cut must Verify the final family
// before any source substitution or pinned execution is published.
type ActionPlanFamilyObservedHeaders struct {
	cutID    string
	headers  []ActionPlanFamilyObservedHeader
	contents map[string][]byte
	modes    map[string]uint32
	// Set only when authenticated executed artifacts supplied every ordinary
	// output, rather than the original unresolved-header roots alone.
	allExecutedOutputs bool
}

// observedHeaderContentID deliberately excludes the variant, producer, tree,
// logical pathname and physical store. Equal bytes and executable permissions
// can have a common artifact identity without conflating their ownership proof.
func observedHeaderContentID(contents []byte, executableMode uint32) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, "linux-kernel-observed-header-content-v1\x00")
	_, _ = io.WriteString(hash, strconv.FormatUint(uint64(executableMode), 8))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(contents)
	return hex.EncodeToString(hash.Sum(nil))
}

// ObserveHeaders opens actual output stores, never recipe/plan marker trees.
// All selected nodes' complete output vectors must exist as regular files,
// including sidecar envelopes. Only the requested ordinary root slots are read
// as header bytes; observed-state slots are never parsed or rewritten here.
// The stores must be completed immutable Bazel action outputs for this cut and
// the retained source/tool/config/probe invocation. This method does not execute
// generators or infer successful execution from the presence of a partial file.
func (cut *ActionPlanFamilyExecutionCut) ObserveHeaders(stores map[string]string) (*ActionPlanFamilyObservedHeaders, error) {
	return cut.observeHeaders(stores, MaxActionPlanFamilyObservedHeaderBytes, MaxActionPlanFamilyObservedHeadersBytes)
}

func (cut *ActionPlanFamilyExecutionCut) observeHeaders(stores map[string]string, perFileLimit, totalLimit int) (observed *ActionPlanFamilyObservedHeaders, returnedErr error) {
	if _, err := cut.CanonicalJSON(); err != nil {
		return nil, fmt.Errorf("observe execution cut: %w", err)
	}
	if perFileLimit <= 0 || totalLimit <= 0 {
		return nil, fmt.Errorf("observed headers require positive byte limits")
	}
	stores = maps.Clone(stores)
	for tree, directory := range stores {
		if !LinuxKernelPlanTrees[tree] || directory == "" {
			return nil, fmt.Errorf("observed headers have invalid output store %q", tree)
		}
	}
	roots := map[ActionPlanFamilyExecutionCutRoot]bool{}
	for _, root := range cut.Roots() {
		roots[root] = true
	}
	result := &ActionPlanFamilyObservedHeaders{
		cutID: cut.ID(), headers: []ActionPlanFamilyObservedHeader{},
		contents: map[string][]byte{}, modes: map[string]uint32{},
	}
	opened := map[string]*os.Root{}
	defer func() {
		for tree, root := range opened {
			if err := root.Close(); err != nil && returnedErr == nil {
				observed = nil
				returnedErr = fmt.Errorf("close observed output store %s: %w", tree, err)
			}
		}
	}()
	total := 0
	for _, output := range cut.Outputs() {
		tree := output.Output.Tree
		root := opened[tree]
		if root == nil {
			directory, exists := stores[tree]
			if !exists {
				return nil, fmt.Errorf("observed headers require output store %s", tree)
			}
			var err error
			root, err = os.OpenRoot(directory)
			if err != nil {
				return nil, fmt.Errorf("open observed output store %s: %w", tree, err)
			}
			opened[tree] = root
		}
		filename := path.Join("nodes", output.NodeID, planOrdinal(output.Slot))
		info, err := observedHeaderRegularFile(root, filename)
		if err != nil {
			return nil, fmt.Errorf("observe %s/%s: %w", tree, filename, err)
		}
		if !roots[ActionPlanFamilyExecutionCutRoot{NodeID: output.NodeID, Slot: output.Slot}] {
			continue
		}
		if output.Output.ObservedPath != "" {
			return nil, fmt.Errorf("observed header root %s/%s is a sidecar envelope", tree, filename)
		}
		if info.Size() < 0 || info.Size() > int64(perFileLimit) || info.Size() > int64(totalLimit-total) {
			return nil, fmt.Errorf("observed header %s/%s exceeds byte budget", tree, filename)
		}
		content, err := readObservedHeader(root, filename, info, min(perFileLimit, totalLimit-total))
		if err != nil {
			return nil, fmt.Errorf("read observed header %s/%s: %w", tree, filename, err)
		}
		total += len(content)
		mode := uint32(info.Mode().Perm() & 0o111)
		id := observedHeaderContentID(content, mode)
		if previous, exists := result.contents[id]; exists {
			if !bytes.Equal(previous, content) || result.modes[id] != mode {
				return nil, fmt.Errorf("observed header content identity collision")
			}
		} else {
			result.contents[id] = content
			result.modes[id] = mode
		}
		result.headers = append(result.headers, ActionPlanFamilyObservedHeader{
			NodeID: output.NodeID, Slot: output.Slot, Output: output.Output,
			ContentID: id, ExecutableMode: mode,
		})
	}
	if len(result.headers) != len(roots) {
		return nil, fmt.Errorf("observed headers did not cover every selected root")
	}
	return result, nil
}

// Reject symlink components even when they happen to point inside the store.
// os.Root also confines the subsequent open if a component changes. Completed
// action stores are immutable; stat checks additionally reject accidental races
// or regular-looking replacements rather than accepting a partial observation.
func observedHeaderRegularFile(root *os.Root, filename string) (os.FileInfo, error) {
	for _, directory := range []string{"nodes", path.Dir(filename)} {
		info, err := root.Lstat(directory)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("output ancestor %s is not a regular directory", directory)
		}
	}
	info, err := root.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("output is not a regular file")
	}
	return info, nil
}

func readObservedHeader(root *os.Root, filename string, before os.FileInfo, limit int) ([]byte, error) {
	file, err := root.Open(filename)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("output changed before opening: %v", statErr)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if statErr != nil {
		return nil, statErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	final, err := observedHeaderRegularFile(root, filename)
	if err != nil {
		return nil, err
	}
	if len(content) > limit || int64(len(content)) != before.Size() ||
		!os.SameFile(before, final) || after.Size() != before.Size() || final.Size() != before.Size() ||
		after.Mode() != before.Mode() || final.Mode() != before.Mode() ||
		!after.ModTime().Equal(before.ModTime()) || !final.ModTime().Equal(before.ModTime()) {
		return nil, fmt.Errorf("output changed or exceeded byte budget while reading")
	}
	return content, nil
}

func (observed *ActionPlanFamilyObservedHeaders) Headers() []ActionPlanFamilyObservedHeader {
	if observed == nil {
		return nil
	}
	return slices.Clone(observed.headers)
}

func (observed *ActionPlanFamilyObservedHeaders) Content(id string) ([]byte, bool) {
	if observed == nil {
		return nil, false
	}
	content, exists := observed.contents[id]
	return slices.Clone(content), exists
}

func (observed *ActionPlanFamilyObservedHeaders) CanonicalJSON() ([]byte, error) {
	if observed == nil || validatePlanDigest("observed header cut", observed.cutID) != nil {
		return nil, fmt.Errorf("observed headers require a sealed execution cut")
	}
	content, err := json.Marshal(actionPlanFamilyObservedHeadersWire{
		Schema: LinuxKernelFamilyObservedHeadersSchema, CutID: observed.cutID, Headers: observed.headers,
	})
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

// Sources returns content-addressed immutable source descriptors for staging
// ordinary headers. Their physical CAS paths are not the Kbuild-visible paths;
// a consumer must preserve each separately proved logical destination.
func (observed *ActionPlanFamilyObservedHeaders) Sources() []ActionPlanSource {
	if observed == nil {
		return nil
	}
	sources := make([]ActionPlanSource, 0, len(observed.contents))
	for _, id := range slices.Sorted(maps.Keys(observed.contents)) {
		pathname := path.Join("content", id)
		sources = append(sources, ActionPlanSource{
			ID:        semanticFamilySourceID(LinuxKernelObservedHeaderSourceNamespace, pathname),
			Namespace: LinuxKernelObservedHeaderSourceNamespace, Path: pathname,
		})
	}
	return sources
}

// WriteContentTree writes only into an existing empty declared output
// directory. Every file is created exclusively and receives canonical read /
// write permissions plus its original executable bits. The manifest is written
// last; errors never return a usable observation and never overwrite an input.
func (observed *ActionPlanFamilyObservedHeaders) WriteContentTree(directory string) (returnedErr error) {
	manifest, err := observed.CanonicalJSON()
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Close(); err != nil && returnedErr == nil {
			returnedErr = err
		}
	}()
	entries, err := root.Open(".")
	if err != nil {
		return err
	}
	names, readErr := entries.Readdirnames(1)
	closeErr := entries.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(names) != 0 {
		return fmt.Errorf("observed header output directory is not empty")
	}
	if err := root.Mkdir("content", 0o755); err != nil {
		return err
	}
	write := func(filename string, content []byte, mode os.FileMode) error {
		file, err := root.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(content)
		chmodErr := file.Chmod(mode)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if chmodErr != nil {
			return chmodErr
		}
		return closeErr
	}
	for _, id := range slices.Sorted(maps.Keys(observed.contents)) {
		if err := write(path.Join("content", id), observed.contents[id], 0o644|os.FileMode(observed.modes[id])); err != nil {
			return err
		}
	}
	return write("manifest.json", manifest, 0o644)
}
