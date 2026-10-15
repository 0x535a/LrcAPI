package file

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lrcAPI/util"
)

// 端到端：写入不完备的日语歌词 → 后台补全逻辑 → 重新读取应变为完备。
// 需要真实模型凭据；未配置则跳过。
func TestCompleteLyricsRoundTrip(t *testing.T) {
	util.LoadEnv("../.env")
	if !util.LLMEnabled() {
		t.Skip("LLM not configured; skipping live round-trip test")
	}
	useTempLyricsDB(t)

	f := File{FolderName: "__test_artist__ - __test_title__"}
	f.InfoLyric = []InfoLyric{
		{
			ID:     "0",
			Title:  "テスト",
			Artist: "__test_artist__",
			Lyric:  "[00:01.00]こんにちは\n[00:05.00]ありがとう",
			Romaji: "",
			Type:   "lrc",
			Source: "netease",
		},
	}
	if err := f.WriteLyric(); err != nil {
		t.Fatalf("WriteLyric: %v", err)
	}
	t.Cleanup(func() {
		rm := File{FolderName: f.FolderName}
		_ = rm.RemoveLyric()
	})

	// 直接同步调用内部补全逻辑（等价于后台协程执行体）
	if err := completeLyrics(f.FolderName); err != nil {
		t.Fatalf("completeLyrics: %v", err)
	}

	var read File
	read.FolderName = f.FolderName
	if err := read.ReadLyric(); err != nil {
		t.Fatalf("ReadLyric: %v", err)
	}
	if len(read.InfoLyric) != 1 {
		t.Fatalf("expected 1 row, got %d", len(read.InfoLyric))
	}
	got := read.InfoLyric[0]
	t.Logf("lyric after completion:\n%s", got.Lyric)
	t.Logf("romaji after completion:\n%s", got.Romaji)
	if !got.IsComplete {
		t.Errorf("expected IsComplete=true after completion")
	}
	if got.Romaji == "" {
		t.Errorf("expected romaji to be filled")
	}
}

func useTempLyricsDB(t *testing.T) {
	t.Helper()
	oldPath := lyricsDBPath
	lyricsDBPath = filepath.Join(t.TempDir(), "lyrics.db")
	t.Cleanup(func() {
		lyricsDBPath = oldPath
	})
}

func enableTestLLM(t *testing.T) {
	t.Helper()
	prevK, prevU, prevM := util.OpenAIAPIKey, util.OpenAIBaseURL, util.OpenAIModel
	util.OpenAIAPIKey = "test-key"
	util.OpenAIBaseURL = "http://127.0.0.1:9"
	util.OpenAIModel = "test-model"
	t.Cleanup(func() {
		util.OpenAIAPIKey, util.OpenAIBaseURL, util.OpenAIModel = prevK, prevU, prevM
	})
}

var numberedLineRe = regexp.MustCompile(`^\s*(\d+)\s*[\.\)\:：．、]\s*(.*)$`)

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

func writeIncompleteJA(t *testing.T, cacheKey, lyricID string) {
	t.Helper()
	f := File{FolderName: cacheKey}
	f.InfoLyric = []InfoLyric{{
		ID:     lyricID,
		Title:  "テスト",
		Artist: "artist",
		Lyric:  "[00:01.00]こんにちは\n[00:05.00]ありがとう",
		Type:   "lrc",
		Source: "netease",
	}}
	if err := f.WriteLyric(); err != nil {
		t.Fatalf("WriteLyric: %v", err)
	}
}

func TestCompleteFailCooldown(t *testing.T) {
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)
	oldLimit, oldCool := completeFailLimit, completeCooldown
	completeFailLimit = 3
	completeCooldown = time.Hour
	t.Cleanup(func() {
		completeFailLimit, completeCooldown = oldLimit, oldCool
	})

	key, id := "a - b", "0"
	if shouldSkipComplete(key, id) {
		t.Fatal("should not skip initially")
	}
	recordCompleteFail(key, id)
	recordCompleteFail(key, id)
	if shouldSkipComplete(key, id) {
		t.Fatal("should not skip before limit")
	}
	recordCompleteFail(key, id)
	if !shouldSkipComplete(key, id) {
		t.Fatal("should skip after limit")
	}
	recordCompleteSuccess(key, id)
	if shouldSkipComplete(key, id) {
		t.Fatal("should not skip after success")
	}
}

func TestCompleteCooldownExpires(t *testing.T) {
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)
	oldLimit, oldCool := completeFailLimit, completeCooldown
	completeFailLimit = 1
	completeCooldown = 20 * time.Millisecond
	t.Cleanup(func() {
		completeFailLimit, completeCooldown = oldLimit, oldCool
	})

	key, id := "a - b", "1"
	recordCompleteFail(key, id)
	if !shouldSkipComplete(key, id) {
		t.Fatal("should skip during cooldown")
	}
	time.Sleep(40 * time.Millisecond)
	if shouldSkipComplete(key, id) {
		t.Fatal("should not skip after cooldown")
	}
}

