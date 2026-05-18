// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
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
	flag.Parse()
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-in and -out are required")
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
}

func emptyRootDTB(source string) ([]byte, error) {
	for _, required := range []string{
		"/dts-v1/;",
		"#address-cells = <0x02>;",
		"#size-cells = <0x02>;",
	} {
		if !strings.Contains(source, required) {
			return nil, fmt.Errorf("empty root DTS missing %q", required)
		}
	}

	var structBlock bytes.Buffer
	write32(&structBlock, fdtBeginNode)
	structBlock.Write([]byte{0, 0, 0, 0})
	writeProp32(&structBlock, 0, 2)
	writeProp32(&structBlock, uint32(len("#address-cells")+1), 2)
	write32(&structBlock, fdtEndNode)
	write32(&structBlock, fdtEnd)

	stringsBlock := []byte("#address-cells\x00#size-cells\x00")
	for len(stringsBlock)%4 != 0 {
		stringsBlock = append(stringsBlock, 0)
	}

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
