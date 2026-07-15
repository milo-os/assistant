package agentcore

import (
	"io"
	"sync"
)

// StreamPartKind discriminates the variants of [StreamPart]. The set is
// unified across every provider adapter and across the loop itself: an
// adapter emits the TextDelta, ToolCall, and StepFinish kinds; [Run]
// additionally emits ToolResult, Finish, and Error.
type StreamPartKind string

const (
	// StreamPartTextDelta is an incremental fragment of assistant text; see
	// StreamPart.Text.
	StreamPartTextDelta StreamPartKind = "text-delta"
	// StreamPartToolCall is a fully-assembled tool call the model requested;
	// see StreamPart.ToolCall.
	StreamPartToolCall StreamPartKind = "tool-call"
	// StreamPartToolResult is the result of executing a tool, emitted by the
	// loop after it runs the tool; see StreamPart.ToolResult.
	StreamPartToolResult StreamPartKind = "tool-result"
	// StreamPartStepFinish marks the end of one model step and carries that
	// step's usage and finish reason; see StreamPart.Usage and
	// StreamPart.FinishReason.
	StreamPartStepFinish StreamPartKind = "step-finish"
	// StreamPartFinish is the terminal part of a successful run; it carries
	// the overall finish reason and the aggregated usage across all steps;
	// see StreamPart.FinishReason and StreamPart.TotalUsage.
	StreamPartFinish StreamPartKind = "finish"
	// StreamPartError is the terminal part of a failed run; see
	// StreamPart.Err.
	StreamPartError StreamPartKind = "error"
)

// StreamPart is one event in a unified model/loop stream. Exactly one
// group of fields is meaningful, selected by Kind:
//
//	TextDelta   -> Text
//	ToolCall    -> ToolCall
//	ToolResult  -> ToolResult
//	StepFinish  -> Usage, FinishReason
//	Finish      -> FinishReason, TotalUsage
//	Error       -> Err
type StreamPart struct {
	Kind StreamPartKind

	Text         string
	ToolCall     *ToolCall
	ToolResult   *ToolResult
	Usage        Usage
	TotalUsage   Usage
	FinishReason FinishReason
	Err          error
}

// StreamReader is a forward-only reader over a sequence of [StreamPart]
// values. Recv returns [io.EOF] once the stream is exhausted. Callers MUST
// call Close to release resources (for HTTP-backed adapters this closes the
// underlying response body; for the loop it cancels the driving goroutine).
type StreamReader interface {
	// Recv returns the next part, or [io.EOF] when the stream is done. Any
	// other error is a transport-level failure surfaced by the reader.
	Recv() (StreamPart, error)
	// Close stops the stream and releases its resources. It is safe to call
	// more than once.
	Close() error
}

// SendFunc delivers one [StreamPart] to a stream's consumer. It returns
// false once the consumer has closed the stream, which a producer should
// treat as a signal to stop and release its resources.
type SendFunc func(StreamPart) bool

// StreamFunc adapts a push-style producer into a pull-style [StreamReader].
// It runs produce in a background goroutine, handing it a [SendFunc] to emit
// parts; when produce returns, the stream ends (Recv reports [io.EOF]).
//
// onClose, if non-nil, is invoked exactly once when the consumer calls
// Close — HTTP adapters use it to close the underlying response body, and
// the loop uses it to cancel its context. A producer that observes SendFunc
// returning false should stop promptly so the goroutine does not leak.
func StreamFunc(produce func(send SendFunc), onClose func()) StreamReader {
	ch := make(chan StreamPart)
	done := make(chan struct{})
	s := &funcStream{ch: ch, done: done, onClose: onClose}

	go func() {
		defer close(ch)
		send := func(p StreamPart) bool {
			select {
			case ch <- p:
				return true
			case <-done:
				return false
			}
		}
		produce(send)
	}()

	return s
}

type funcStream struct {
	ch      chan StreamPart
	done    chan struct{}
	onClose func()
	once    sync.Once
}

func (s *funcStream) Recv() (StreamPart, error) {
	part, ok := <-s.ch
	if !ok {
		return StreamPart{}, io.EOF
	}
	return part, nil
}

func (s *funcStream) Close() error {
	s.once.Do(func() {
		close(s.done)
		if s.onClose != nil {
			s.onClose()
		}
		// Drain any parts still in flight so a producer blocked on send can
		// observe the closed done channel and exit.
		go func() {
			for range s.ch {
			}
		}()
	})
	return nil
}
