package main

import (
	"context"
	"io"
	"os"
)

// streamIo writes to the real process stdout/stderr.
type streamIo struct {
	out io.Writer
	err io.Writer
}

func (s streamIo) Out(text string) { _, _ = s.out.Write([]byte(text)) }
func (s streamIo) Err(text string) { _, _ = s.err.Write([]byte(text)) }

func main() {
	code := Run(context.Background(), os.Args[1:], os.Getenv, streamIo{out: os.Stdout, err: os.Stderr})
	os.Exit(code)
}
