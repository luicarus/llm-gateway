// Package usage parses token accounting out of OpenAI-compatible API traffic.
//
// The gateway sits between the client and the upstream provider, so it must be
// able to read usage from two very different shapes of response:
//
//   - Non-streaming: a single JSON body carrying a top-level "usage" object.
//   - Streaming (SSE): a sequence of `data: {...}` chunks. OpenAI-compatible
//     providers only emit the "usage" object in the final chunk, and only when
//     the request asked for it via stream_options.include_usage.
//
// Usage can also appear in a non-2xx error body, so parsing is deliberately
// tolerant: anything unparseable simply yields no usage rather than an error.
package usage

import (
	"encoding/json"
	"strings"
)

// Record is a single request's token accounting, normalised across providers.
type Record struct {
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`

	// CachedTokens is the prompt-prefix cache hit count (DeepSeek reports
	// prompt_cache_hit_tokens, OpenAI reports prompt_tokens_details.cached_tokens).
	CachedTokens int `json:"cached_tokens"`

	// ReasoningTokens is the hidden thinking-token count for reasoning models.
	ReasoningTokens int `json:"reasoning_tokens"`

	// Stream reports whether the response was an SSE stream.
	Stream bool `json:"stream"`
}

// Known reports whether any token counts were recovered.
func (r Record) Known() bool {
	return r.PromptTokens > 0 || r.CompletionTokens > 0 || r.TotalTokens > 0
}

// normalize fills in TotalTokens when the provider omitted it, and clamps
// nonsensical negative values that occasionally leak out of proxies.
func (r *Record) normalize() {
	if r.PromptTokens < 0 {
		r.PromptTokens = 0
	}
	if r.CompletionTokens < 0 {
		r.CompletionTokens = 0
	}
	if r.CachedTokens < 0 {
		r.CachedTokens = 0
	}
	if r.ReasoningTokens < 0 {
		r.ReasoningTokens = 0
	}
	// Only synthesise the total when it is genuinely absent, so we never
	// contradict a provider that reports an authoritative value.
	if r.TotalTokens == 0 && (r.PromptTokens > 0 || r.CompletionTokens > 0) {
		r.TotalTokens = r.PromptTokens + r.CompletionTokens
	}
}

// wireUsage mirrors the union of usage objects seen across OpenAI-compatible
// providers. Every field is a pointer so that "absent" stays distinguishable
// from "explicitly zero" while we merge partial chunks.
type wireUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`

	// DeepSeek-style flat cache counters.
	PromptCacheHitTokens  *int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens *int `json:"prompt_cache_miss_tokens"`

	// OpenAI-style nested detail objects.
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// apply merges the wire usage into a Record, preferring any value the provider
// actually supplied over whatever was accumulated earlier.
func (w *wireUsage) apply(rec *Record) {
	if w == nil {
		return
	}
	if w.PromptTokens != nil {
		rec.PromptTokens = *w.PromptTokens
	}
	if w.CompletionTokens != nil {
		rec.CompletionTokens = *w.CompletionTokens
	}
	if w.TotalTokens != nil {
		rec.TotalTokens = *w.TotalTokens
	}
	// Cache counters: OpenAI nests them, DeepSeek flattens them. Accept both.
	if w.PromptCacheHitTokens != nil {
		rec.CachedTokens = *w.PromptCacheHitTokens
	}
	if w.PromptTokensDetails != nil && w.PromptTokensDetails.CachedTokens != nil {
		rec.CachedTokens = *w.PromptTokensDetails.CachedTokens
	}
	if w.CompletionTokensDetails != nil && w.CompletionTokensDetails.ReasoningTokens != nil {
		rec.ReasoningTokens = *w.CompletionTokensDetails.ReasoningTokens
	}
}

// envelope is the subset of a chat-completion response we need, used for both
// full JSON bodies and individual SSE chunks.
type envelope struct {
	Model string     `json:"model"`
	Usage *wireUsage `json:"usage"`
}

// ParseBody recovers usage from a non-streaming response (or an error body).
func ParseBody(body []byte) (Record, bool) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Record{}, false
	}
	rec := Record{Model: env.Model}
	env.Usage.apply(&rec)
	rec.normalize()
	return rec, rec.Known()
}

// Accumulator incrementally consumes SSE bytes and retains the last usage
// object observed in the stream. It is safe for use from a single goroutine.
type Accumulator struct {
	rec      Record
	hasUsage bool
	// partial holds an incomplete trailing line between Write calls, because a
	// single SSE event can be split across arbitrary TCP segment boundaries.
	partial []byte
}

// maxLineBytes guards against a malformed stream growing the buffer without
// bound. A legitimate SSE data line for chat completions is far below this.
const maxLineBytes = 1 << 20 // 1 MiB

func (a *Accumulator) Write(p []byte) (int, error) {
	n := len(p)
	a.partial = append(a.partial, p...)

	for {
		idx := indexByte(a.partial, '\n')
		if idx < 0 {
			break
		}
		line := a.partial[:idx]
		a.partial = a.partial[idx+1:]
		a.consumeLine(line)
	}

	// If a single line exceeds the cap, it cannot be a valid usage event;
	// drop it rather than buffering unbounded memory.
	if len(a.partial) > maxLineBytes {
		a.partial = a.partial[:0]
	}
	return n, nil
}

func (a *Accumulator) consumeLine(line []byte) {
	line = trimSpace(line)
	if len(line) == 0 {
		return
	}
	const prefix = "data:"
	if !hasPrefixFold(line, prefix) {
		return // comment, "event:", "id:", etc.
	}
	payload := trimSpace(line[len(prefix):])
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return
	}

	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if env.Model != "" {
		a.rec.Model = env.Model
	}
	if env.Usage != nil {
		env.Usage.apply(&a.rec)
		a.hasUsage = true
	}
}

// Record returns the accumulated streaming usage. ok is false when the stream
// never carried a usage object, which is the normal outcome for providers that
// omit usage unless stream_options.include_usage was requested.
func (a *Accumulator) Record() (Record, bool) {
	// Drain any trailing bytes that arrived without a terminating newline.
	// A well-behaved stream ends each event with "\n\n", but a truncated or
	// abruptly closed connection can leave the final usage event unterminated,
	// and losing it would silently under-count the request.
	if len(a.partial) > 0 {
		a.consumeLine(a.partial)
		a.partial = nil
	}

	rec := a.rec
	rec.Stream = true
	rec.normalize()
	return rec, a.hasUsage
}

// small helpers kept local so the package has no dependencies beyond stdlib.

func indexByte(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

func hasPrefixFold(b []byte, prefix string) bool {
	if len(b) < len(prefix) {
		return false
	}
	return strings.EqualFold(string(b[:len(prefix)]), prefix)
}
