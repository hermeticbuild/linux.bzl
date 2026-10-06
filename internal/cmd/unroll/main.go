// Command unroll reimplements lib/raid6/unroll.awk.
//
// Kbuild generates lib/raid6/int{1,2,4,8,16,32}.c from a single int.uc
// template with:
//
//	cmd_unroll = $(AWK) -v N=$* -f $(src)/unroll.awk < $< > $@
//
// No action in this project has an awk toolchain, so the filter is ported
// here. The upstream script is short enough to quote in full:
//
//	BEGIN { n = N + 0 }
//	{
//	    if (/\$\$/) { rep = n } else { rep = 1 }
//	    for (i = 0; i < rep; ++i) {
//	        tmp = $0
//	        gsub(/\$\$/, i, tmp)
//	        gsub(/\$#/, n, tmp)
//	        gsub(/\$\*/, "$", tmp)
//	        print tmp
//	    }
//	}
//
// A line containing "$$" is repeated n times with "$$" replaced by the
// iteration index; every line has "$#" replaced by n and "$*" replaced by a
// literal "$".
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	n := flag.Int("n", -1, "Unroll factor, awk's -vN=")
	in := flag.String("in", "", "Template input, for example lib/raid6/int.uc")
	out := flag.String("out", "", "Generated C output")
	flag.Parse()

	if *n < 0 || *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-n, -in, and -out are required")
		os.Exit(2)
	}
	if err := run(*n, *in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "unroll: %v\n", err)
		os.Exit(1)
	}
}

func run(n int, inPath, outPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(unroll(string(data), n)), 0o644)
}

// unroll applies the awk filter to input, producing the generated source.
func unroll(input string, n int) string {
	var out strings.Builder
	out.Grow(len(input))
	nText := strconv.Itoa(n)
	for _, line := range splitAwkRecords(input) {
		// awk repeats a line n times only when it contains "$$". With n == 0
		// such a line is dropped entirely. That is upstream behaviour and the
		// raid6 build relies on it; do not "fix" it.
		repeat := 1
		if strings.Contains(line, "$$") {
			repeat = n
		}
		substitute := strings.ContainsRune(line, '$')
		for i := 0; i < repeat; i++ {
			expanded := line
			if substitute {
				// The substitution order is load-bearing. "$*" becomes a
				// literal "$" last, so that the "$" it produces is not itself
				// treated as the start of another substitution.
				expanded = strings.ReplaceAll(expanded, "$$", strconv.Itoa(i))
				expanded = strings.ReplaceAll(expanded, "$#", nText)
				expanded = strings.ReplaceAll(expanded, "$*", "$")
			}
			// awk's print always appends ORS.
			out.WriteString(expanded)
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// splitAwkRecords splits input the way awk splits records on the default RS.
//
// A trailing newline terminates the last record rather than starting an empty
// one. Getting this wrong inserts a blank line inside a function body in
// int.uc, which is a correctness bug in the generated C rather than cosmetic.
func splitAwkRecords(input string) []string {
	if input == "" {
		return nil
	}
	records := strings.Split(input, "\n")
	if len(records) != 0 && records[len(records)-1] == "" {
		records = records[:len(records)-1]
	}
	return records
}
