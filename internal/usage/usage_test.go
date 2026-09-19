package usage

import "testing"

func TestParseBodyOpenAI(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-1","model":"gpt-4o-mini",
		"choices":[{"message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,
		         "prompt_tokens_details":{"cached_tokens":4},
		         "completion_tokens_details":{"reasoning_tokens":2}}
	}`)

	rec, ok := ParseBody(body)
	if !ok {
		t.Fatal("expected usage to be parsed")
	}
	if rec.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", rec.Model)
	}
	if rec.PromptTokens != 12 || rec.CompletionTokens != 7 || rec.TotalTokens != 19 {
		t.Errorf("tokens = %d/%d/%d, want 12/7/19", rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens)
	}
	if rec.CachedTokens != 4 {
		t.Errorf("cached = %d, want 4 (from prompt_tokens_details)", rec.CachedTokens)
	}
	if rec.ReasoningTokens != 2 {
		t.Errorf("reasoning = %d, want 2", rec.ReasoningTokens)
	}
	if rec.Stream {
		t.Error("Stream should be false for a buffered body")
	}
}

// DeepSeek flattens its cache counters and omits prompt_tokens_details.
func TestParseBodyDeepSeekCache(t *testing.T) {
	body := []byte(`{"model":"deepseek-chat",
		"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,
		         "prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":36}}`)

	rec, ok := ParseBody(body)
	if !ok {
		t.Fatal("expected usage to be parsed")
	}
	if rec.CachedTokens != 64 {
		t.Errorf("cached = %d, want 64 (from prompt_cache_hit_tokens)", rec.CachedTokens)
	}
}

// A provider that omits total_tokens must still produce a usable total.
func TestParseBodyDerivesTotal(t *testing.T) {
	body := []byte(`{"model":"m","usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	rec, ok := ParseBody(body)
	if !ok {
		t.Fatal("expected usage to be parsed")
	}
	if rec.TotalTokens != 15 {
		t.Errorf("total = %d, want 15 (derived)", rec.TotalTokens)
	}
}

func TestParseBodyNoUsage(t *testing.T) {
	if _, ok := ParseBody([]byte(`{"model":"m","choices":[]}`)); ok {
		t.Error("expected ok=false when no usage object is present")
	}
	if _, ok := ParseBody([]byte(`not json at all`)); ok {
		t.Error("expected ok=false for a non-JSON body")
	}
	if _, ok := ParseBody(nil); ok {
		t.Error("expected ok=false for an empty body")
	}
}

func TestStreamAccumulator(t *testing.T) {
	stream := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
		"data: {\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"

	var acc Accumulator
	if _, err := acc.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rec, ok := acc.Record()
	if !ok {
		t.Fatal("expected usage to be found in the stream")
	}
	if rec.PromptTokens != 9 || rec.CompletionTokens != 3 || rec.TotalTokens != 12 {
		t.Errorf("tokens = %d/%d/%d, want 9/3/12", rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens)
	}
	if rec.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", rec.Model)
	}
	if !rec.Stream {
		t.Error("Stream should be true for an SSE stream")
	}
}

// This is the highest-risk case: SSE events split at arbitrary byte offsets.
// Every possible split point must produce identical results to a single write.
func TestStreamAccumulatorSplitAtEveryByte(t *testing.T) {
	stream := "data: {\"model\":\"m\",\"choices\":[]}\n\n" +
		"data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":11,\"total_tokens\":32}}\n\n" +
		"data: [DONE]\n\n"

	for split := 0; split <= len(stream); split++ {
		var acc Accumulator
		if _, err := acc.Write([]byte(stream[:split])); err != nil {
			t.Fatalf("split %d: Write(part1): %v", split, err)
		}
		if _, err := acc.Write([]byte(stream[split:])); err != nil {
			t.Fatalf("split %d: Write(part2): %v", split, err)
		}
		rec, ok := acc.Record()
		if !ok {
			t.Fatalf("split %d: usage not found", split)
		}
		if rec.PromptTokens != 21 || rec.CompletionTokens != 11 || rec.TotalTokens != 32 {
			t.Fatalf("split %d: tokens = %d/%d/%d, want 21/11/32",
				split, rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens)
		}
	}
}

// Byte-at-a-time delivery is the worst realistic case (one TCP segment per byte).
func TestStreamAccumulatorByteAtATime(t *testing.T) {
	stream := "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6,\"total_tokens\":11}}\n\n"

	var acc Accumulator
	for i := 0; i < len(stream); i++ {
		if _, err := acc.Write([]byte{stream[i]}); err != nil {
			t.Fatalf("byte %d: %v", i, err)
		}
	}
	rec, ok := acc.Record()
	if !ok || rec.TotalTokens != 11 {
		t.Fatalf("tokens = %+v ok=%v, want total 11", rec, ok)
	}
}

// Streams without usage (the default when include_usage is not set) must be
// reported as "no usage" rather than as zero-token success.
func TestStreamAccumulatorWithoutUsage(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"

	var acc Accumulator
	if _, err := acc.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := acc.Record(); ok {
		t.Error("expected ok=false when the stream carries no usage object")
	}
}

// SSE comment/heartbeat lines and non-data fields must be ignored safely.
func TestStreamAccumulatorIgnoresNonDataLines(t *testing.T) {
	stream := ": keepalive\n\nevent: ping\nid: 42\nretry: 100\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"

	var acc Accumulator
	if _, err := acc.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rec, ok := acc.Record()
	if !ok {
		t.Fatal("expected usage to still be found")
	}
	if rec.TotalTokens != 2 {
		t.Errorf("total = %d, want 2", rec.TotalTokens)
	}
}

// A later usage object must supersede an earlier partial one, which is how
// providers that emit incremental usage in every chunk are handled.
func TestStreamAccumulatorLastUsageWins(t *testing.T) {
	stream := "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":0,\"total_tokens\":3}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":42,\"total_tokens\":45}}\n\n"

	var acc Accumulator
	if _, err := acc.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rec, _ := acc.Record()
	if rec.CompletionTokens != 42 || rec.TotalTokens != 45 {
		t.Errorf("tokens = %d/%d, want 42/45 (last usage wins)", rec.CompletionTokens, rec.TotalTokens)
	}
}

// A trailing line without a newline must not be lost when the stream ends.
func TestStreamAccumulatorUnterminatedFinalLine(t *testing.T) {
	stream := "data: {\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":9}}"

	var acc Accumulator
	if _, err := acc.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rec, ok := acc.Record()
	if !ok {
		t.Fatal("expected usage from an unterminated data line")
	}
	if rec.PromptTokens != 8 || rec.CompletionTokens != 9 {
		t.Errorf("tokens = %d/%d, want 8/9", rec.PromptTokens, rec.CompletionTokens)
	}
}

func TestNegativeValuesClamped(t *testing.T) {
	body := []byte(`{"model":"m","usage":{"prompt_tokens":-5,"completion_tokens":-1,"total_tokens":0}}`)
	rec, _ := ParseBody(body)
	if rec.PromptTokens != 0 || rec.CompletionTokens != 0 {
		t.Errorf("negative values not clamped: %+v", rec)
	}
}
