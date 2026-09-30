package main

import (
	"slices"
	"testing"
)

func TestKbuildInvocationVirtualPathAliasesRespectWorkingDirectory(t *testing.T) {
	for name, test := range map[string]struct {
		file      string
		directory string
		want      []string
	}{
		"root exposes graph path": {
			file: "generated/modules.order",
			want: []string{
				kbuildEvalObjectTree + "/generated/modules.order",
				"generated/modules.order",
			},
		},
		"nested path outside cwd": {
			file:      "generated/modules.order",
			directory: "drivers/net",
			want: []string{
				"../../generated/modules.order",
				kbuildEvalObjectTree + "/generated/modules.order",
			},
		},
		"nested path inside cwd": {
			file:      "drivers/net/modules.order",
			directory: "drivers/net",
			want: []string{
				kbuildEvalObjectTree + "/drivers/net/modules.order",
				"modules.order",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := kbuildInvocationVirtualPathAliases(test.file, test.directory)
			if !slices.Equal(got, test.want) {
				t.Fatalf("aliases = %#v, want %#v", got, test.want)
			}
		})
	}
}
