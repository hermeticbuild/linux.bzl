package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunGrepSupportsConfiguredProbeOptions(t *testing.T) {
	for _, test := range []struct {
		name       string
		args       []string
		input      string
		wantStatus int
		wantOutput string
	}{
		{
			name:       "quiet match",
			args:       []string{"-q", "pack-relative-relocs"},
			input:      "ld.lld: unknown argument pack-relative-relocs\n",
			wantStatus: 0,
		},
		{
			name:       "quiet inverse match",
			args:       []string{"-qv", "llvm"},
			input:      "GNU objcopy\n",
			wantStatus: 0,
		},
		{
			name:       "quiet case insensitive match",
			args:       []string{"-qi", "llvm"},
			input:      "LLVM version 22\n",
			wantStatus: 0,
		},
		{
			name:       "prints selected lines",
			args:       []string{"stack"},
			input:      "plain\nstack protector\n",
			wantStatus: 0,
			wantOutput: "stack protector\n",
		},
		{
			name:       "no match",
			args:       []string{"missing"},
			input:      "present\n",
			wantStatus: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			status := runGrep(
				test.args,
				strings.NewReader(test.input),
				&stdout,
				&stderr,
			)
			if status != test.wantStatus {
				t.Fatalf("status = %d, want %d; stderr=%q", status, test.wantStatus, stderr.String())
			}
			if stdout.String() != test.wantOutput {
				t.Fatalf("stdout = %q, want %q", stdout.String(), test.wantOutput)
			}
		})
	}
}
