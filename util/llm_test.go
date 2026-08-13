package util

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func enableTestLLM(t *testing.T) {
	t.Helper()
	prevK, prevU, prevM := OpenAIAPIKey, OpenAIBaseURL, OpenAIModel
	OpenAIAPIKey = "test-key"
	OpenAIBaseURL = "http://127.0.0.1:9"
	OpenAIModel = "test-model"
	t.Cleanup(func() {
		OpenAIAPIKey, OpenAIBaseURL, OpenAIModel = prevK, prevU, prevM
	})
}

func mockTranslate(system, user string) (string, error) {
	prefix := "译"
	if strings.Contains(system, "罗马音") {
		prefix = "roma "
	}
	var out []string
	for _, line := range strings.Split(user, "\n") {
		if m := numberedLineRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, m[1]+". "+prefix+m[2])
			continue
		}
		out = append(out, prefix+line)
	}
	return strings.Join(out, "\n"), nil
}

func TestIsTransientLLMError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("unexpected end of JSON input"), true},
		{errors.New("Client.Timeout exceeded while awaiting headers"), true},
		{errors.New("context deadline exceeded"), true},
		{errors.New("LLM HTTP 429: rate limit"), true},
		{errors.New("LLM HTTP 502: bad gateway"), true},
		{errors.New("connection reset by peer"), true},
		{errors.New("dial tcp: lookup api.deepseek.com: no such host"), true},
		{errors.New("unexpected EOF"), true},
		{errors.New("LLM not configured"), false},
		{errors.New("LLM returned no choices"), false},
		{errors.New("LLM HTTP 400: bad request"), false},
	}
	for _, c := range cases {
		if got := isTransientLLMError(c.err); got != c.want {
			t.Errorf("isTransientLLMError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestChatCompleteRetriesTransient(t *testing.T) {
	enableTestLLM(t)
	prevA, prevB := llmRetryAttempts, llmRetryBackoff
	llmRetryAttempts = 3
	llmRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() {
		llmRetryAttempts, llmRetryBackoff = prevA, prevB
	})

	var n atomic.Int32
	restore := SwapInvokeChat(func(system, user string) (string, error) {
		i := n.Add(1)
		if i < 3 {
			return "", errors.New("unexpected end of JSON input")
		}
		return "ok", nil
	})
	defer restore()

	got, err := chatComplete("sys", "user")
	if err != nil {
		t.Fatalf("chatComplete: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %q", got)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", n.Load())
	}
}

func TestChatCompleteNoRetryOnPermanent(t *testing.T) {
	enableTestLLM(t)
	prevA, prevB := llmRetryAttempts, llmRetryBackoff
	llmRetryAttempts = 3
	llmRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() {
		llmRetryAttempts, llmRetryBackoff = prevA, prevB
	})

	var n atomic.Int32
	restore := SwapInvokeChat(func(system, user string) (string, error) {
		n.Add(1)
		return "", errors.New("LLM HTTP 400: bad request")
	})
	defer restore()

	_, err := chatComplete("sys", "user")
	if err == nil {
		t.Fatal("expected error")
	}
	if n.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", n.Load())
	}
}

func TestLlmTransformChunks(t *testing.T) {
	enableTestLLM(t)
	prev := llmChunkSize
	llmChunkSize = 2
	t.Cleanup(func() { llmChunkSize = prev })

	var sizes []int
	restore := SwapInvokeChat(func(system, user string) (string, error) {
		lines := strings.Split(user, "\n")
		sizes = append(sizes, len(lines))
		return mockTranslate(system, user)
	})
	defer restore()

	lines := []string{"a", "b", "c", "d", "e"}
	got, err := llmTransform(translateSystemPrompt, lines)
	if err != nil {
		t.Fatalf("llmTransform: %v", err)
	}
	if got == nil {
		t.Fatal("expected output")
	}
	if len(got) != 5 {
		t.Fatalf("len=%d want 5: %v", len(got), got)
	}
	if got[0] != "译a" || got[4] != "译e" {
		t.Fatalf("unexpected output: %v", got)
	}
	wantSizes := []int{2, 2, 1}
	if len(sizes) != len(wantSizes) {
		t.Fatalf("chunk calls %v, want %v", sizes, wantSizes)
	}
	for i := range wantSizes {
		if sizes[i] != wantSizes[i] {
			t.Fatalf("chunk calls %v, want %v", sizes, wantSizes)
		}
	}
}

func TestLlmTransformLineMismatch(t *testing.T) {
	enableTestLLM(t)
	prev := llmChunkSize
	llmChunkSize = 40
	t.Cleanup(func() { llmChunkSize = prev })

	restore := SwapInvokeChat(func(system, user string) (string, error) {
		return "xxx\nyyy\nzzz", nil
	})
	defer restore()

	got, err := llmTransform(translateSystemPrompt, []string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil on mismatch, got %v", got)
	}
}

