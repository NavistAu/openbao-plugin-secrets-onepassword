package backend

import (
	"strconv"
	"testing"
)

func TestChunkIDs(t *testing.T) {
	cases := []struct {
		name     string
		ids      []string
		size     int
		wantLens []int
	}{
		{
			name:     "empty",
			ids:      nil,
			size:     50,
			wantLens: nil,
		},
		{
			name:     "exact cap",
			ids:      idsN(50),
			size:     50,
			wantLens: []int{50},
		},
		{
			name:     "cap plus one",
			ids:      idsN(51),
			size:     50,
			wantLens: []int{50, 1},
		},
		{
			name:     "well under cap",
			ids:      idsN(3),
			size:     50,
			wantLens: []int{3},
		},
		{
			name:     "several full chunks plus remainder",
			ids:      idsN(184),
			size:     50,
			wantLens: []int{50, 50, 50, 34},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkIDs(tc.ids, tc.size)
			if len(got) != len(tc.wantLens) {
				t.Fatalf("chunkIDs(%d ids) = %d chunks, want %d", len(tc.ids), len(got), len(tc.wantLens))
			}
			for i, wantLen := range tc.wantLens {
				if len(got[i]) != wantLen {
					t.Errorf("chunk %d: len = %d, want %d", i, len(got[i]), wantLen)
				}
			}
			// Order must be preserved: concatenating the chunks
			// reproduces the input.
			var flat []string
			for _, c := range got {
				flat = append(flat, c...)
			}
			if len(flat) != len(tc.ids) {
				t.Fatalf("flattened chunks have %d ids, want %d", len(flat), len(tc.ids))
			}
			for i := range flat {
				if flat[i] != tc.ids[i] {
					t.Errorf("flattened[%d] = %q, want %q", i, flat[i], tc.ids[i])
				}
			}
		})
	}
}

func idsN(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "id-" + strconv.Itoa(i)
	}
	return ids
}
