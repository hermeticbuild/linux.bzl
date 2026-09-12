package mapped_kernel_test

import (
	"os"
	"testing"
)

// This object is compiled directly by the family-selected compiler contract,
// with no dependency on any scanner, supplemental answer, or generated header.
// Each value is a comparison, not a hardcoded claim about builtin support.
func familyIntrinsicMeasurements(t *testing.T) map[string]byte {
	t.Helper()
	content, err := os.ReadFile(runfileFromEnv(t, "FAMILY_SMOKE_INTRINSIC_MEASUREMENT"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]byte{}
	for _, symbol := range []string{
		"measured_attribute_deprecated", "measured_attribute_unknown",
		"measured_builtin_dynamic_object_size", "measured_builtin_unknown",
	} {
		encoded := compiledObjectSymbolByte(t, content, symbol)
		if encoded != 1 && encoded != 2 {
			t.Fatalf("independent compiler measurement %s has invalid encoded Boolean %d", symbol, encoded)
		}
		values[symbol] = encoded - 1
	}
	return values
}
