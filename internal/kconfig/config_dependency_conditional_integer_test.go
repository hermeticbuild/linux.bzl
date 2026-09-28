package kconfig

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestConfigDependencyConditionalIntegerValues(t *testing.T) {
	for _, test := range []struct {
		text string
		want int64
	}{
		{"0", 0}, {"00", 0}, {"077", 63}, {"0x7f", 127}, {"0X7F", 127},
		{"2147483648", 2147483648}, {"0xffffffff", 4294967295},
		{"0x7fffffffffffffff", 9223372036854775807}, {"0777777777777777777777", 9223372036854775807},
		{"1l + 2L + 3ll + 4LL", 10},
		{"- 9223372036854775807", -9223372036854775807},
		{"- - 7", 7}, {"+ + 7", 7}, {"! ! - 7", 1}, {"! 0", 1},
		{"1 + 2 * 3", 7}, {"( 1 + 2 ) * 3", 9},
		{"8 - 3 - 2", 3}, {"16 / 4 / 2", 2}, {"8 / 3", 2}, {"8 % 3", 2},
		{"- 8 / 3", -2}, {"- 8 % 3", -2}, {"8 / - 3", -2}, {"8 % - 3", 2},
		{"1 << 2 + 1", 8}, {"16 >> 1 >> 1", 4}, {"1 << 62", 4611686018427387904},
		{"0 << 63", 0}, {"9223372036854775807 >> 63", 0},
		{"1 | 2 & 4", 1}, {"1 ^ 3 & 2", 3}, {"1 | 2 ^ 3", 1},
		{"4 & 1 == 0", 0}, {"1 == 2 < 3", 1}, {"3 > 2 == 1", 1},
		{"1 < 2 < 3", 1}, {"3 > 2 > 1", 0}, {"1 << 2 == 4", 1},
		{"- 1 < 0", 1}, {"- 2 <= - 2", 1}, {"3 >= 4", 0}, {"3 != 4", 1},
		{"1 || 0 && 0", 1}, {"0 && 0 || 3", 1}, {"3 && 4", 1},
		{"1 ? 0 ? 2 : 3 : 4", 3}, {"0 ? 1 : 0 ? 2 : 3", 3},
		{"0 || 1 ? 6 : 7", 6}, {"1 ? 0 : 1 || 1", 0},
		{"( 0 ? 1 : 2 ) + 3", 5}, {"- 7 ? - 4 : 9", -4},
		{"9223372036854775807 - 9223372036854775807", 0},
		{"- 9223372036854775807 + 9223372036854775807", 0},
		{"- 9223372036854775807 / - 1", 9223372036854775807},
		{"- 9223372036854775807 * - 1", 9223372036854775807},
	} {
		t.Run(test.text, func(t *testing.T) {
			tokens := strings.Fields(test.text)
			original := slices.Clone(tokens)
			got, reason := configDependencyConditionalIntegerValue(tokens, nil)
			if reason != "" || got != test.want {
				t.Fatalf("value = %d reason %q, want %d", got, reason, test.want)
			}
			wantState := configDependencyMacroDefined
			if test.want == 0 {
				wantState = configDependencyMacroUndefined
			}
			if state, reason := configDependencyConditionalInteger(tokens, nil); reason != "" || state != wantState {
				t.Fatalf("state = %d reason %q, want %d", state, reason, wantState)
			}
			if !slices.Equal(tokens, original) {
				t.Fatal("mutated caller tokens")
			}
		})
	}
}

