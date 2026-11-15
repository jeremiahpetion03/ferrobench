package bench

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeremiahpetion03/ferrobench/internal/resp"
)

// fakeServer is a tiny in-memory GET/SET server. If dropEvery > 0, it
// silently discards every Nth SET while still replying OK, simulating a
// server that loses writes.
type fakeServer struct {
	ln        net.Listener
	mu        sync.Mutex
	data      map[string]string
	dropEvery int64
	sets      atomic.Int64
}

func startFake(t *testing.T, dropEvery int64) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, data: map[string]string{}, dropEvery: dropEvery}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeServer) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		v, err := resp.Read(r)
		if err != nil {
			return
		}
		args := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			args[i] = e.Str
		}
		switch strings.ToUpper(args[0]) {
		case "SET":
			n := s.sets.Add(1)
			if s.dropEvery == 0 || n%s.dropEvery != 0 {
				s.mu.Lock()
				s.data[args[1]] = args[2]
				s.mu.Unlock()
			}
			w.WriteString("+OK\r\n")
		case "GET":
			s.mu.Lock()
			val, ok := s.data[args[1]]
			s.mu.Unlock()
			if ok {
				w.Write(resp.AppendCommand(nil, val)[4:]) // reuse bulk encoding, drop "*1\r\n"
			} else {
				w.WriteString("$-1\r\n")
			}
		default:
			w.WriteString("-ERR unknown command\r\n")
		}
		// Flush once the pipelined batch is drained, like a real server.
		if r.Buffered() == 0 {
			if w.Flush() != nil {
				return
			}
		}
	}
}

func cfgFor(addr string) Config {
	return Config{
		Addr: addr, Clients: 4, Pipeline: 8, Duration: 300 * time.Millisecond,
		Keys: 50, ValueSize: 32, GetRatio: 0.5, Seed: 42,
	}
}

func TestCorrectServerHasNoMismatches(t *testing.T) {
	s := startFake(t, 0)
	res, err := Run(context.Background(), cfgFor(s.ln.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Ops == 0 || res.Hits == 0 {
		t.Fatalf("expected traffic and hits, got %+v", res)
	}
	if res.Mismatches != 0 || res.Errors != 0 {
		t.Fatalf("correct server produced %d mismatches, %d errors", res.Mismatches, res.Errors)
	}
	if res.Ops != res.Gets+res.Sets {
		t.Fatalf("ops %d != gets %d + sets %d", res.Ops, res.Gets, res.Sets)
	}
	if res.Latency.Count() != res.Ops/8 {
		t.Fatalf("expected one latency sample per 8-command round trip, got %d for %d ops",
			res.Latency.Count(), res.Ops)
	}
}

// The verifier must actually catch a server that drops writes.
func TestLostWritesAreDetected(t *testing.T) {
	s := startFake(t, 7)
	res, err := Run(context.Background(), cfgFor(s.ln.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Mismatches == 0 {
		t.Fatal("server dropped every 7th write but no mismatches were reported")
	}
}

func TestPipelineOfOne(t *testing.T) {
	s := startFake(t, 0)
	cfg := cfgFor(s.ln.Addr().String())
	cfg.Pipeline = 1
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mismatches != 0 || res.Latency.Count() != res.Ops {
		t.Fatalf("pipeline=1: %+v", res)
	}
}

func TestConnectFailure(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listening now
	cfg := cfgFor(addr)
	cfg.Timeout = time.Second
	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected a connection error")
	}
}

func TestInvalidConfig(t *testing.T) {
	bad := []func(*Config){
		func(c *Config) { c.Clients = 0 },
		func(c *Config) { c.Pipeline = 0 },
		func(c *Config) { c.Duration = 0 },
		func(c *Config) { c.Keys = 0 },
		func(c *Config) { c.ValueSize = 0 },
		func(c *Config) { c.GetRatio = 1.5 },
		func(c *Config) { c.Addr = "" },
	}
	for i, mutate := range bad {
		cfg := cfgFor("127.0.0.1:1")
		mutate(&cfg)
		if _, err := Run(context.Background(), cfg); err == nil {
			t.Errorf("case %d: expected a validation error", i)
		}
	}
}
