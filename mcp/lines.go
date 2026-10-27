package mcp

import (
	"bufio"
	"errors"
	"io"
)

// MaxLineBytes is the longest newline-delimited JSON-RPC message the stdio
// transport accepts. Tool results are not limited by this: it only bounds
// what a client may send.
const MaxLineBytes = 64 << 20

// ErrLineTooLong is returned by LineReader.ReadLine for a line longer than
// the limit. The rest of that line has been discarded, so the next call
// returns the following message.
var ErrLineTooLong = errors.New("line too long")

// LineReader reads newline-delimited messages of any size up to a limit.
// Unlike bufio.Scanner, whose default token limit is 64 KiB, it handles
// large requests, and an oversized line does not end the stream.
type LineReader struct {
	r   *bufio.Reader
	max int
	// buf is the line storage, reused between calls so a run of ordinary
	// lines does not allocate. It is dropped after a line longer than
	// maxRetainedBytes, so one large request does not pin up to max bytes.
	buf []byte
}

// maxRetainedBytes is the largest line buffer a LineReader keeps between
// calls.
const maxRetainedBytes = 1 << 20

// NewLineReader returns a LineReader over r that accepts lines of up to max
// bytes (excluding the line terminator).
func NewLineReader(r io.Reader, max int) *LineReader {
	return &LineReader{r: bufio.NewReaderSize(r, 64*1024), max: max}
}

// ReadLine returns the next line without its "\n" or "\r\n". The returned
// slice is only valid until the next call. It returns io.EOF when the input
// ends; a final line without a terminator is returned first.
func (l *LineReader) ReadLine() ([]byte, error) {
	line := l.buf[:0]
	tooLong := false
	for {
		chunk, err := l.r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > l.max+2 { // room for "\r\n"
				tooLong = true
				line = line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		if err == io.EOF && len(chunk) == 0 && len(line) == 0 && !tooLong {
			return nil, io.EOF
		}
		if tooLong {
			return nil, ErrLineTooLong
		}
		if cap(line) > maxRetainedBytes {
			l.buf = nil
		} else {
			l.buf = line
		}
		line = trimEOL(line)
		if len(line) > l.max {
			return nil, ErrLineTooLong
		}
		return line, nil
	}
}

func trimEOL(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}
