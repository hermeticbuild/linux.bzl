package main

import "runtime/pprof"

// This file belongs only to kconfig_parse_heap_lib, never the ordinary planner.
// Its reachability intentionally enables Go's allocation sampling at startup.
func init() {
	diagnosticHeapWriter = pprof.WriteHeapProfile
}
