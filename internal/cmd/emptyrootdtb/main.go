package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

const (
	fdtMagic       = 0xd00dfeed
	fdtBeginNode   = 0x1
	fdtEndNode     = 0x2
	fdtProp        = 0x3
	fdtEnd         = 0x9
	fdtVersion     = 17
	fdtLastCompat  = 16
	fdtHeaderSize  = 40
	fdtReserveSize = 16
)

func main() {
	in := flag.String("in", "", "drivers/of/empty_root.dts input")
	out := flag.String("out", "", "output dtb")
	wrapperOut := flag.String("wrapper_out", "", "output assembly wrapper")
	section := flag.String("section", "", "assembly section for the embedded dtb")
	symbol := flag.String("symbol", "", "symbol prefix for the embedded dtb")
	flag.Parse()
	if *in == "" || *out == "" || *wrapperOut == "" || *section == "" || *symbol == "" {
		fmt.Fprintln(os.Stderr, "-in, -out, -wrapper_out, -section, and -symbol are required")
		os.Exit(2)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read input: %v\n", err)
		os.Exit(1)
	}
	dtb, err := emptyRootDTB(string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate dtb: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, dtb, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*wrapperOut, []byte(assemblyWrapper(*out, *section, *symbol)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write assembly wrapper: %v\n", err)
		os.Exit(1)
	}
}

func assemblyWrapper(dtbPath, section, symbol string) string {
	return fmt.Sprintf(`#include <asm-generic/vmlinux.lds.h>
.section %s,"a"
.balign STRUCT_ALIGNMENT
.global %s_begin
%s_begin:
.incbin %q
.global %s_end
%s_end:
.balign STRUCT_ALIGNMENT
`, section, symbol, symbol, dtbPath, symbol, symbol)
}

func emptyRootDTB(source string) ([]byte, error) {
	hasCellProperties, err := parseEmptyRootDTS(source)
	if err != nil {
		return nil, err
	}

	var structBlock bytes.Buffer
	write32(&structBlock, fdtBeginNode)
	structBlock.Write([]byte{0, 0, 0, 0})
	var stringsBlock []byte
	if hasCellProperties {
		writeProp32(&structBlock, 0, 2)
		writeProp32(&structBlock, uint32(len("#address-cells")+1), 2)
		stringsBlock = []byte("#address-cells\x00#size-cells\x00")
	}
	write32(&structBlock, fdtEndNode)
	write32(&structBlock, fdtEnd)

	offMemRsvmap := uint32(fdtHeaderSize)
	offStruct := uint32(fdtHeaderSize + fdtReserveSize)
	offStrings := offStruct + uint32(structBlock.Len())
	totalSize := offStrings + uint32(len(stringsBlock))

	var out bytes.Buffer
	write32(&out, fdtMagic)
	write32(&out, totalSize)
	write32(&out, offStruct)
	write32(&out, offStrings)
	write32(&out, offMemRsvmap)
	write32(&out, fdtVersion)
	write32(&out, fdtLastCompat)
	write32(&out, 0)
	write32(&out, uint32(len(stringsBlock)))
	write32(&out, uint32(structBlock.Len()))
	write64(&out, 0)
	write64(&out, 0)
	out.Write(structBlock.Bytes())
	out.Write(stringsBlock)
	return out.Bytes(), nil
}

func parseEmptyRootDTS(source string) (bool, error) {
	tokens, err := lexEmptyRootDTS(source)
	if err != nil {
		return false, err
	}
	emptyRoot := []string{"/dts-v1/", ";", "/", "{", "}", ";"}
	rootWithCells := []string{
		"/dts-v1/", ";", "/", "{",
		"#address-cells", "=", "<", "0x02", ">", ";",
		"#size-cells", "=", "<", "0x02", ">", ";",
		"}", ";",
	}
	switch {
	case equalTokens(tokens, emptyRoot):
		return false, nil
	case equalTokens(tokens, rootWithCells):
		return true, nil
	default:
		return false, fmt.Errorf("unsupported empty root DTS structure")
	}
}

func lexEmptyRootDTS(source string) ([]string, error) {
	var tokens []string
	for pos := 0; pos < len(source); {
		switch {
		case isDTSWhitespace(source[pos]):
			pos++
		case source[pos] == '/' && pos+1 < len(source) && source[pos+1] == '/':
			pos += 2
			for pos < len(source) && source[pos] != '\n' {
				pos++
			}
		case source[pos] == '/' && pos+1 < len(source) && source[pos+1] == '*':
			end := pos + 2
			for end+1 < len(source) && (source[end] != '*' || source[end+1] != '/') {
				end++
			}
			if end+1 >= len(source) {
				return nil, fmt.Errorf("unterminated block comment in empty root DTS")
			}
			pos = end + 2
		case len(source)-pos >= len("/dts-v1/") && source[pos:pos+len("/dts-v1/")] == "/dts-v1/":
			tokens = append(tokens, "/dts-v1/")
			pos += len("/dts-v1/")
		case source[pos] == '/' || isDTSPunctuation(source[pos]):
			tokens = append(tokens, source[pos:pos+1])
			pos++
		default:
			end := pos
			for end < len(source) &&
				!isDTSWhitespace(source[end]) &&
				source[end] != '/' &&
				!isDTSPunctuation(source[end]) {
				end++
			}
			tokens = append(tokens, source[pos:end])
			pos = end
		}
	}
	return tokens, nil
}

func isDTSWhitespace(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

func isDTSPunctuation(value byte) bool {
	switch value {
	case '{', '}', ';', '=', '<', '>':
		return true
	default:
		return false
	}
}

func equalTokens(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func writeProp32(buf *bytes.Buffer, nameOffset, value uint32) {
	write32(buf, fdtProp)
	write32(buf, 4)
	write32(buf, nameOffset)
	write32(buf, value)
}

func write32(buf *bytes.Buffer, value uint32) {
	_ = binary.Write(buf, binary.BigEndian, value)
}

func write64(buf *bytes.Buffer, value uint64) {
	_ = binary.Write(buf, binary.BigEndian, value)
}
