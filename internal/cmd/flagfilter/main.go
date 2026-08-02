package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	inPath := flag.String("in", "", "Input response file")
	outPath := flag.String("out", "", "Output response file")
	var remove repeatedFlag
	var removePrefix repeatedFlag
	flag.Var(&remove, "remove", "Flag token to remove. May be repeated.")
	flag.Var(&removePrefix, "remove_prefix", "Flag token prefix to remove. May be repeated.")
	flag.Parse()

	if *inPath == "" || *outPath == "" {
		flag.PrintDefaults()
		os.Exit(2)
	}

	flags, err := readResponseFile(*inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read input: %v\n", err)
		os.Exit(1)
	}

	filtered := filterFlags(flags, remove, removePrefix)
	if err := os.WriteFile(*outPath, []byte(responseFile(filtered)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(1)
	}
}

func readResponseFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var flags []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		flag := strings.TrimSpace(scanner.Text())
		if flag != "" {
			flags = append(flags, flag)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return flags, nil
}

func filterFlags(flags []string, remove, removePrefix []string) []string {
	if len(remove) == 0 && len(removePrefix) == 0 {
		return append([]string(nil), flags...)
	}
	removeSet := map[string]bool{}
	for _, flag := range remove {
		if flag != "" {
			removeSet[flag] = true
		}
	}
	out := make([]string, 0, len(flags))
	for _, flag := range flags {
		shouldRemove := removeSet[flag]
		if !shouldRemove {
			for _, prefix := range removePrefix {
				if prefix != "" && strings.HasPrefix(flag, prefix) {
					shouldRemove = true
					break
				}
			}
		}
		if !shouldRemove {
			out = append(out, flag)
		}
	}
	return out
}

func responseFile(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	return strings.Join(flags, "\n") + "\n"
}
