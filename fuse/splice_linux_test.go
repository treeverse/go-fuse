package fuse

import "testing"

// trySplice reaches the zero-copy path only for a result satisfying
// statefulResult, so the type carrying the move flag must keep satisfying it:
// wrapping a ReadResult hides Stateful and falls back to copying.
func TestReadResultPipeSpliceMove(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    ReadResult
		want bool
	}{
		{"ReadResultPipe", ReadResultPipe(nil, 4096), false},
		{"ReadResultPipeMove", ReadResultPipeMove(nil, 4096), true},
	} {
		if _, ok := tc.r.(statefulResult); !ok {
			t.Errorf("%s: not a statefulResult; trySplice would copy instead of splice", tc.name)
		}
		m, ok := tc.r.(movableResult)
		if !ok {
			t.Fatalf("%s: not a movableResult", tc.name)
		}
		if got := m.SpliceMove(); got != tc.want {
			t.Errorf("%s: SpliceMove() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
