// Ported from libudx src/win_filter.c and src/win_filter_f64.c, Kathleen Nichols' windowed min and
// max filter (the algorithm of lib/minmax.c). Copyright 2017, Google Inc. Use of the libudx source
// is governed by the following BSD-style license:
//
//	Redistribution and use in source and binary forms, with or without modification, are permitted
//	provided that the following conditions are met:
//
//	  - Redistributions of source code must retain the above copyright notice, this list of
//	    conditions and the following disclaimer.
//	  - Redistributions in binary form must reproduce the above copyright notice, this list of
//	    conditions and the following disclaimer in the documentation and/or other materials
//	    provided with the distribution.
//	  - Neither the name of Google Inc. nor the names of its contributors may be used to endorse or
//	    promote products derived from this software without specific prior written permission.
//
//	THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR
//	IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND
//	FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR
//	CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
//	DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
//	DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER
//	IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT
//	OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
//
// The libudx source is part of libudx, Apache License 2.0, Copyright (c) 2021 Holepunch Inc.
package udx

import "cmp"

// winEntry is one sample kept by a winFilter: its time in milliseconds and its value.
type winEntry[T cmp.Ordered] struct {
	t uint64
	v T
}

// winFilter keeps the best, second best and third best samples of a window. The zero value is not
// reset: call reset before the first apply. It is win_filter_t (uint32 samples) and win_filter_f64_t
// (float64 samples) in one generic type.
type winFilter[T cmp.Ordered] struct {
	entries [3]winEntry[T]
}

// reset starts the filter over with one sample at time t.
func (f *winFilter[T]) reset(t uint64, v T) {
	e := winEntry[T]{t: t, v: v}
	f.entries[2], f.entries[1], f.entries[0] = e, e, e
}

// get returns the best sample in the window.
func (f *winFilter[T]) get() T { return f.entries[0].v }

// applyMin records a sample for a window of win milliseconds, where the best is the smallest.
func (f *winFilter[T]) applyMin(win uint32, t uint64, v T) {
	// new minimum, or nothing in the window: start over
	if v <= f.entries[0].v || t-f.entries[2].t > uint64(win) {
		f.reset(t, v)
		return
	}
	e := winEntry[T]{t: t, v: v}
	if v <= f.entries[1].v {
		// smaller than the second best
		f.entries[2], f.entries[1] = e, e
	} else if v <= f.entries[2].v {
		// smaller than the third best
		f.entries[2] = e
	}
	f.applyCommon(win, t, v)
}

// applyMax records a sample for a window of win milliseconds, where the best is the largest.
func (f *winFilter[T]) applyMax(win uint32, t uint64, v T) {
	// new maximum, or nothing in the window: start over
	if v >= f.entries[0].v || t-f.entries[2].t > uint64(win) {
		f.reset(t, v)
		return
	}
	e := winEntry[T]{t: t, v: v}
	if v >= f.entries[1].v {
		// bigger than the second best
		f.entries[2], f.entries[1] = e, e
	} else if v >= f.entries[2].v {
		// bigger than the third best
		f.entries[2] = e
	}
	f.applyCommon(win, t, v)
}

// applyCommon is win_filter_apply_common: it moves the best samples along once they leave the
// window. The age is 32 bits, as in libudx.
func (f *winFilter[T]) applyCommon(win uint32, t uint64, v T) {
	dt := uint32(t - f.entries[0].t)
	e := winEntry[T]{t: t, v: v}
	if dt > win {
		// we've passed the window so bump off entries[0]
		f.entries[0], f.entries[1], f.entries[2] = f.entries[1], f.entries[2], e
		if t-f.entries[0].t > uint64(win) {
			// bump off entries[1] too
			f.entries[0], f.entries[1], f.entries[2] = f.entries[1], f.entries[2], e
		}
	} else if f.entries[1].t == f.entries[0].t && dt > win/4 {
		f.entries[2], f.entries[1] = e, e
	} else if f.entries[2].t == f.entries[1].t && dt > win/2 {
		f.entries[2] = e
	}
}
