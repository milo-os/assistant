// Package agentcore is a small, provider-neutral toolkit for driving a
// streaming language model through a tool-use loop.
//
// The package is deliberately self-contained: it reads no environment,
// performs no I/O of its own, and imports nothing outside the standard
// library and its own sub-packages. A concrete [Model] (see the anthropic,
// openaicompat, and mockmodel sub-packages), a set of executable tools
// (see the mcptool sub-package or any type implementing [Tool]), and a
// context are injected by the caller. This makes agentcore a candidate for
// extraction as a standalone "Go AI SDK core".
//
// The two entry points are:
//
//   - [Model], the interface every provider adapter implements, and
//   - [Run], which drives a [Model] through the tool-use loop and streams
//     back a unified sequence of [StreamPart] values.
package agentcore

// Usage is a token-accounting record. It is used both for a single model
// step and, once aggregated, for the whole run (the loop sums per-step
// usage into a running total; see [Run]).
//
// Field semantics are normalized across providers so that callers never
// have to know which model produced the numbers:
//
//   - Input is the total number of prompt tokens billed as input,
//     INCLUSIVE of any tokens served from the prompt cache (CacheRead) and
//     EXCLUSIVE of tokens written to the cache (CacheWrite). This matches
//     the convention that a cache read still counts toward input while a
//     cache write is billed on its own axis.
//   - Output is the number of generated (completion) tokens.
//   - CacheRead is the subset of prompt tokens served from the cache.
//   - CacheWrite is the number of tokens written to the cache on this
//     request (Anthropic "cache creation"); providers without a cache-write
//     concept leave it zero.
//
// Every field is billing-critical: adapters MUST populate CacheRead and
// CacheWrite whenever the provider reports them, and the loop MUST NOT drop
// them when aggregating.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Add returns the element-wise sum of u and other. It is used by the loop
// to aggregate per-step usage into a run total without mutating either
// operand.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		Input:      u.Input + other.Input,
		Output:     u.Output + other.Output,
		CacheRead:  u.CacheRead + other.CacheRead,
		CacheWrite: u.CacheWrite + other.CacheWrite,
	}
}
