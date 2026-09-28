package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hermeticbuild/linux.bzl/internal/kconfig"
)

func testKbuildFrontierEvent(name string) kbuildRecursiveMakeFrontierEvent {
	return kbuildRecursiveMakeFrontierEvent{invocation: name}
}

func testJoinKbuildFrontierValue(
	path, owner string,
	origin *kbuildRecursiveMakeFrontier,
) kbuildFrontierValue {
	return kbuildFrontierValue{
		artifact: kconfig.CompactKbuildVisibleArtifact{
			Path: path, Profile: owner, Target: path,
		},
		origin: origin,
	}
}

func testKbuildFrontierReplayResult(
	path string,
	value kbuildFrontierValue,
) frontierReplayResult {
	return frontierReplayResult{
		state:   kbuildFrontierSet(kbuildFrontierState{}, path, value),
		touched: kbuildPathSetAdd(emptyKbuildPathSet(), path),
	}
}

func TestKbuildRecursiveMakeFrontierJoinElidesCausalAncestors(t *testing.T) {
	builder := newKbuildRecursiveMakeFrontierBuilder()
	ancestor := builder.sequence(nil, testKbuildFrontierEvent("ancestor"))
	descendant := builder.sequence(ancestor, testKbuildFrontierEvent("descendant"))
	independent := builder.sequence(nil, testKbuildFrontierEvent("independent"))
	nested := builder.join(ancestor, independent)

	got := builder.join(nested, descendant)
	if got == nil || got.hasEvent {
		t.Fatalf("joined frontier = %#v, want a prerequisite join", got)
	}
	if len(got.parents) != 2 {
		t.Fatalf("joined parents = %#v, want descendant and independent", got.parents)
	}
	parents := map[string]bool{}
	for _, parent := range got.parents {
		parents[parent.id] = true
	}
	if parents[ancestor.id] || !parents[descendant.id] || !parents[independent.id] {
		t.Fatalf("joined parent IDs = %#v, want only descendant %q and independent %q", parents, descendant.id, independent.id)
	}
	if again := builder.join(independent, descendant); again != got {
		t.Fatalf("equivalent reordered join was not interned: got %p, want %p", again, got)
	}
}

func TestKbuildRecursiveMakeFrontierJoinScalesAcrossIndependentParents(t *testing.T) {
	const parentCount = 4096
	builder := newKbuildRecursiveMakeFrontierBuilder()
	parents := make([]*kbuildRecursiveMakeFrontier, parentCount)
	for index := range parents {
		parents[index] = builder.sequence(nil, testKbuildFrontierEvent(fmt.Sprintf("child-%04d", index)))
	}

	joined := builder.join(parents...)
	if joined == nil || joined.hasEvent {
		t.Fatalf("high-fanout frontier = %#v, want a prerequisite join", joined)
	}
	if got := len(joined.parents); got != parentCount {
		t.Fatalf("high-fanout join retained %d parents, want %d", got, parentCount)
	}
	for index := 1; index < len(joined.parents); index++ {
		if joined.parents[index-1].id >= joined.parents[index].id {
			t.Fatalf("high-fanout parents are not in stable ID order at %d", index)
		}
	}
}