func TestConfigDependencyConditionalIntegerShortCircuit(t *testing.T) {
	for _, test := range []struct {
		text string
		want int64
	}{
		{"0 && ( 1 / 0 )", 0}, {"1 || ( 1 / 0 )", 1},
		{"0 && ( 9223372036854775807 + 1 )", 0},
		{"1 ? 7 : ( 1 / 0 )", 7}, {"0 ? ( 1 / 0 ) : 9", 9},
		{"1 || ( 1 << 64 )", 1}, {"0 && ( - 1 & 7 )", 0},
		{"0 ? ( - 9223372036854775807 - 1 ) : 3", 3},
		{"0 && ( 0 ? ( 1 / 0 ) : ( 2 / 0 ) )", 0},
	} {
		if value, reason := configDependencyConditionalIntegerValue(strings.Fields(test.text), nil); reason != "" || value != test.want {
			t.Errorf("%s: value %d reason %q, want %d", test.text, value, reason, test.want)
		}
	}
}

func TestConfigDependencyConditionalIntegerRejectsUnsafeOrPartialExpressions(t *testing.T) {
	for _, text := range []string{
		"", "( )", "( 1", "1 )", "1 2", "1 +", "+", "? 1 : 2", "1 ? : 2", "1 ? 2", "1 : 2",
		"1 , 2", "1 ? 2 , 3 : 4", "1 = 2", "1 += 2", "1 ++", "1 ** 2", "( int ) 1", "F ( 1 )",
		"0b10", "09", "0x", "0xg", "1U", "1u", "1UL", "1LU", "1ull", "1LLU", "1lL", "1Ll", "1LLL",
		"1.0", ".5", "1e2", "0x1p2", "1'000", "1_000", "'A'", "L'A'", "u8'a'", "\"text\"",
		"9223372036854775808", "0x8000000000000000", "01777777777777777777777", "18446744073709551615",
		"- 9223372036854775808", "9223372036854775807 + 1", "- 9223372036854775807 - 1",
		"9223372036854775807 - - 1", "- 9223372036854775807 + - 1",
		"9223372036854775807 * 2", "- 9223372036854775807 * 2", "3037000500 * 3037000500",
		"1 / 0", "0 % 0", "1 << - 1", "1 >> - 1", "1 << 64", "0 << 64", "1 >> 64",
		"1 << 63", "3 << 62", "- 1 << 0", "- 1 >> 1", "- 1 & 0", "0 | - 1", "- 1 ^ 0", "~ 0",
		"1 || ( 1 + )", "0 && ( 1 + )", "1 ? 1 :", "1 ? 1 : 0x8000000000000000",
		"1 || 1U", "0 && 1.0", "1 ? 2 : 'a'", "0 && ~ 0",
		"defined", "defined ( NAME )", "true", "false", "not 0", "1 and 0", "bitand",
		"\\u0041", "CONFIG_♥", "1+2", "0x1.0p1",
	} {
		t.Run(text, func(t *testing.T) {
			tokens := strings.Fields(text)
			state, reason := configDependencyConditionalInteger(tokens, func(string) bool { return true })
			if reason == "" || state != configDependencyMacroUnknown {
				t.Fatalf("accepted unsupported expression: state %d reason %q", state, reason)
			}
			if value, why := configDependencyConditionalIntegerValue(tokens, func(string) bool { return true }); why == "" || value != 0 {
				t.Fatalf("failure leaked a numeric value: %d %q", value, why)
			}
		})
	}
}

func TestConfigDependencyConditionalIntegerUndefinedAuthority(t *testing.T) {
	for _, predicate := range []func(string) bool{nil, func(string) bool { return false }} {
		for _, text := range []string{"MISSING", "1 || MISSING", "0 && MISSING", "1 ? 2 : MISSING"} {
			if state, reason := configDependencyConditionalInteger(strings.Fields(text), predicate); reason == "" || state != configDependencyMacroUnknown {
				t.Fatalf("unproven identifier accepted in %s", text)
			}
		}
	}
	seen := map[string]int{}
	undefined := func(name string) bool {
		seen[name]++
		return name == "CONFIG_ABSENT" || name == "OTHER_ABSENT"
	}
	if state, reason := configDependencyConditionalInteger([]string{"1", "?", "CONFIG_ABSENT", ":", "OTHER_ABSENT"}, undefined); reason != "" || state != configDependencyMacroUndefined {
		t.Fatalf("authenticated identifiers rejected: %d %q", state, reason)
	}
	if !maps.Equal(seen, map[string]int{"CONFIG_ABSENT": 1, "OTHER_ABSENT": 1}) {
		t.Fatalf("did not authenticate both parsed arms: %#v", seen)
	}
	for _, keyword := range []string{"defined", "true", "false", "and", "or", "not", "bitand", "bitor", "compl", "xor"} {
		called := false
		state, reason := configDependencyConditionalInteger([]string{keyword}, func(string) bool { called = true; return true })
		if reason == "" || state != configDependencyMacroUnknown || called {
			t.Fatalf("keyword %q was accepted as an undefined identifier", keyword)
		}
	}
}

