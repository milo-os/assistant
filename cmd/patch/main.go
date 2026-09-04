package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
)

// streamIo writes to the real process stdout/stderr and reads interactive
// input line-by-line from stdin (the [LineReader] extension).
type streamIo struct {
	out io.Writer
	err io.Writer
	in  *bufio.Reader
}

func (s *streamIo) Out(text string) { _, _ = s.out.Write([]byte(text)) }
func (s *streamIo) Err(text string) { _, _ = s.err.Write([]byte(text)) }

func (s *streamIo) ReadLine() (string, bool) {
	line, err := s.in.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", false
	}
	return line, true
}

func main() {
	io := &streamIo{out: os.Stdout, err: os.Stderr, in: bufio.NewReader(os.Stdin)}
	code := Run(context.Background(), os.Args[1:], os.Getenv, io)
	os.Exit(code)
}
