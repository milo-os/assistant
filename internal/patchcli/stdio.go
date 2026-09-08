// The process-stream implementation of [Io], shared by both binaries that
// embed this package. Kept here rather than in each main so the two agree on
// the stdout/stderr split that makes `patch chat … > answer.txt` work.
package patchcli

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// stdio writes to the real process stdout/stderr and reads interactive input
// line-by-line from stdin (the [LineReader] extension).
type stdio struct {
	out io.Writer
	err io.Writer
	in  *bufio.Reader
}

// StdIO returns the [Io] the real CLIs use: the assistant's answer on stdout,
// status and decoration on stderr, and — for interactive chat — one line of
// input at a time from stdin.
func StdIO() Io {
	return &stdio{out: os.Stdout, err: os.Stderr, in: bufio.NewReader(os.Stdin)}
}

func (s *stdio) Out(text string) { _, _ = s.out.Write([]byte(text)) }
func (s *stdio) Err(text string) { _, _ = s.err.Write([]byte(text)) }

func (s *stdio) ReadLine() (string, bool) {
	line, err := s.in.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", false
	}
	return line, true
}
