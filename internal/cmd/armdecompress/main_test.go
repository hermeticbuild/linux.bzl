package main

import (
	"bytes"
	"strings"
	"testing"
)

const linux618ARMDecompressor = `// SPDX-License-Identifier: GPL-2.0
#define _LINUX_STRING_H_

#include <linux/compiler.h>
#include "misc.h"

#define STATIC static
#define STATIC_RW_DATA

#ifdef CONFIG_KERNEL_GZIP
#include "../../../../lib/decompress_inflate.c"
#endif

#ifdef CONFIG_KERNEL_LZ4
#include "../../../../lib/decompress_unlz4.c"
#endif

int do_decompress(u8 *input, int len, u8 *output, void (*error)(char *x))
{
	return __decompress(input, len, NULL, NULL, output, 0, NULL, error);
}
`

func TestGenerateARMDecompressorLinux618(t *testing.T) {
	generated, err := generateARMDecompressor([]byte(linux618ARMDecompressor))
	if err != nil {
		t.Fatal(err)
	}
	text := string(generated)
	if count := strings.Count(text, `#include "../../../../lib/decompress_unzstd.c"`); count != 1 {
		t.Fatalf("ZSTD decompressor include count = %d, want 1:\n%s", count, text)
	}
	zstd := strings.Index(text, "#ifdef CONFIG_KERNEL_ZSTD")
	entrypoint := strings.Index(text, "int do_decompress")
	if zstd < 0 || entrypoint < 0 || zstd > entrypoint {
		t.Fatalf("ZSTD decompressor was not inserted before do_decompress:\n%s", text)
	}
}

func TestGenerateARMDecompressorPreservesDownstreamSupport(t *testing.T) {
	downstream := strings.Replace(
		linux618ARMDecompressor,
		"#ifdef CONFIG_KERNEL_LZ4",
		"#ifdef CONFIG_KERNEL_ZSTD\n#include \"../../../../lib/decompress_unzstd.c\"\n#endif\n\n#ifdef CONFIG_KERNEL_LZ4",
		1,
	)
	generated, err := generateARMDecompressor([]byte(downstream))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, []byte(downstream)) {
		t.Fatalf("downstream ZSTD support was modified:\n%s", generated)
	}
}

func TestGenerateARMDecompressorRejectsUnknownShape(t *testing.T) {
	if _, err := generateARMDecompressor([]byte("void unrelated(void) {}\n")); err == nil {
		t.Fatal("source without do_decompress unexpectedly succeeded")
	}
}
