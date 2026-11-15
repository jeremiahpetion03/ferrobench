// Command ferrobench load-tests a Redis-compatible server and verifies every
// reply. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/jeremiahpetion03/ferrobench/internal/bench"
)

func main() {
	var cfg bench.Config
	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:6379", "server address")
	flag.IntVar(&cfg.Clients, "clients", 50, "concurrent connections")
	flag.IntVar(&cfg.Pipeline, "pipeline", 1, "commands per round trip")
	flag.DurationVar(&cfg.Duration, "duration", 10*time.Second, "how long to run")
	flag.IntVar(&cfg.Keys, "keys", 10_000, "distinct keys per client")
	flag.IntVar(&cfg.ValueSize, "size", 64, "value size in bytes")
	flag.Float64Var(&cfg.GetRatio, "get-ratio", 0.5, "fraction of commands that are GETs (0-1)")
	flag.Int64Var(&cfg.Seed, "seed", 0, "random seed for a reproducible command mix (0 = random)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("ferrobench → %s  clients=%d pipeline=%d duration=%s keys/client=%d size=%dB get-ratio=%.2f\n",
		cfg.Addr, cfg.Clients, cfg.Pipeline, cfg.Duration, cfg.Keys, cfg.ValueSize, cfg.GetRatio)

	res, err := bench.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ferrobench:", err)
		os.Exit(2)
	}

	l := &res.Latency
	fmt.Printf("\n  throughput   %s ops/sec  (%s ops in %.2fs)\n",
		commas(uint64(res.OpsPerSec())), commas(res.Ops), res.Elapsed.Seconds())
	fmt.Printf("  mix          %s GET (%s hits)  /  %s SET\n", commas(res.Gets), commas(res.Hits), commas(res.Sets))
	fmt.Printf("  round trip   p50 %s   p99 %s   p99.9 %s   max %s\n",
		dur(l.Percentile(50)), dur(l.Percentile(99)), dur(l.Percentile(99.9)), dur(l.Max()))

	if res.Mismatches == 0 && res.Errors == 0 {
		fmt.Println("  verified     every reply matched  ✓")
		return
	}
	fmt.Printf("  verified     FAILED: %d mismatched replies, %d error replies\n", res.Mismatches, res.Errors)
	os.Exit(1)
}

func dur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func commas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
