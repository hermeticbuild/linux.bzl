package kconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestLinuxProbeSymbolMatcherMatchesRegexp(t *testing.T) {
	validA := linuxProbeSymbolPrefix + strings.Repeat("a", linuxProbeSymbolDigestLength)
	validB := linuxProbeSymbolPrefix + strings.Repeat("0123456789abcdef", 4)
	for _, value := range []string{
		"",
		linuxProbeSymbolPrefix,
		linuxProbeSymbolPrefix + strings.Repeat("a", linuxProbeSymbolDigestLength-1),
		linuxProbeSymbolPrefix + strings.Repeat("a", 17) + "g" + strings.Repeat("a", 46),
		validA,
		"before/" + validA + "/after",
		validA + validB,
		linuxProbeSymbolPrefix + "X" + validA,
		linuxProbeSymbolPrefix + strings.Repeat("f", linuxProbeSymbolDigestLength+1),
		validA + ":" + linuxProbeSymbolPrefix + strings.Repeat("A", linuxProbeSymbolDigestLength) + ":" + validB,
	} {
		t.Run(value, func(t *testing.T) {
			if got, want := linuxProbeSymbolPattern.MatchString(value), linuxProbeSymbolRegexp.MatchString(value); got != want {
				t.Fatalf("MatchString(%q) = %v, want %v", value, got, want)
			}
			if got, want := linuxProbeSymbolPattern.Match([]byte(value)), linuxProbeSymbolRegexp.MatchString(value); got != want {
				t.Fatalf("Match(%q) = %v, want %v", value, got, want)
			}
			if got, want := string(linuxProbeSymbolPattern.Find([]byte(value))), string(linuxProbeSymbolRegexp.Find([]byte(value))); got != want {
				t.Fatalf("Find(%q) = %q, want %q", value, got, want)
			}
			for _, limit := range []int{-1, 0, 1, 2, 20} {
				if got, want := linuxProbeSymbolPattern.FindAllString(value, limit), linuxProbeSymbolRegexp.FindAllString(value, limit); !reflect.DeepEqual(got, want) {
					t.Fatalf("FindAllString(%q, %d) = %#v, want %#v", value, limit, got, want)
				}
				if got, want := linuxProbeSymbolPattern.FindAllStringIndex(value, limit), linuxProbeSymbolRegexp.FindAllStringIndex(value, limit); !reflect.DeepEqual(got, want) {
					t.Fatalf("FindAllStringIndex(%q, %d) = %#v, want %#v", value, limit, got, want)
				}
			}
			var ranged []string
			for match := range linuxProbeSymbolPattern.AllString(value) {
				ranged = append(ranged, match)
			}
			if want := linuxProbeSymbolRegexp.FindAllString(value, -1); !reflect.DeepEqual(ranged, want) {
				t.Fatalf("AllString(%q) = %#v, want %#v", value, ranged, want)
			}
			var rangedIndices [][]int
			for start, end := range linuxProbeSymbolPattern.AllStringIndex(value) {
				rangedIndices = append(rangedIndices, []int{start, end})
			}
			if want := linuxProbeSymbolRegexp.FindAllStringIndex(value, -1); !reflect.DeepEqual(rangedIndices, want) {
				t.Fatalf("AllStringIndex(%q) = %#v, want %#v", value, rangedIndices, want)
			}
			for _, replacement := range []string{"", "replacement", "$$", "$0", "${0}"} {
				if got, want := linuxProbeSymbolPattern.ReplaceAllString(value, replacement), linuxProbeSymbolRegexp.ReplaceAllString(value, replacement); got != want {
					t.Fatalf("ReplaceAllString(%q, %q) = %q, want %q", value, replacement, got, want)
				}
			}
			wrap := func(token string) string { return "<" + token + ">" }
			if got, want := linuxProbeSymbolPattern.ReplaceAllStringFunc(value, wrap), linuxProbeSymbolRegexp.ReplaceAllStringFunc(value, wrap); got != want {
				t.Fatalf("ReplaceAllStringFunc(%q) = %q, want %q", value, got, want)
			}
		})
	}
}

var linuxProbeSymbolMatcherAllocationSink int

func TestLinuxProbeSymbolMatcherRangesDoNotAllocate(t *testing.T) {
	validA := linuxProbeSymbolPrefix + strings.Repeat("a", linuxProbeSymbolDigestLength)
	validB := linuxProbeSymbolPrefix + strings.Repeat("0123456789abcdef", 4)
	value := "before/" + validA + "/middle/" + validB + "/after"

	if allocations := testing.AllocsPerRun(1000, func() {
		total := 0
		for token := range linuxProbeSymbolPattern.AllString(value) {
			total += len(token)
		}
		linuxProbeSymbolMatcherAllocationSink = total
	}); allocations != 0 {
		t.Fatalf("AllString allocated %v objects per scan, want 0", allocations)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		total := 0
		for start, end := range linuxProbeSymbolPattern.AllStringIndex(value) {
			total += end - start
		}
		linuxProbeSymbolMatcherAllocationSink = total
	}); allocations != 0 {
		t.Fatalf("AllStringIndex allocated %v objects per scan, want 0", allocations)
	}
}