func TestMergeKbuildFrontierReplayResultsPreservesCausalSemantics(t *testing.T) {
	t.Run("later writer wins", func(t *testing.T) {
		builder := newKbuildRecursiveMakeFrontierBuilder()
		older := builder.sequence(nil, testKbuildFrontierEvent("older"))
		newer := builder.sequence(older, testKbuildFrontierEvent("newer"))
		path := "generated/shared.h"
		olderValue := testJoinKbuildFrontierValue(path, "older-profile", older)
		newerValue := testJoinKbuildFrontierValue(path, "newer-profile", newer)

		merged, err := mergeKbuildFrontierReplayResults(
			kbuildFrontierState{},
			[]frontierReplayResult{
				testKbuildFrontierReplayResult(path, newerValue),
				testKbuildFrontierReplayResult(path, olderValue),
			},
			map[string]bool{},
		)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := kbuildFrontierGet(merged.state, path)
		if !ok || got.artifact != newerValue.artifact || got.origin != newer {
			t.Fatalf("merged value = %#v, want causally later writer %#v", got, newerValue)
		}
		if !kbuildPathSetContains(merged.touched, path) {
			t.Fatalf("merged touched paths omit %q", path)
		}
	})

	t.Run("untouched parent contributes initial value", func(t *testing.T) {
		builder := newKbuildRecursiveMakeFrontierBuilder()
		writer := builder.sequence(nil, testKbuildFrontierEvent("writer"))
		path := "include/generated/config.h"
		initialValue := testJoinKbuildFrontierValue(path, "initial-profile", nil)
		initial := kbuildFrontierSet(kbuildFrontierState{}, path, initialValue)
		written := testJoinKbuildFrontierValue(path, "writer-profile", writer)

		merged, err := mergeKbuildFrontierReplayResults(
			initial,
			[]frontierReplayResult{
				{state: initial, touched: emptyKbuildPathSet()},
				testKbuildFrontierReplayResult(path, written),
			},
			map[string]bool{},
		)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := kbuildFrontierGet(merged.state, path)
		if !ok || got.artifact != written.artifact || got.origin != writer {
			t.Fatalf("merged value = %#v, want writer over implicit initial value %#v", got, written)
		}
	})

	t.Run("incomparable writers conflict", func(t *testing.T) {
		builder := newKbuildRecursiveMakeFrontierBuilder()
		left := builder.sequence(nil, testKbuildFrontierEvent("left"))
		right := builder.sequence(nil, testKbuildFrontierEvent("right"))
		path := "shared.out"

		_, err := mergeKbuildFrontierReplayResults(
			kbuildFrontierState{},
			[]frontierReplayResult{
				testKbuildFrontierReplayResult(path, testJoinKbuildFrontierValue(path, "left", left)),
				testKbuildFrontierReplayResult(path, testJoinKbuildFrontierValue(path, "right", right)),
			},
			map[string]bool{},
		)
		if err == nil || !strings.Contains(err.Error(), "incomparable versions") ||
			!strings.Contains(err.Error(), path) {
			t.Fatalf("incomparable writer error = %v", err)
		}
	})

	t.Run("same origin conflicting values conflict", func(t *testing.T) {
		builder := newKbuildRecursiveMakeFrontierBuilder()
		origin := builder.sequence(nil, testKbuildFrontierEvent("writer"))
		path := "same-origin.out"

		_, err := mergeKbuildFrontierReplayResults(
			kbuildFrontierState{},
			[]frontierReplayResult{
				testKbuildFrontierReplayResult(path, testJoinKbuildFrontierValue(path, "first", origin)),
				testKbuildFrontierReplayResult(path, testJoinKbuildFrontierValue(path, "second", origin)),
			},
			map[string]bool{},
		)
		if err == nil || !strings.Contains(err.Error(), "conflicting values with the same origin") {
			t.Fatalf("same-origin conflict error = %v", err)
		}
	})
}

func TestMergeKbuildFrontierReplayResultsScalesAcrossSparseParents(t *testing.T) {
	const parentCount = 4096
	builder := newKbuildRecursiveMakeFrontierBuilder()
	parents := make([]frontierReplayResult, parentCount)
	for index := range parents {
		path := fmt.Sprintf("generated/path-%04d.h", index)
		origin := builder.sequence(nil, testKbuildFrontierEvent(fmt.Sprintf("child-%04d", index)))
		parents[index] = testKbuildFrontierReplayResult(
			path, testJoinKbuildFrontierValue(path, fmt.Sprintf("profile-%04d", index), origin),
		)
	}

	merged, err := mergeKbuildFrontierReplayResults(
		kbuildFrontierState{}, parents, map[string]bool{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := kbuildFrontierLen(merged.state); got != parentCount {
		t.Fatalf("sparse high-fanout state has %d paths, want %d", got, parentCount)
	}
	if got := kbuildPathSetLen(merged.touched); got != parentCount {
		t.Fatalf("sparse high-fanout touched set has %d paths, want %d", got, parentCount)
	}
	for index := range parents {
		path := fmt.Sprintf("generated/path-%04d.h", index)
		value, ok := kbuildFrontierGet(merged.state, path)
		if !ok || value.artifact.Profile != fmt.Sprintf("profile-%04d", index) {
			t.Fatalf("merged sparse path %q = %#v, want its unique writer", path, value)
		}
	}
}