func TestParseNumbered(t *testing.T) {
	got := parseNumbered("1. 你好\n2. 世界", 2)
	if got == nil || got[0] != "你好" || got[1] != "世界" {
		t.Fatalf("basic: %v", got)
	}
	got = parseNumbered("2) world\n1) hello", 2)
	if got == nil || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("out of order: %v", got)
	}
	got = parseNumbered("preamble\n1. a\n\n2. b\nextra", 2)
	if got == nil || got[0] != "a" || got[1] != "b" {
		t.Fatalf("noise: %v", got)
	}
	got = parseNumbered("foo\nbar", 2)
	if got == nil || got[0] != "foo" || got[1] != "bar" {
		t.Fatalf("plain fallback: %v", got)
	}
	got = parseNumbered("1. only", 3)
	if got != nil {
		t.Fatalf("incomplete numbered should fail: %v", got)
	}
}

func TestLlmTransformSplitsOnMismatch(t *testing.T) {
	enableTestLLM(t)
	prev := llmChunkSize
	llmChunkSize = 40
	t.Cleanup(func() { llmChunkSize = prev })

	var sizes []int
	restore := SwapInvokeChat(func(system, user string) (string, error) {
		n := len(strings.Split(user, "\n"))
		sizes = append(sizes, n)
		if n >= 4 {
			return "1. x\n2. y", nil
		}
		return mockTranslate(system, user)
	})
	defer restore()

	got, err := llmTransform(translateSystemPrompt, []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("llmTransform: %v", err)
	}
	if got == nil {
		t.Fatal("expected split recovery")
	}
	want := []string{"译a", "译b", "译c", "译d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if len(sizes) < 3 || sizes[0] != 4 {
		t.Fatalf("expected first call of 4 then splits, got %v", sizes)
	}
}

func TestCompleteLyricKeepsTimestampsAligned(t *testing.T) {
	enableTestLLM(t)
	restore := SwapInvokeChat(mockTranslate)
	defer restore()

	lyric := "[00:01.00]こんにちは\n[00:05.00]ありがとう\n[00:10.00]さようなら"
	newLyric, newRomaji, changed, err := CompleteLyric(lyric, "")
	if err != nil || !changed {
		t.Fatalf("CompleteLyric changed=%v err=%v", changed, err)
	}
	orig, trans := originalAndTranslation(newLyric)
	if len(orig) != 3 || len(trans) != 3 {
		t.Fatalf("orig=%d trans=%d", len(orig), len(trans))
	}
	for i, ts := range []string{"[00:01.00]", "[00:05.00]", "[00:10.00]"} {
		if orig[i].Timestamp != ts || trans[i].Timestamp != ts {
			t.Fatalf("ts mismatch at %d: orig=%s trans=%s", i, orig[i].Timestamp, trans[i].Timestamp)
		}
		if !strings.Contains(newRomaji, ts) {
			t.Fatalf("romaji missing %s", ts)
		}
	}
}

func TestCompleteLyricUsesChunks(t *testing.T) {
	enableTestLLM(t)
	prev := llmChunkSize
	llmChunkSize = 2
	t.Cleanup(func() { llmChunkSize = prev })

	var calls int
	restore := SwapInvokeChat(func(system, user string) (string, error) {
		calls++
		return mockTranslate(system, user)
	})
	defer restore()

	var sb strings.Builder
	for i := 0; i < 5; i++ {
		sb.WriteString("[00:0")
		sb.WriteByte(byte('1' + i))
		sb.WriteString(".00]こんにちは")
		sb.WriteByte('\n')
	}
	lyric := strings.TrimRight(sb.String(), "\n")
	newLyric, newRomaji, changed, err := CompleteLyric(lyric, "")
	if err != nil {
		t.Fatalf("CompleteLyric: %v", err)
	}
	if !changed {
		t.Fatal("expected changed")
	}
	if !IsLyricComplete(newLyric, newRomaji) {
		t.Fatal("expected complete")
	}
	// 5 lines / chunk 2 = 3 chunks, times 2 (trans+roma)
	if calls != 6 {
		t.Fatalf("calls=%d want 6", calls)
	}
}

func TestParseTransformLinesTrims(t *testing.T) {
	got := parseTransformLines("\nfoo\nbar\n")
	if len(got) != 2 || got[0] != "foo" || got[1] != "bar" {
		t.Fatalf("got %v", got)
	}
	got = parseTransformLines("```\na\nb\n```")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("fenced got %v", got)
	}
}

func TestSwapInvokeChatRestores(t *testing.T) {
	prev := invokeChat
	restore := SwapInvokeChat(func(string, string) (string, error) { return "x", nil })
	restore()
	if invokeChat == nil {
		t.Fatal("invokeChat nil after restore")
	}
	_ = prev
}
