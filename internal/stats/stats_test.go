package stats

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

func within1Percent(t *testing.T, name string, got, want time.Duration) {
	t.Helper()
	diff := float64(got-want) / float64(want)
	if diff < -0.01 || diff > 0.01 {
		t.Errorf("%s: got %v, want %v (off by %.2f%%)", name, got, want, diff*100)
	}
}

func TestEmpty(t *testing.T) {
	var h Histogram
	if h.Percentile(50) != 0 || h.Mean() != 0 || h.Count() != 0 {
		t.Fatal("empty histogram should report zeros")
	}
}

func TestUniform(t *testing.T) {
	var h Histogram
	for i := 1; i <= 1000; i++ {
		h.Record(time.Duration(i) * time.Microsecond)
	}
	within1Percent(t, "p50", h.Percentile(50), 500*time.Microsecond)
	within1Percent(t, "p99", h.Percentile(99), 990*time.Microsecond)
	if h.Percentile(100) != time.Millisecond {
		t.Errorf("p100 should equal the exact max, got %v", h.Percentile(100))
	}
	if h.Min() != time.Microsecond || h.Max() != time.Millisecond {
		t.Errorf("min/max: got %v/%v", h.Min(), h.Max())
	}
	if h.Mean() != 500500*time.Nanosecond {
		t.Errorf("mean: got %v", h.Mean())
	}
}

func TestSingleSample(t *testing.T) {
	var h Histogram
	h.Record(42 * time.Millisecond)
	for _, p := range []float64{0, 50, 99.9, 100} {
		if got := h.Percentile(p); got != 42*time.Millisecond {
			t.Errorf("p%v: got %v, want exactly 42ms", p, got)
		}
	}
}

// Cross-check percentiles against an exact sort of the same random samples.
func TestAgainstExactSort(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var h Histogram
	samples := make([]time.Duration, 50_000)
	for i := range samples {
		// Log-normal-ish spread from ~10µs to ~100ms, like real latencies.
		samples[i] = time.Duration(10_000 * (1 + rng.ExpFloat64()*rng.ExpFloat64()*50))
		h.Record(samples[i])
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	for _, p := range []float64{50, 90, 99, 99.9} {
		exact := samples[int(p/100*float64(len(samples)))-1]
		within1Percent(t, "percentile", h.Percentile(p), exact)
	}
}

func TestMergeEqualsCombined(t *testing.T) {
	var a, b, all Histogram
	for i := 1; i <= 500; i++ {
		d := time.Duration(i*i) * time.Microsecond
		if i%2 == 0 {
			a.Record(d)
		} else {
			b.Record(d)
		}
		all.Record(d)
	}
	a.Merge(&b)
	if a.Count() != all.Count() || a.Mean() != all.Mean() || a.Min() != all.Min() || a.Max() != all.Max() {
		t.Fatal("merged summary differs from recording everything in one histogram")
	}
	for _, p := range []float64{1, 50, 99, 100} {
		if a.Percentile(p) != all.Percentile(p) {
			t.Fatalf("p%v differs after merge", p)
		}
	}
}
