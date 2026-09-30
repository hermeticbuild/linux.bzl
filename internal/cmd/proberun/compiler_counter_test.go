package main

import (
	"strings"
	"testing"
)

// Exercise the real runner's post-interpolation, source-shell decoding and
// candidate-ownership boundary. The helper subprocess asserts the exact argv;
// configured GCC/Clang sequence semantics have separate actual-toolchain tests.
func TestCompilerCounterRunnerProtectsMeasuredBinding(t *testing.T) {
	for _, mode := range []string{"literal", "fragment", "source-shell"} {
		for _, test := range []struct {
			name      string
			arguments []string
			invalid   bool
		}{
			{"ordinary macros", []string{"-DOTHER=73", "-UOLD"}, false},
			{"unrelated operator", []string{"-D__has_attribute(x)=7"}, false},
			{"define joined", []string{"-D__COUNTER__=73"}, true},
			{"define separate", []string{"-D", "__COUNTER__=73"}, true},
			{"function override", []string{"-D__COUNTER__(x)=x"}, true},
			{"undef joined", []string{"-U__COUNTER__"}, true},
			{"undef separate", []string{"-U", "__COUNTER__"}, true},
			{"forwarded override", []string{"-Wp,-D__COUNTER__=73"}, true},
		} {
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				runCompilerIntrinsicNamedSourceArgumentsForTest(t, test.arguments, mode,
					"compiler-counter-sequence", strings.Repeat("__COUNTER__\n", 16), test.invalid)
			})
		}
	}
}
