package toolaction

import (
	"bytes"
	"strings"
	"testing"
)

const (
	observedWriterA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	observedWriterB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestObservedOutputStateRoundTrip(t *testing.T) {
	states := []ObservedOutputState{
		{Disposition: ObservedOutputAbsent},
		{Disposition: ObservedOutputDeleted, Writer: observedWriterA},
		{Disposition: ObservedOutputPresent, Writer: observedWriterA, Content: []byte("generated\x00bytes\n"), ExecutableMode: 0o101},
		{Disposition: ObservedOutputPresent, Writer: observedWriterB, Content: []byte{}},
	}
	for _, want := range states {
		t.Run(string(want.Disposition), func(t *testing.T) {
			encoded, err := EncodeObservedOutputState(want)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeObservedOutputState(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if !observedOutputStatesEqual(got, want) {
				t.Fatalf("decoded state = %#v, want %#v", got, want)
			}
			encodedAgain, err := EncodeObservedOutputState(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, encodedAgain) {
				t.Fatalf("encoding is not deterministic:\nfirst:  %q\nsecond: %q", encoded, encodedAgain)
			}
		})
	}
}

func TestObservedOutputStateRejectsInvalidValues(t *testing.T) {
	for _, state := range []ObservedOutputState{
		{},
		{Disposition: "unchanged"},
		{Disposition: ObservedOutputAbsent, Writer: observedWriterA},
		{Disposition: ObservedOutputAbsent, Content: []byte("unexpected")},
		{Disposition: ObservedOutputPresent},
		{Disposition: ObservedOutputPresent, Writer: "not-a-writer"},
		{Disposition: ObservedOutputPresent, Writer: observedWriterA, ExecutableMode: 0o200},
		{Disposition: ObservedOutputDeleted},
		{Disposition: ObservedOutputDeleted, Writer: observedWriterA, Content: []byte("unexpected")},
	} {
		if _, err := EncodeObservedOutputState(state); err == nil {
			t.Errorf("EncodeObservedOutputState(%#v) succeeded", state)
		}
	}
}

func TestObservedOutputStateRejectsMalformedWireData(t *testing.T) {
	valid, err := EncodeObservedOutputState(ObservedOutputState{
		Disposition: ObservedOutputPresent, Writer: observedWriterA, Content: []byte("bytes"), ExecutableMode: 0o100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		nil,
		[]byte(`{}` + "\n"),
		[]byte(`{"schema":"wrong","state":"absent"}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"unchanged"}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"absent","writer":"` + observedWriterA + `"}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"present","writer":"` + observedWriterA + `"}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"present","writer":"` + observedWriterA + `","content_base64":"***","executable_mode":0}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"present","writer":"` + observedWriterA + `","content_base64":"","executable_mode":128}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"deleted"}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"deleted","writer":"` + observedWriterA + `","content_base64":""}` + "\n"),
		[]byte(`{"schema":"linux-bzl-observed-output-state-v2","state":"absent","unknown":true}` + "\n"),
		append(append([]byte(nil), valid...), []byte("{}\n")...),
		[]byte(strings.TrimSuffix(string(valid), "\n")),
	} {
		if _, err := DecodeObservedOutputState(data); err == nil {
			t.Errorf("DecodeObservedOutputState(%q) succeeded", data)
		}
	}
}

func TestMergeObservedOutputStates(t *testing.T) {
	present := ObservedOutputState{
		Disposition: ObservedOutputPresent, Writer: observedWriterA, Content: []byte("selected"), ExecutableMode: 0o101,
	}
	for _, states := range [][]ObservedOutputState{
		{present},
		{{Disposition: ObservedOutputAbsent}, present},
		{present, {Disposition: ObservedOutputAbsent}, present},
	} {
		got, err := MergeObservedOutputStates(states)
		if err != nil {
			t.Fatal(err)
		}
		if !observedOutputStatesEqual(got, present) {
			t.Fatalf("merged state = %#v, want %#v", got, present)
		}
		got.Content[0] = 'X'
		if string(present.Content) != "selected" {
			t.Fatal("merged state aliases an input content slice")
		}
	}
	absent, err := MergeObservedOutputStates(nil)
	if err != nil || absent.Disposition != ObservedOutputAbsent || absent.Writer != "" {
		t.Fatalf("empty merge = %#v, %v", absent, err)
	}
	deleted, err := MergeObservedOutputStates([]ObservedOutputState{
		{Disposition: ObservedOutputAbsent},
		{Disposition: ObservedOutputDeleted, Writer: observedWriterA},
	})
	if err != nil || deleted.Disposition != ObservedOutputDeleted || deleted.Writer != observedWriterA {
		t.Fatalf("deleted merge = %#v, %v", deleted, err)
	}
}

func TestMergeObservedOutputStatesRejectsAmbiguityAndInconsistency(t *testing.T) {
	for _, test := range []struct {
		name   string
		states []ObservedOutputState
		want   string
	}{
		{
			name: "unordered writers",
			states: []ObservedOutputState{
				{Disposition: ObservedOutputPresent, Writer: observedWriterA, Content: []byte("same")},
				{Disposition: ObservedOutputPresent, Writer: observedWriterB, Content: []byte("same")},
			},
			want: "unordered writers",
		},
		{
			name: "same writer different bytes",
			states: []ObservedOutputState{
				{Disposition: ObservedOutputPresent, Writer: observedWriterA, Content: []byte("first")},
				{Disposition: ObservedOutputPresent, Writer: observedWriterA, Content: []byte("second")},
			},
			want: "inconsistent states",
		},
		{
			name: "same writer different disposition",
			states: []ObservedOutputState{
				{Disposition: ObservedOutputPresent, Writer: observedWriterA, Content: []byte("bytes")},
				{Disposition: ObservedOutputDeleted, Writer: observedWriterA},
			},
			want: "inconsistent states",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := MergeObservedOutputStates(test.states); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("merge error = %v, want %q", err, test.want)
			}
		})
	}
}
