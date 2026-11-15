// Package bench drives a Redis-compatible server with concurrent clients and
// verifies every reply it gets back.
//
// Each client owns a private set of keys (namespaced by a random run ID and
// the client number), so it knows exactly what every GET should return: the
// value it last wrote, or nothing if it has never written that key. A wrong
// value, a phantom value, or a lost write all count as mismatches. A fast
// server that returns wrong answers fails the run.
package bench

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/jeremiahpetion03/ferrobench/internal/resp"
	"github.com/jeremiahpetion03/ferrobench/internal/stats"
)

// Config describes one benchmark run.
type Config struct {
	Addr      string
	Clients   int           // concurrent connections, one goroutine each
	Pipeline  int           // commands sent per round trip
	Duration  time.Duration // how long to run
	Keys      int           // distinct keys per client
	ValueSize int           // bytes per value
	GetRatio  float64       // fraction of commands that are GETs, in [0, 1]
	Seed      int64         // for reproducible command sequences; 0 = random
	Timeout   time.Duration // per round trip; 0 = 5s
}

func (c Config) validate() error {
	switch {
	case c.Addr == "":
		return errors.New("address is required")
	case c.Clients < 1:
		return errors.New("clients must be at least 1")
	case c.Pipeline < 1:
		return errors.New("pipeline must be at least 1")
	case c.Duration <= 0:
		return errors.New("duration must be positive")
	case c.Keys < 1:
		return errors.New("keys must be at least 1")
	case c.ValueSize < 1:
		return errors.New("value size must be at least 1")
	case c.GetRatio < 0 || c.GetRatio > 1:
		return errors.New("get ratio must be between 0 and 1")
	}
	return nil
}

// Result is the outcome of a run, summed over all clients.
type Result struct {
	Ops        uint64 // replies received
	Gets       uint64
	Sets       uint64
	Hits       uint64 // GETs that correctly returned a value
	Mismatches uint64 // replies that contradict what this client wrote
	Errors     uint64 // error replies from the server
	Elapsed    time.Duration
	Latency    stats.Histogram // one sample per round trip
}

// OpsPerSec is overall throughput.
func (r *Result) OpsPerSec() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Ops) / r.Elapsed.Seconds()
}

// Run executes the benchmark. All connections are opened before the clock
// starts, so connection setup doesn't count against throughput.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Seed == 0 {
		cfg.Seed = time.Now().UnixNano()
	}

	conns := make([]net.Conn, 0, cfg.Clients)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < cfg.Clients; i++ {
		c, err := net.DialTimeout("tcp", cfg.Addr, cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("connect %d/%d: %w", i+1, cfg.Clients, err)
		}
		conns = append(conns, c)
	}

	runID := newRunID()
	// Keys expire on their own, so repeated runs don't fill the server.
	ttl := strconv.Itoa(int(max(300*time.Second, 2*cfg.Duration+time.Minute) / time.Second))

	ctx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	results := make([]*Result, cfg.Clients)
	errs := make([]error, cfg.Clients)
	var wg sync.WaitGroup
	start := time.Now()
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &worker{
				cfg:    cfg,
				conn:   c,
				prefix: fmt.Sprintf("fb:%s:%d:", runID, i),
				ttl:    ttl,
				rng:    mrand.New(mrand.NewSource(cfg.Seed + int64(i))),
				set:    make([]bool, cfg.Keys),
			}
			results[i], errs[i] = w.run(ctx)
		}()
	}
	wg.Wait()

	total := &Result{Elapsed: time.Since(start)}
	for i, r := range results {
		if errs[i] != nil {
			return nil, fmt.Errorf("client %d: %w", i, errs[i])
		}
		total.Ops += r.Ops
		total.Gets += r.Gets
		total.Sets += r.Sets
		total.Hits += r.Hits
		total.Mismatches += r.Mismatches
		total.Errors += r.Errors
		total.Latency.Merge(&r.Latency)
	}
	return total, nil
}

func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type op struct {
	get bool
	key int
}

type worker struct {
	cfg    Config
	conn   net.Conn
	prefix string
	ttl    string
	rng    *mrand.Rand
	set    []bool // set[k]: this client has successfully written key k
}

func (w *worker) keyName(k int) string { return w.prefix + strconv.Itoa(k) }

// valueFor derives a value from the key, so a GET can be checked without
// storing every value. It repeats the key until it fills ValueSize bytes.
func (w *worker) valueFor(key string) string {
	b := make([]byte, w.cfg.ValueSize)
	for i := range b {
		b[i] = key[i%len(key)]
	}
	return string(b)
}

func (w *worker) run(ctx context.Context) (*Result, error) {
	res := &Result{}
	r := bufio.NewReaderSize(w.conn, 64<<10)
	batch := make([]op, w.cfg.Pipeline)
	var buf []byte

	for ctx.Err() == nil {
		// Build one round trip's worth of commands.
		buf = buf[:0]
		for i := range batch {
			o := op{get: w.rng.Float64() < w.cfg.GetRatio, key: w.rng.Intn(w.cfg.Keys)}
			batch[i] = o
			key := w.keyName(o.key)
			if o.get {
				buf = resp.AppendCommand(buf, "GET", key)
			} else {
				buf = resp.AppendCommand(buf, "SET", key, w.valueFor(key), "EX", w.ttl)
			}
		}

		if err := w.conn.SetDeadline(time.Now().Add(w.cfg.Timeout)); err != nil {
			return nil, err
		}
		started := time.Now()
		if _, err := w.conn.Write(buf); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
		// Replies come back in command order, so a GET that follows a SET
		// of the same key in this batch is checked after that SET is applied.
		for _, o := range batch {
			v, err := resp.Read(r)
			if err != nil {
				return nil, fmt.Errorf("read reply: %w", err)
			}
			w.check(o, v, res)
		}
		res.Latency.Record(time.Since(started))
	}
	return res, nil
}

func (w *worker) check(o op, v resp.Value, res *Result) {
	res.Ops++
	if v.Kind == resp.Error {
		res.Errors++
		return
	}
	if !o.get {
		res.Sets++
		if v.Kind == resp.SimpleString && v.Str == "OK" {
			w.set[o.key] = true
		} else {
			res.Mismatches++
		}
		return
	}

	res.Gets++
	switch {
	case v.Kind != resp.BulkString:
		res.Mismatches++ // GET must return a bulk string or null
	case v.Null:
		if w.set[o.key] {
			res.Mismatches++ // lost write
		}
	case !w.set[o.key]:
		res.Mismatches++ // value for a key we never wrote
	case v.Str != w.valueFor(w.keyName(o.key)):
		res.Mismatches++ // wrong or corrupted value
	default:
		res.Hits++
	}
}
