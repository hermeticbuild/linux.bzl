package toolaction

// This file defines the immutable absolute-state handoff for source-selected
// working-tree paths. Each envelope records the complete post-action state and
// the node which last changed it, so consumers need only projected maximal
// predecessor states instead of every transitive delta.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const ObservedOutputStateSchema = "linux-bzl-observed-output-state-v2"

type ObservedOutputDisposition string

const (
	ObservedOutputAbsent  ObservedOutputDisposition = "absent"
	ObservedOutputPresent ObservedOutputDisposition = "present"
	ObservedOutputDeleted ObservedOutputDisposition = "deleted"
)

// ObservedOutputState is the absolute state of one observed regular-file path.
// Absent carries no writer. Present carries its writer, complete bytes, and
// executable permission bits. Deleted carries the writer which removed the
// path. Read/write permission changes are intentionally ignored.
type ObservedOutputState struct {
	Disposition    ObservedOutputDisposition
	Writer         string
	Content        []byte
	ExecutableMode uint32
}

type observedOutputStateWire struct {
	Schema         string                    `json:"schema"`
	State          ObservedOutputDisposition `json:"state"`
	Writer         *string                   `json:"writer,omitempty"`
	ContentBase64  *string                   `json:"content_base64,omitempty"`
	ExecutableMode *uint32                   `json:"executable_mode,omitempty"`
}

func validObservedOutputWriter(writer string) bool {
	if len(writer) != 64 {
		return false
	}
	for _, character := range writer {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateObservedOutputState(state ObservedOutputState) error {
	switch state.Disposition {
	case ObservedOutputAbsent:
		if state.Writer != "" || len(state.Content) != 0 || state.ExecutableMode != 0 {
			return fmt.Errorf("absent observed output state cannot carry writer, content, or executable mode")
		}
	case ObservedOutputPresent:
		if !validObservedOutputWriter(state.Writer) {
			return fmt.Errorf("present observed output state has invalid writer %q", state.Writer)
		}
		if state.ExecutableMode&^uint32(0o111) != 0 {
			return fmt.Errorf("observed output executable mode %#o contains non-executable bits", state.ExecutableMode)
		}
	case ObservedOutputDeleted:
		if !validObservedOutputWriter(state.Writer) {
			return fmt.Errorf("deleted observed output state has invalid writer %q", state.Writer)
		}
		if len(state.Content) != 0 || state.ExecutableMode != 0 {
			return fmt.Errorf("deleted observed output state cannot carry content or executable mode")
		}
	default:
		return fmt.Errorf("unsupported observed output state %q", state.Disposition)
	}
	return nil
}

// EncodeObservedOutputState returns the unique canonical JSON encoding of an
// absolute observed-output state.
func EncodeObservedOutputState(state ObservedOutputState) ([]byte, error) {
	if err := validateObservedOutputState(state); err != nil {
		return nil, err
	}
	wire := observedOutputStateWire{
		Schema: ObservedOutputStateSchema,
		State:  state.Disposition,
	}
	switch state.Disposition {
	case ObservedOutputAbsent:
	case ObservedOutputPresent:
		writer := state.Writer
		content := base64.StdEncoding.EncodeToString(state.Content)
		mode := state.ExecutableMode
		wire.Writer = &writer
		wire.ContentBase64 = &content
		wire.ExecutableMode = &mode
	case ObservedOutputDeleted:
		writer := state.Writer
		wire.Writer = &writer
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode observed output state: %w", err)
	}
	return append(data, '\n'), nil
}

// DecodeObservedOutputState accepts only the canonical representation emitted
// by EncodeObservedOutputState.
func DecodeObservedOutputState(data []byte) (ObservedOutputState, error) {
	var wire observedOutputStateWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return ObservedOutputState{}, fmt.Errorf("decode observed output state: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return ObservedOutputState{}, fmt.Errorf("decode observed output state: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return ObservedOutputState{}, fmt.Errorf("decode observed output state: %w", err)
	}
	if wire.Schema != ObservedOutputStateSchema {
		return ObservedOutputState{}, fmt.Errorf("observed output state schema %q, want %q", wire.Schema, ObservedOutputStateSchema)
	}
	state := ObservedOutputState{Disposition: wire.State}
	switch wire.State {
	case ObservedOutputAbsent:
		if wire.Writer != nil || wire.ContentBase64 != nil || wire.ExecutableMode != nil {
			return ObservedOutputState{}, fmt.Errorf("absent observed output state cannot carry writer, content, or executable mode")
		}
	case ObservedOutputPresent:
		if wire.Writer == nil || wire.ContentBase64 == nil || wire.ExecutableMode == nil {
			return ObservedOutputState{}, fmt.Errorf("present observed output state must carry writer, content, and executable mode")
		}
		content, err := base64.StdEncoding.DecodeString(*wire.ContentBase64)
		if err != nil {
			return ObservedOutputState{}, fmt.Errorf("decode observed output content: %w", err)
		}
		state.Writer = *wire.Writer
		state.Content = content
		state.ExecutableMode = *wire.ExecutableMode
	case ObservedOutputDeleted:
		if wire.Writer == nil {
			return ObservedOutputState{}, fmt.Errorf("deleted observed output state must carry writer")
		}
		if wire.ContentBase64 != nil || wire.ExecutableMode != nil {
			return ObservedOutputState{}, fmt.Errorf("deleted observed output state cannot carry content or executable mode")
		}
		state.Writer = *wire.Writer
	default:
		return ObservedOutputState{}, fmt.Errorf("unsupported observed output state %q", wire.State)
	}
	if err := validateObservedOutputState(state); err != nil {
		return ObservedOutputState{}, err
	}
	canonical, err := EncodeObservedOutputState(state)
	if err != nil {
		return ObservedOutputState{}, err
	}
	if !bytes.Equal(data, canonical) {
		return ObservedOutputState{}, fmt.Errorf("observed output state is not canonically encoded")
	}
	return state, nil
}

func observedOutputStatesEqual(left, right ObservedOutputState) bool {
	return left.Disposition == right.Disposition &&
		left.Writer == right.Writer &&
		left.ExecutableMode == right.ExecutableMode &&
		bytes.Equal(left.Content, right.Content)
}

// MergeObservedOutputStates merges projected maximal predecessor states.
// Absent states contribute no writer. Repeated projections of one writer must
// be byte-for-byte identical; two distinct writers are unordered and therefore
// ambiguous.
func MergeObservedOutputStates(states []ObservedOutputState) (ObservedOutputState, error) {
	merged := ObservedOutputState{Disposition: ObservedOutputAbsent}
	mergedOrdinal := -1
	for ordinal, state := range states {
		if err := validateObservedOutputState(state); err != nil {
			return ObservedOutputState{}, fmt.Errorf("observed output state ordinal %d: %w", ordinal, err)
		}
		if state.Disposition == ObservedOutputAbsent {
			continue
		}
		if mergedOrdinal < 0 {
			merged = state
			merged.Content = bytes.Clone(state.Content)
			mergedOrdinal = ordinal
			continue
		}
		if state.Writer != merged.Writer {
			return ObservedOutputState{}, fmt.Errorf(
				"observed output states have unordered writers %q at ordinal %d and %q at ordinal %d",
				merged.Writer, mergedOrdinal, state.Writer, ordinal,
			)
		}
		if !observedOutputStatesEqual(merged, state) {
			return ObservedOutputState{}, fmt.Errorf(
				"observed output writer %q has inconsistent states at ordinals %d and %d",
				state.Writer, mergedOrdinal, ordinal,
			)
		}
	}
	return merged, nil
}
