// Package stats provides a fixed-memory latency histogram.
//
// Storing every sample would cost hundreds of megabytes at a million
// operations per second, so samples go into logarithmic buckets, each 1%
// wider than the last. Percentiles are therefore accurate to within 1%,
// and memory stays at a few kilobytes no matter how long a run lasts.
package stats

import (
	"math"
	"time"
)

const growth = 1.01

var logGrowth = math.Log(growth)

// Histogram records durations. The zero value is ready to use. It is not
// safe for concurrent use: give each goroutine its own and Merge them.
type Histogram struct {
	counts []uint64
	total  uint64
	sum    time.Duration
	min    time.Duration
	max    time.Duration
}

func bucket(d time.Duration) int {
	if d < 1 {
		d = 1
	}
	return int(math.Log(float64(d)) / logGrowth)
}

// upperBound is the largest duration that falls into bucket i.
func upperBound(i int) time.Duration {
	return time.Duration(math.Pow(growth, float64(i+1)))
}

// Record adds one sample.
func (h *Histogram) Record(d time.Duration) {
	i := bucket(d)
	if i >= len(h.counts) {
		grown := make([]uint64, i+1)
		copy(grown, h.counts)
		h.counts = grown
	}
	h.counts[i]++
	if h.total == 0 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}
	h.total++
	h.sum += d
}

// Merge adds every sample from o into h.
func (h *Histogram) Merge(o *Histogram) {
	if o.total == 0 {
		return
	}
	if len(o.counts) > len(h.counts) {
		grown := make([]uint64, len(o.counts))
		copy(grown, h.counts)
		h.counts = grown
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	if h.total == 0 || o.min < h.min {
		h.min = o.min
	}
	if o.max > h.max {
		h.max = o.max
	}
	h.total += o.total
	h.sum += o.sum
}

// Count is the number of samples recorded.
func (h *Histogram) Count() uint64 { return h.total }

// Min and Max are exact.
func (h *Histogram) Min() time.Duration { return h.min }
func (h *Histogram) Max() time.Duration { return h.max }

// Mean is exact.
func (h *Histogram) Mean() time.Duration {
	if h.total == 0 {
		return 0
	}
	return h.sum / time.Duration(h.total)
}

// Percentile returns the duration at or below which p percent of samples
// fall (p in [0, 100]), accurate to within 1%. It returns 0 if empty.
func (h *Histogram) Percentile(p float64) time.Duration {
	if h.total == 0 {
		return 0
	}
	target := uint64(math.Ceil(p / 100 * float64(h.total)))
	if target < 1 {
		target = 1
	}
	var seen uint64
	for i, c := range h.counts {
		seen += c
		if seen >= target {
			return min(max(upperBound(i), h.min), h.max)
		}
	}
	return h.max
}