func TestCompleteLyricsSkipsCooldown(t *testing.T) {
	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)
	oldLimit, oldCool := completeFailLimit, completeCooldown
	completeFailLimit = 2
	completeCooldown = time.Hour
	t.Cleanup(func() {
		completeFailLimit, completeCooldown = oldLimit, oldCool
	})

	cacheKey := "artist - title"
	writeIncompleteJA(t, cacheKey, "0")
	recordCompleteFail(cacheKey, "0")
	recordCompleteFail(cacheKey, "0")

	var calls atomic.Int32
	restore := util.SwapInvokeChat(func(system, user string) (string, error) {
		calls.Add(1)
		return mockTranslate(system, user)
	})
	defer restore()

	if err := completeLyrics(cacheKey); err != nil {
		t.Fatalf("completeLyrics: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("LLM called %d times during cooldown", calls.Load())
	}
}

func TestCompleteLyricsWritesPartialOnRomaError(t *testing.T) {
	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)

	cacheKey := "artist - partial"
	writeIncompleteJA(t, cacheKey, "0")

	defer util.SwapLLMRetry(1, nil)()
	restore := util.SwapInvokeChat(func(system, user string) (string, error) {
		if strings.Contains(system, "罗马音") {
			return "", errors.New("unexpected end of JSON input")
		}
		return mockTranslate(system, user)
	})
	defer restore()

	if err := completeLyrics(cacheKey); err != nil {
		t.Fatalf("completeLyrics: %v", err)
	}

	var read File
	read.FolderName = cacheKey
	if err := read.ReadLyric(); err != nil {
		t.Fatalf("ReadLyric: %v", err)
	}
	if len(read.InfoLyric) != 1 {
		t.Fatalf("rows=%d", len(read.InfoLyric))
	}
	got := read.InfoLyric[0]
	if !strings.Contains(got.Lyric, "译") {
		t.Fatalf("expected partial translation written: %s", got.Lyric)
	}
	if got.IsComplete {
		t.Fatal("should still be incomplete without romaji")
	}
}

func TestCompleteLyricsMockRoundTrip(t *testing.T) {
	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)

	cacheKey := "artist - ok"
	writeIncompleteJA(t, cacheKey, "0")
	restore := util.SwapInvokeChat(mockTranslate)
	defer restore()

	if err := completeLyrics(cacheKey); err != nil {
		t.Fatalf("completeLyrics: %v", err)
	}
	var read File
	read.FolderName = cacheKey
	if err := read.ReadLyric(); err != nil {
		t.Fatalf("ReadLyric: %v", err)
	}
	if !read.InfoLyric[0].IsComplete {
		t.Fatal("expected complete")
	}
}

// 多歌曲批量补全：两首歌合并成「翻译 1 次 + 罗马音 1 次」共 2 次 LLM 调用，
// 而不是每首各 2 次；且两首都应变完备。
func TestCompleteManyBatchesSongs(t *testing.T) {
	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)

	writeIncompleteJA(t, "artistA - songA", "0")
	writeIncompleteJA(t, "artistB - songB", "0")

	var calls atomic.Int32
	restore := util.SwapInvokeChat(func(system, user string) (string, error) {
		calls.Add(1)
		return mockTranslate(system, user)
	})
	defer restore()

	if err := completeMany([]string{"artistA - songA", "artistB - songB"}); err != nil {
		t.Fatalf("completeMany: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("LLM calls=%d want 2 (两首歌应合并请求)", calls.Load())
	}
	for _, key := range []string{"artistA - songA", "artistB - songB"} {
		var read File
		read.FolderName = key
		if err := read.ReadLyric(); err != nil {
			t.Fatalf("ReadLyric %q: %v", key, err)
		}
		if !read.InfoLyric[0].IsComplete {
			t.Errorf("%q not complete after batch", key)
		}
	}
}

// 全库回填：IncompleteCacheKeys 列出待补歌曲，backfillAll 顺序批量补齐后清空。
func TestBackfillAllCompletesIncomplete(t *testing.T) {
	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)

	writeIncompleteJA(t, "artistA - songA", "0")
	writeIncompleteJA(t, "artistB - songB", "0")

	restore := util.SwapInvokeChat(mockTranslate)
	defer restore()

	keys, err := IncompleteCacheKeys()
	if err != nil {
		t.Fatalf("IncompleteCacheKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("IncompleteCacheKeys=%v want 2 keys", keys)
	}

	if err := backfillAll(12); err != nil {
		t.Fatalf("backfillAll: %v", err)
	}

	rest, err := IncompleteCacheKeys()
	if err != nil {
		t.Fatalf("IncompleteCacheKeys after: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("after backfill still incomplete: %v", rest)
	}
}

// 回填时应跳过处于失败冷却中的歌曲，不为其调用 LLM。
func TestBackfillRespectsCooldown(t *testing.T) {
	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)
	oldLimit, oldCool := completeFailLimit, completeCooldown
	completeFailLimit = 2
	completeCooldown = time.Hour
	t.Cleanup(func() { completeFailLimit, completeCooldown = oldLimit, oldCool })

	writeIncompleteJA(t, "artistA - songA", "0")
	recordCompleteFail("artistA - songA", "0")
	recordCompleteFail("artistA - songA", "0")

	var calls atomic.Int32
	restore := util.SwapInvokeChat(func(system, user string) (string, error) {
		calls.Add(1)
		return mockTranslate(system, user)
	})
	defer restore()

	if err := backfillAll(12); err != nil {
		t.Fatalf("backfillAll: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("LLM called %d times for cooled-down song", calls.Load())
	}
}
