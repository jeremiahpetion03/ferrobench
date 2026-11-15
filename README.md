# ferrobench

[![CI](https://github.com/jeremiahpetion03/ferrobench/actions/workflows/ci.yml/badge.svg)](https://github.com/jeremiahpetion03/ferrobench/actions/workflows/ci.yml)

A load tester for Redis-compatible servers, written in Go with **zero dependencies**. It measures throughput and latency, and it **verifies every reply**: a server that is fast but returns wrong answers fails the run.

I built it to benchmark [ferrodb](https://github.com/jeremiahpetion03/ferrodb), my Rust Redis clone, against real Redis.

```
$ ferrobench -addr 127.0.0.1:6391 -clients 50 -pipeline 16 -duration 5s -seed 7
ferrobench → 127.0.0.1:6391  clients=50 pipeline=16 duration=5s keys/client=10000 size=64B get-ratio=0.50

  throughput   253,728 ops/sec  (1,271,520 ops in 5.01s)
  mix          636,593 GET (276,130 hits)  /  634,927 SET
  round trip   p50 1.11ms   p99 30.52ms   p99.9 70.41ms   max 130.81ms
  verified     every reply matched  ✓
```

## Why verify replies?

Most load testers only count replies. ferrobench checks them. Each client writes to its own private keys (namespaced by a random run ID), so it always knows what a `GET` should return:

| Server returns | Client expected | Verdict |
|---|---|---|
| the value it last wrote | that value | hit |
| nothing | a key it never wrote | correct miss |
| nothing | a key it **did** write | **mismatch: lost write** |
| a value | a key it never wrote | **mismatch: phantom value** |
| a different value | a specific value | **mismatch: corruption** |

Values are derived from their key, so checking costs no extra memory. The test suite proves the checker works by running against a fake server that silently drops every 7th write and asserting the run fails.

The exit code is `0` if every reply matched, `1` on any mismatch or error reply, and `2` if the run couldn't happen (for example, connection refused). That makes it usable in CI as a correctness gate.

## Results

Redis vs ferrodb on my laptop: 4 cores, Ubuntu on WSL2, benchmark and server running on the same machine. 50 clients, 64-byte values, 50/50 GET/SET, 5-second runs, `-seed 7`.

| Server | Pipeline | Throughput | p50 | p99 | Verified |
|---|---|---|---|---|---|
| Redis | 1 | 33,556 ops/sec | 1.21ms | 5.85ms | ✓ |
| ferrodb | 1 | 47,660 ops/sec | 215µs | 10.84ms | ✓ |
| Redis | 16 | 176,352 ops/sec | 4.26ms | 12.72ms | ✓ |
| ferrodb | 16 | 253,728 ops/sec | 1.11ms | 30.52ms | ✓ |

**How to read this.** Absolute numbers are modest because the benchmark and the server share 4 cores inside WSL2's virtualization layer. The comparison is the interesting part. ferrodb delivered about 1.4× Redis's throughput in both modes, with a much lower median, because its 64 lock-sharded partitions let clients work in parallel. Its tail latency is worse, though: p99 is roughly 2× Redis's. With 50 OS threads competing for 4 cores, some requests wait for a CPU time slice, while Redis runs commands on a single thread and keeps latency more even. That throughput-versus-tail trade-off is what I expected from the two designs.

## Usage

```bash
go build -o ferrobench .
./ferrobench -addr 127.0.0.1:6379 -clients 50 -pipeline 16 -duration 10s
```

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `127.0.0.1:6379` | server address |
| `-clients` | `50` | concurrent connections, one goroutine each |
| `-pipeline` | `1` | commands sent per round trip |
| `-duration` | `10s` | how long to run |
| `-keys` | `10000` | distinct keys per client |
| `-size` | `64` | value size in bytes |
| `-get-ratio` | `0.5` | fraction of commands that are GETs |
| `-seed` | random | fixed seed for a reproducible command mix |

Keys are written with an expiry (at least 5 minutes), so repeated runs don't fill up the server.

## How it works

```
main.go              flags, output, exit codes
internal/resp        RESP2 encoder + incremental decoder
internal/bench       connections, worker goroutines, reply verification
internal/stats       fixed-memory latency histogram
```

- **One goroutine per connection.** All connections are opened before the clock starts, so connection setup isn't counted. Workers stop together through a `context` deadline. Ctrl-C stops the run cleanly and still prints results.
- **Pipelining.** Each round trip writes `-pipeline` commands in one `Write`, then reads that many replies in order. Latency is recorded per round trip, which is what a pipelining client actually waits for.
- **RESP decoder.** Reads from a `bufio.Reader`, so replies split across TCP reads and several replies in one read both just work. It rejects malformed input with a wrapped `ErrProtocol`, enforces Redis's size limits (512 MiB bulk strings, 1M array elements), and caps array nesting so a hostile server can't exhaust the stack.
- **Latency histogram.** Storing every sample at ~1M ops/sec would cost hundreds of megabytes. Samples go into logarithmic buckets each 1% wider than the last, so percentiles are accurate to within 1% using a few kilobytes. Each worker has its own histogram (no locks on the hot path), and they're merged at the end.

## Tests

```bash
go test -race ./...
```

- **RESP:** every reply type, null and empty values, binary-safe strings, pipelined replies, a reply fed **one byte at a time**, 12 kinds of malformed or truncated input, and an encode/decode round trip.
- **Histogram:** percentiles cross-checked against an exact sort of 50,000 random samples (within 1%), and merging proven identical to recording everything in one histogram.
- **Benchmark:** full runs against an in-process fake server. A correct server must give zero mismatches, and a server that drops every 7th write must be caught.

CI runs `gofmt`, `go vet`, and the tests under the race detector, then benchmarks a real Redis server and fails if any reply doesn't verify.

## Notes

Built with AI assistance (Claude). I've reviewed the code, can explain how every part works, and ran the benchmarks above on my own machine.

## License

MIT