func TestConfigDependencyConditionalIntegerBudgets(t *testing.T) {
	for _, tokens := range [][]string{
		nil, {""}, {strings.Repeat("0", 129)},
		slices.Repeat([]string{"0"}, configDependencyConditionalIntegerMaxTokens+1),
		slices.Repeat([]string{strings.Repeat("0", 128)}, configDependencyConditionalIntegerMaxBytes/128+1),
		append(append(slices.Repeat([]string{"("}, 129), "1"), slices.Repeat([]string{")"}, 129)...),
		append(slices.Repeat([]string{"!"}, 129), "1"),
	} {
		if state, reason := configDependencyConditionalInteger(tokens, nil); reason == "" || state != configDependencyMacroUnknown {
			t.Fatalf("unbounded or empty input accepted: %d tokens", len(tokens))
		}
	}
	// A large shallow chain exercises iterative left associativity without
	// consuming stack depth proportional to expression length.
	tokens := []string{"1"}
	for range 1000 {
		tokens = append(tokens, "+", "1")
	}
	if value, reason := configDependencyConditionalIntegerValue(tokens, nil); reason != "" || value != 1001 {
		t.Fatalf("bounded shallow chain rejected: %d %q", value, reason)
	}
}

func TestConfigDependencyConditionalIntegerConcurrentIndependentCalls(t *testing.T) {
	tokens := []string{"1", "?", "A", "+", "7", ":", "1", "/", "0"}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 100 {
				value, reason := configDependencyConditionalIntegerValue(tokens, func(name string) bool { return name == "A" })
				if reason != "" || value != 7 {
					t.Errorf("concurrent result %d %q", value, reason)
					return
				}
			}
		})
	}
	workers.Wait()
}

func FuzzConfigDependencyConditionalInteger(f *testing.F) {
	for _, source := range []string{"1 + 2 * 3", "1 ? 7 : 1 / 0", "0 && 1 / 0", "- 9223372036854775807", "defined ( NAME )"} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > configDependencyConditionalIntegerMaxBytes {
			t.Skip()
		}
		tokens := strings.Fields(source)
		original := slices.Clone(tokens)
		value, reason := configDependencyConditionalIntegerValue(tokens, nil)
		if reason != "" && value != 0 {
			t.Fatal("failed parse leaked a value")
		}
		if reason == "" && (value < configDependencyConditionalIntegerMin || value > configDependencyConditionalIntegerMax) {
			t.Fatal("successful value outside the portable signed range")
		}
		if !slices.Equal(tokens, original) {
			t.Fatal("input token slice was mutated")
		}
	})
}

func BenchmarkConfigDependencyConditionalInteger(b *testing.B) {
	tokens := strings.Fields("( 6 * 65536 + 1 * 256 + 90 ) >= ( 5 * 65536 + 10 * 256 + 0 ) && ( 1 << 6 ) == 64")
	b.ReportAllocs()
	for b.Loop() {
		if state, reason := configDependencyConditionalInteger(tokens, nil); reason != "" || state != configDependencyMacroDefined {
			b.Fatal(strconv.Itoa(int(state)) + ": " + reason)
		}
	}
}
