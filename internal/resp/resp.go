// Package resp implements the client side of RESP2, the Redis wire protocol:
// encoding commands and decoding replies.
//
// The decoder reads from a bufio.Reader, so it naturally handles replies that
// arrive split across many TCP reads, and several pipelined replies that
// arrive in one read.
package resp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Kind identifies a RESP2 reply type by its leading byte.
type Kind byte

const (
	SimpleString Kind = '+'
	Error        Kind = '-'
	Integer      Kind = ':'
	BulkString   Kind = '$'
	Array        Kind = '*'
)

// Limits protect the client from a misbehaving server asking it to allocate
// huge buffers. They match Redis's own defaults.
const (
	MaxBulkLen  = 512 << 20 // 512 MiB
	MaxArrayLen = 1 << 20
)

// ErrProtocol is returned (wrapped) for any reply that is not valid RESP2.
var ErrProtocol = errors.New("resp: protocol error")

// Value is one decoded reply.
type Value struct {
	Kind  Kind
	Str   string  // SimpleString, Error, BulkString
	Int   int64   // Integer
	Elems []Value // Array
	Null  bool    // null bulk string ($-1) or null array (*-1)
}

// AppendCommand appends args encoded as a RESP array of bulk strings, the
// form every Redis command is sent in. Arguments are binary-safe.
func AppendCommand(dst []byte, args ...string) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(len(args)), 10)
	dst = append(dst, '\r', '\n')
	for _, a := range args {
		dst = append(dst, '$')
		dst = strconv.AppendInt(dst, int64(len(a)), 10)
		dst = append(dst, '\r', '\n')
		dst = append(dst, a...)
		dst = append(dst, '\r', '\n')
	}
	return dst
}

// Read decodes exactly one reply. It returns io.EOF only if the stream ended
// cleanly before the reply started, and io.ErrUnexpectedEOF if it ended
// partway through one.
func Read(r *bufio.Reader) (Value, error) {
	return read(r, 0)
}

// maxDepth bounds nesting so a hostile reply cannot exhaust the stack.
const maxDepth = 32

func read(r *bufio.Reader, depth int) (Value, error) {
	if depth > maxDepth {
		return Value{}, fmt.Errorf("%w: arrays nested deeper than %d", ErrProtocol, maxDepth)
	}
	line, err := readLine(r, depth > 0)
	if err != nil {
		return Value{}, err
	}
	if len(line) == 0 {
		return Value{}, fmt.Errorf("%w: empty line", ErrProtocol)
	}
	body := line[1:]

	switch Kind(line[0]) {
	case SimpleString:
		return Value{Kind: SimpleString, Str: body}, nil
	case Error:
		return Value{Kind: Error, Str: body}, nil
	case Integer:
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("%w: bad integer %q", ErrProtocol, body)
		}
		return Value{Kind: Integer, Int: n}, nil
	case BulkString:
		n, err := parseLen(body, MaxBulkLen)
		if err != nil {
			return Value{}, err
		}
		if n < 0 {
			return Value{Kind: BulkString, Null: true}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Value{}, unexpected(err)
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return Value{}, fmt.Errorf("%w: bulk string not terminated by CRLF", ErrProtocol)
		}
		return Value{Kind: BulkString, Str: string(buf[:n])}, nil
	case Array:
		n, err := parseLen(body, MaxArrayLen)
		if err != nil {
			return Value{}, err
		}
		if n < 0 {
			return Value{Kind: Array, Null: true}, nil
		}
		elems := make([]Value, n)
		for i := range elems {
			if elems[i], err = read(r, depth+1); err != nil {
				return Value{}, unexpected(err)
			}
		}
		return Value{Kind: Array, Elems: elems}, nil
	default:
		return Value{}, fmt.Errorf("%w: unknown type byte %q", ErrProtocol, line[0])
	}
}

// parseLen parses a bulk or array length: -1 means null, anything else must be
// in [0, max].
func parseLen(s string, max int) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%w: bad length %q", ErrProtocol, s)
	}
	if n == -1 {
		return -1, nil
	}
	if n < 0 || n > max {
		return 0, fmt.Errorf("%w: length %d out of range", ErrProtocol, n)
	}
	return n, nil
}

// readLine reads up to and including CRLF and returns the line without it.
// midReply reports whether we are already inside a reply, in which case any
// EOF is unexpected.
func readLine(r *bufio.Reader, midReply bool) (string, error) {
	b, err := r.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return "", fmt.Errorf("%w: line longer than %d bytes", ErrProtocol, r.Size())
	case errors.Is(err, io.EOF):
		if len(b) == 0 && !midReply {
			return "", io.EOF
		}
		return "", io.ErrUnexpectedEOF
	case err != nil:
		return "", err
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return "", fmt.Errorf("%w: line not terminated by CRLF", ErrProtocol)
	}
	// Copy out: ReadSlice's buffer is reused by the next read.
	return string(b[:len(b)-2]), nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
