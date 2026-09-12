package template_arguments

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestLargeMappedArguments(t *testing.T) {
	for index, env := range []string{"MAPPED_TRANSPORT_OUTPUT", "COLLIDING_TRANSPORT_OUTPUT"} {
		root, err := runfiles.Rlocation(os.Getenv(env))
		if err != nil {
			t.Fatal(err)
		}
		collision := index == 1
		data, err := os.ReadFile(filepath.Join(root, "copied.txt"))
		if err != nil || string(data) != "fastbuild\n" {
			t.Fatalf("copy through transported tree path: %q, %v", data, err)
		}
		files, err := filepath.Glob(filepath.Join(root, "argument-chunks/fixture/*.json"))
		if err != nil || len(files) < 40 {
			t.Fatalf("expected bounded chunks for >3 MiB argv, got %d, %v", len(files), err)
		}
		var args []string
		for _, filename := range files {
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			var chunk []string
			if err := json.Unmarshal(data, &chunk); err != nil {
				t.Fatal(err)
			}
			args = append(args, chunk...)
		}
		if len(args) < 7 || args[0] != "-copy_tree_file" || args[1] != "-tree" || args[3] != "-path" || args[4] != "payload.txt" || args[5] != "-output" {
			t.Fatalf("bad transported header: %q", args[:min(7, len(args))])
		}
		values := args[7:]
		var want []string
		for ordinal := range 15000 {
			want = append(want, "-action_arg", fmt.Sprintf("%d:", ordinal)+strings.Repeat("x", 240))
		}
		for _, value := range []string{"", "@literal", "-argument_chunks", "a=b=c", "line\nwith\ttabs\r", "duplicate", "duplicate"} {
			want = append(want, "-action_arg", value)
		}
		if len(values) < len(want) || !slices.Equal(values[:len(want)], want) {
			t.Fatal("transport changed order, count or literal argument bytes")
		}
		paths := values[len(want):]
		if len(paths) != 2+2*index {
			t.Fatalf("wrong path arguments: %q", paths)
		}
		for offset := 0; offset < len(paths); offset += 2 {
			if paths[offset] != "-action_arg" || !strings.HasSuffix(paths[offset+1], "/payload.txt=suffix") {
				t.Fatalf("bad joined File argument: %q", paths[offset:offset+2])
			}
			mapped := strings.Contains(paths[offset+1], "=bazel-out/cfg/")
			if mapped == collision {
				t.Fatalf("mapping decision mismatch (collision=%v): %q", collision, paths[offset+1])
			}
		}
	}
}
