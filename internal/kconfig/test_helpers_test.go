package kconfig

import (
	"context"
	"strings"
	"testing"
)

func mustParseString(t *testing.T, fixture string) *Tree {
	t.Helper()
	tree, err := Parse(context.Background(), strings.NewReader(fixture), "Kconfig", Options{})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	return tree
}
