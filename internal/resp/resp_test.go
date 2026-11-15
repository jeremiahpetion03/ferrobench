package resp

import (
	"bufio"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestAppendCommand(t *testing.T) {
	got := string(AppendCommand(nil, "SET", "k", "hello"))
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$5\r\nhello\r\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := string(AppendCommand(nil, "")); got != "*1\r\n$0\r\n\r\n" {
		t.Fatalf("empty argument: got %q", got)
	}
}

func TestReadValid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Value
	}{
		{"simple string", "+OK\r\n", Value{Kind: SimpleString, Str: "OK"}},
		{"error", "-ERR unknown command\r\n", Value{Kind: Error, Str: "ERR unknown command"}},
		{"integer", ":1000\r\n", Value{Kind: Integer, Int: 1000}},
		{"negative integer", ":-42\r\n", Value{Kind: Integer, Int: -42}},
		{"bulk string", "$5\r\nhello\r\n", Value{Kind: BulkString, Str: "hello"}},
		{"empty bulk string", "$0\r\n\r\n", Value{Kind: BulkString, Str: ""}},
		{"binary-safe bulk string", "$4\r\na\r\nb\r\n", Value{Kind: BulkString, Str: "a\r\nb"}},
		{"null bulk string", "$-1\r\n", Value{Kind: BulkString, Null: true}},
		{"null array", "*-1\r\n", Value{Kind: Array, Null: true}},
		{"empty array", "*0\r\n", Value{Kind: Array, Elems: []Value{}}},
		{
			"nested array",
			"*2\r\n$3\r\nfoo\r\n*2\r\n:1\r\n$-1\r\n",
			Value{Kind: Array, Elems: []Value{
				{Kind: BulkString, Str: "foo"},
				{Kind: Array, Elems: []Value{{Kind: Integer, Int: 1}, {Kind: BulkString, Null: true}}},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Read(bufio.NewReader(strings.NewReader(tc.in)))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A reply split into one-byte reads must decode exactly like a whole one.
// This is what a slow network looks like to the parser.
func TestReadOneByteAtATime(t *testing.T) {
	in := "*3\r\n$3\r\nfoo\r\n:7\r\n$-1\r\n"
	whole, err := Read(bufio.NewReader(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	split, err := Read(bufio.NewReader(iotest.OneByteReader(strings.NewReader(in))))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(whole, split) {
		t.Fatalf("split read %+v differs from whole read %+v", split, whole)
	}
}

// Pipelined replies arrive back to back in one buffer.
func TestReadPipelined(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("+OK\r\n$2\r\nhi\r\n:3\r\n"))
	want := []Value{
		{Kind: SimpleString, Str: "OK"},
		{Kind: BulkString, Str: "hi"},
		{Kind: Integer, Int: 3},
	}
	for i, w := range want {
		got, err := Read(r)
		if err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("reply %d: got %+v, want %+v", i, got, w)
		}
	}
	if _, err := Read(r); err != io.EOF {
		t.Fatalf("after last reply: got %v, want io.EOF", err)
	}
}

func TestReadMalformed(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"unknown type byte", "?what\r\n", ErrProtocol},
		{"missing CR", "+OK\n", ErrProtocol},
		{"bad integer", ":12a\r\n", ErrProtocol},
		{"bad bulk length", "$x\r\n", ErrProtocol},
		{"negative bulk length", "$-2\r\n", ErrProtocol},
		{"bulk length too large", "$999999999999\r\n", ErrProtocol},
		{"bulk missing terminator", "$3\r\nabcXY", ErrProtocol},
		{"empty line", "\r\n", ErrProtocol},
		{"truncated bulk", "$5\r\nab", io.ErrUnexpectedEOF},
		{"truncated line", "+OK", io.ErrUnexpectedEOF},
		{"truncated array", "*2\r\n:1\r\n", io.ErrUnexpectedEOF},
		{"too deeply nested", strings.Repeat("*1\r\n", 40) + ":1\r\n", ErrProtocol},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(bufio.NewReader(strings.NewReader(tc.in)))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got error %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadEmptyStream(t *testing.T) {
	if _, err := Read(bufio.NewReader(strings.NewReader(""))); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

func TestReadLineTooLong(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("+"+strings.Repeat("a", 100)+"\r\n"), 16)
	if _, err := Read(r); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v, want ErrProtocol", err)
	}
}

// Every command we can encode must decode back to the same arguments.
func TestRoundTrip(t *testing.T) {
	args := []string{"SET", "key with spaces", "", "bin\x00ary\r\n", strings.Repeat("x", 10_000)}
	r := bufio.NewReader(strings.NewReader(string(AppendCommand(nil, args...))))
	v, err := Read(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Elems) != len(args) {
		t.Fatalf("got %d elements, want %d", len(v.Elems), len(args))
	}
	for i, e := range v.Elems {
		if e.Kind != BulkString || e.Str != args[i] {
			t.Fatalf("element %d: got %+v, want %q", i, e, args[i])
		}
	}
}
