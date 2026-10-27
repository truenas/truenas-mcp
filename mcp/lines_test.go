package mcp

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func readAll(t *testing.T, l *LineReader) (lines []string, errs []error) {
	t.Helper()
	for {
		b, err := l.ReadLine()
		if err == io.EOF {
			return
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lines = append(lines, string(b))
	}
}

func TestLineReaderLargeLines(t *testing.T) {
	// Well above bufio.Scanner's 64 KiB default token limit and the reader's
	// own buffer.
	big := strings.Repeat("x", 5<<20)
	in := "first\n" + big + "\r\n" + "last"
	lines, errs := readAll(t, NewLineReader(strings.NewReader(in), MaxLineBytes))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(lines) != 3 || lines[0] != "first" || lines[1] != big || lines[2] != "last" {
		t.Fatalf("got %d lines (lengths %d, %d, %d)", len(lines), len(lines[0]), len(lines[1]), len(lines[2]))
	}
}

func TestLineReaderTerminators(t *testing.T) {
	lines, errs := readAll(t, NewLineReader(strings.NewReader("a\r\nb\n\nc\r\n"), 100))
	if len(errs) != 0 || strings.Join(lines, "|") != "a|b||c" {
		t.Fatalf("lines = %q, errs = %v", lines, errs)
	}
	lines, _ = readAll(t, NewLineReader(strings.NewReader(""), 100))
	if len(lines) != 0 {
		t.Fatalf("empty input gave %q", lines)
	}
}

func TestLineReaderOversizedLineIsSkipped(t *testing.T) {
	const max = 10
	tooLong := strings.Repeat("y", 200000)                                       // many buffer fills
	in := "ok1\n" + tooLong + "\nexactly10!\n" + tooLong + "\r\nok2\n" + tooLong // last one has no newline
	lines, errs := readAll(t, NewLineReader(strings.NewReader(in), max))
	if strings.Join(lines, "|") != "ok1|exactly10!|ok2" {
		t.Fatalf("lines = %q", lines)
	}
	if len(errs) != 3 {
		t.Fatalf("errs = %v; want 3 ErrLineTooLong", errs)
	}
	for _, err := range errs {
		if !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestLineReaderLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		in   string
		ok   bool
		want string
	}{
		{"1234567890\n", true, "1234567890"},
		{"1234567890\r\n", true, "1234567890"},
		{"1234567890", true, "1234567890"},
		{"12345678901\n", false, ""},
		{"12345678901\r\n", false, ""},
		{"12345678901", false, ""},
	} {
		b, err := NewLineReader(strings.NewReader(tc.in), 10).ReadLine()
		if tc.ok && (err != nil || string(b) != tc.want) {
			t.Errorf("%q: got %q, %v", tc.in, b, err)
		}
		if !tc.ok && !errors.Is(err, ErrLineTooLong) {
			t.Errorf("%q: got %q, %v; want ErrLineTooLong", tc.in, b, err)
		}
	}
}

func TestLineReaderReadError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(bytes.NewReader([]byte("par")), iotest.ErrReader(boom))
	if _, err := NewLineReader(r, 100).ReadLine(); !errors.Is(err, boom) {
		t.Fatalf("err = %v; want %v", err, boom)
	}
}

func TestLineReaderOneByteReads(t *testing.T) {
	lines, errs := readAll(t, NewLineReader(iotest.OneByteReader(strings.NewReader("ab\r\ncd\n")), 100))
	if len(errs) != 0 || strings.Join(lines, "|") != "ab|cd" {
		t.Fatalf("lines = %q, errs = %v", lines, errs)
	}
}
