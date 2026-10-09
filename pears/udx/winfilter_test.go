package udx

import (
	"cmp"
	"testing"
)

// Upstream values. Each case was run through libudx ae8bff7 (src/win_filter.c and
// src/win_filter_f64.c, built with clang from that source): win_filter_reset at time 0 with the
// start value, then one win_filter_apply_min or win_filter_apply_max per step at the step's time,
// reading win_filter_get after each. Times and the window are in milliseconds.

type winStep[T any] struct {
	t    uint64
	v    T
	want T // the best sample after the step
}

type winCase[T any] struct {
	name  string
	win   uint32
	start T
	steps []winStep[T]
}

// runWinCases drives each case through a fresh filter and checks the best after every step.
func runWinCases[T cmp.Ordered](t *testing.T, cases []winCase[T], apply func(f *winFilter[T], win uint32, now uint64, v T)) {
	t.Helper()
	for _, c := range cases {
		var f winFilter[T]
		f.reset(0, c.start)
		if got := f.get(); got != c.start {
			t.Fatalf("%s: get after reset = %v, want %v", c.name, got, c.start)
		}
		for i, s := range c.steps {
			apply(&f, c.win, s.t, s.v)
			if got := f.get(); got != s.want {
				t.Fatalf("%s step %d (t=%d v=%v): get = %v, want %v", c.name, i, s.t, s.v, got, s.want)
			}
		}
	}
}

// The minimum keeps the second and third best, so the best rises when the window passes it. Case
// D expires the whole window in one step, which starts over. Case E moves the two oldest samples out
// in one step (the second bump of win_filter_apply_common).
func TestWinFilterMinLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	cases := []winCase[uint32]{
		{"A", 100, 10, []winStep[uint32]{
			{20, 20, 10}, {30, 20, 10}, {60, 30, 10}, {101, 40, 20}, {140, 50, 30}, {170, 60, 40},
		}},
		{"B", 100, 10, []winStep[uint32]{
			{5, 15, 10}, {10, 8, 8}, {40, 9, 8}, {60, 7, 7}, {200, 12, 12}, {220, 11, 11}, {330, 30, 30},
		}},
		{"D", 100, 10, []winStep[uint32]{
			{30, 20, 10}, {60, 30, 10}, {250, 40, 40}, {260, 50, 40},
		}},
		{"E", 100, 10, []winStep[uint32]{
			{30, 20, 10}, {60, 30, 10}, {150, 40, 30},
		}},
	}
	runWinCases(t, cases, func(f *winFilter[uint32], win uint32, now uint64, v uint32) {
		f.applyMin(win, now, v)
	})
}

func TestWinFilterMaxLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	cases := []winCase[uint32]{
		{"A", 100, 10, []winStep[uint32]{
			{20, 5, 10}, {30, 5, 10}, {60, 3, 10}, {101, 2, 5}, {140, 1, 3}, {170, 0, 2},
		}},
		{"B", 100, 10, []winStep[uint32]{
			{5, 15, 15}, {10, 22, 22}, {40, 9, 22}, {60, 7, 22}, {200, 12, 12}, {220, 11, 12}, {330, 30, 30},
		}},
		{"D", 100, 10, []winStep[uint32]{
			{30, 5, 10}, {60, 3, 10}, {250, 4, 4}, {260, 2, 4},
		}},
		{"E", 100, 10, []winStep[uint32]{
			{30, 5, 10}, {60, 3, 10}, {150, 4, 4},
		}},
	}
	runWinCases(t, cases, func(f *winFilter[uint32], win uint32, now uint64, v uint32) {
		f.applyMax(win, now, v)
	})
}

func TestWinFilterF64MaxLikeUpstream(t *testing.T) {
	defer failOnPanic(t)
	cases := []winCase[float64]{
		{"C", 16, 1.0, []winStep[float64]{
			{1, 1.5, 1.5}, {2, 0.5, 1.5}, {3, 2.25, 2.25}, {9, 2, 2.25}, {10, 1, 2.25},
			{12, 0.75, 2.25}, {20, 0.25, 2}, {40, 0.1, 0.1},
		}},
	}
	runWinCases(t, cases, func(f *winFilter[float64], win uint32, now uint64, v float64) {
		f.applyMax(win, now, v)
	})
}
