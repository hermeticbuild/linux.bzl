package kconfig

// Content publication follows complete final-family execution-cut validation.
// This writer independently checks ownership, bytes and modes before any write.
import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
)

func writeExecutedArtifactContentTree(observed *ActionPlanFamilyExecutedArtifacts, directory string) (returnedErr error) {
	if observed == nil || observed.cut == nil || observed.cut.ID() != observed.cutID {
		return fmt.Errorf("artifact content tree requires its authenticated execution")
	}
	if _, err := observed.cut.CanonicalJSON(); err != nil {
		return err
	}
	outputs := observed.cut.Outputs()
	if len(observed.outputs) != len(outputs) {
		return fmt.Errorf("artifact content tree lost an executed output")
	}
	for _, output := range outputs {
		key := ActionPlanFamilyExecutionCutRoot{NodeID: output.NodeID, Slot: output.Slot}
		artifact, found := observed.outputs[key]
		if !found || artifact.output != output {
			return fmt.Errorf("artifact content tree changed executed output ownership")
		}
		content, found := observed.contents[artifact.contentID]
		mode, haveMode := observed.modes[artifact.contentID]
		if !found || !haveMode || mode & ^uint32(0o111) != 0 || executedArtifactContentID(content, mode) != artifact.contentID {
			return fmt.Errorf("artifact content tree lost authenticated bytes or mode")
		}
	}
	if len(observed.contents) != len(observed.modes) {
		return fmt.Errorf("artifact content tree has unmatched modes")
	}
	ids := slices.Sorted(maps.Keys(observed.contents))
	for _, id := range ids {
		if err := validatePlanDigest("artifact content identity", id); err != nil {
			return err
		}
		mode, found := observed.modes[id]
		if !found || mode & ^uint32(0o111) != 0 || executedArtifactContentID(observed.contents[id], mode) != id {
			return fmt.Errorf("artifact content tree has an invalid source commitment")
		}
	}
	manifest, err := json.Marshal(struct {
		Schema   string   `json:"schema"`
		CutID    string   `json:"cut_id"`
		Contents []string `json:"contents"`
	}{"linux-kernel-executed-artifacts-v1", observed.cutID, ids})
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
		return fmt.Errorf("artifact output directory is not empty")
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
	for _, id := range ids {
		if err := write(path.Join("content", id), observed.contents[id], 0o644|os.FileMode(observed.modes[id])); err != nil {
			return err
		}
	}
	return write("manifest.json", append(manifest, '\n'), 0o644)
}
