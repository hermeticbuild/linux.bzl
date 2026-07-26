package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var blistDefineRE = regexp.MustCompile(`\bdefine\s+BLIST_([A-Z0-9_]+)\b`)

func main() {
	in := flag.String("in", "", "include/scsi/scsi_devinfo.h input")
	out := flag.String("out", "", "Generated scsi_devinfo_tbl.c output")
	flag.Parse()

	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
		os.Exit(2)
	}
	if err := run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "scsidevinfo: %v\n", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	input, err := os.Open(in)
	if err != nil {
		return err
	}
	defer input.Close()

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	output, err := os.Create(out)
	if err != nil {
		return err
	}
	defer output.Close()

	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		matches := blistDefineRE.FindStringSubmatch(scanner.Text())
		if matches == nil {
			continue
		}
		fmt.Fprintf(output, "BLIST_FLAG_NAME(%s),\n", matches[1])
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}
	return nil
}
