package jsonrpc

import (
	"reflect"
	"testing"
)

func TestSampleEvenly(t *testing.T) {
	input := []int{0, 1, 2, 3, 4}
	tests := []struct {
		name  string
		count int
		want  []int
	}{
		{name: "empty", count: 0, want: []int{}},
		{name: "latest only", count: 1, want: []int{4}},
		{name: "even selection", count: 3, want: []int{0, 2, 4}},
		{name: "all", count: len(input), want: input},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sampleEvenly(input, test.count); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("sampleEvenly(%v, %d) = %v, want %v", input, test.count, got, test.want)
			}
		})
	}
}

func TestAllocateTargetsSupportsTypedKeys(t *testing.T) {
	groups := []allocationGroup[uint]{
		{key: 7, length: 6},
		{key: 9, length: 4},
	}
	got := allocateTargets(groups, 5)
	if got[7] != 3 || got[9] != 2 {
		t.Fatalf("allocateTargets() = %v, want map[7:3 9:2]", got)
	}
}

func TestClampCompatibilityRecordMaxCountEnforcesCeiling(t *testing.T) {
	for _, input := range []int{-1, -100, 0, 4001, 100_000} {
		if got := clampCompatibilityRecordMaxCount(input); got != maxCompatibilityRecordPoints {
			t.Fatalf("clampCompatibilityRecordMaxCount(%d) = %d, want %d", input, got, maxCompatibilityRecordPoints)
		}
	}
	for _, input := range []int{1, 120, 4000} {
		if got := clampCompatibilityRecordMaxCount(input); got != input {
			t.Fatalf("clampCompatibilityRecordMaxCount(%d) = %d, want %d", input, got, input)
		}
	}
}

func TestFilterVisiblePingTaskClientsStripsHiddenUUIDs(t *testing.T) {
	hidden := map[string]bool{
		"node-hidden-1": true,
		"node-hidden-2": true,
	}
	got := filterVisiblePingTaskClients([]string{"node-visible-a", "node-hidden-1", "", "node-visible-b", "node-hidden-2"}, hidden)
	want := []string{"node-visible-a", "node-visible-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterVisiblePingTaskClients() = %v, want %v", got, want)
	}
	empty := filterVisiblePingTaskClients([]string{"node-hidden-1"}, hidden)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("expected non-nil empty slice when all clients are hidden, got %#v", empty)
	}
}
