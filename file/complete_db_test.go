package file

import (
	"os"
	"path/filepath"
	"testing"

	"lrcAPI/util"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func snapshotDBPath(t *testing.T) string {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "data", "lyrics.db"),
		filepath.Join("data", "lyrics.db"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("data/lyrics.db not found")
	return ""
}

func openSnapshotRO(t *testing.T) *gorm.DB {
	t.Helper()
	path := snapshotDBPath(t)
	db, err := gorm.Open(sqlite.Open("file:"+path+"?mode=ro"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() { closeDB(db) })
	return db
}

func TestSnapshotIncompleteStats(t *testing.T) {
	db := openSnapshotRO(t)
	var total, incomplete int64
	if err := db.Model(&lyricRow{}).Count(&total).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&lyricRow{}).Where("is_complete = 0").Count(&incomplete).Error; err != nil {
		t.Fatal(err)
	}
	if total < 2000 {
		t.Fatalf("unexpected snapshot size: %d", total)
	}
	if incomplete == 0 {
		t.Fatal("expected incomplete rows in snapshot")
	}
	t.Logf("snapshot: total=%d incomplete=%d", total, incomplete)
}

func TestCompleteSnapshotIncompleteWithMock(t *testing.T) {
	src := openSnapshotRO(t)
	var rows []lyricRow
	if err := src.Where("is_complete = 0 AND source != ?", "fallback").
		Order("length(lyric) ASC").
		Limit(8).
		Find(&rows).Error; err != nil {
		t.Fatalf("load incomplete: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no incomplete rows")
	}

	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)

	dst, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Create(&rows).Error; err != nil {
		closeDB(dst)
		t.Fatalf("copy rows: %v", err)
	}
	closeDB(dst)

	restore := util.SwapInvokeChat(mockTranslate)
	defer restore()

	seen := map[string]struct{}{}
	for _, row := range rows {
		if _, ok := seen[row.CacheKey]; ok {
			continue
		}
		seen[row.CacheKey] = struct{}{}
		if err := completeLyrics(row.CacheKey); err != nil {
			t.Fatalf("completeLyrics %q: %v", row.CacheKey, err)
		}
	}

	var left []lyricRow
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB(db)
	if err := db.Where("is_complete = 0").Find(&left).Error; err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		for _, r := range left {
			t.Logf("still incomplete: %s#%s src=%s len=%d complete=%v",
				r.CacheKey, r.LyricID, r.Source, len(r.Lyric), util.IsLyricComplete(r.Lyric, r.Romaji))
		}
		t.Fatalf("%d rows still incomplete after mock completion", len(left))
	}
}

func TestCompleteSnapshotLongestWithChunks(t *testing.T) {
	src := openSnapshotRO(t)
	var row lyricRow
	if err := src.Where("is_complete = 0 AND source != ?", "fallback").
		Order("length(lyric) DESC").
		First(&row).Error; err != nil {
		t.Fatalf("load longest: %v", err)
	}
	t.Logf("longest incomplete: %s#%s chars=%d", row.CacheKey, row.LyricID, len(row.Lyric))

	useTempLyricsDB(t)
	enableTestLLM(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)

	dst, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Create(&row).Error; err != nil {
		closeDB(dst)
		t.Fatal(err)
	}
	closeDB(dst)

	var calls int
	restore := util.SwapInvokeChat(func(system, user string) (string, error) {
		calls++
		return mockTranslate(system, user)
	})
	defer restore()

	if err := completeLyrics(row.CacheKey); err != nil {
		t.Fatalf("completeLyrics: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected chunked calls, got %d", calls)
	}
	t.Logf("longest song LLM chunk calls: %d", calls)

	var got lyricRow
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB(db)
	if err := db.Where(lyricPrimaryKeyFilter(row.CacheKey, row.LyricID)).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.IsComplete == 0 {
		t.Fatal("longest song still incomplete after chunked mock")
	}
}

func TestCompleteSnapshotLiveShort(t *testing.T) {
	util.LoadEnv("../.env")
	if !util.LLMEnabled() {
		t.Skip("LLM not configured; skipping live snapshot test")
	}

	src := openSnapshotRO(t)
	var rows []lyricRow
	if err := src.Where("is_complete = 0 AND source != ?", "fallback").
		Order("length(lyric) ASC").
		Limit(30).
		Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var pick *lyricRow
	for i := range rows {
		if !util.IsLyricComplete(rows[i].Lyric, rows[i].Romaji) {
			pick = &rows[i]
			break
		}
	}
	if pick == nil {
		t.Skip("no truly incomplete row")
	}
	t.Logf("live pick: %s#%s chars=%d", pick.CacheKey, pick.LyricID, len(pick.Lyric))

	useTempLyricsDB(t)
	resetCompleteFailures()
	t.Cleanup(resetCompleteFailures)

	dst, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Create(pick).Error; err != nil {
		closeDB(dst)
		t.Fatal(err)
	}
	closeDB(dst)

	if err := completeLyrics(pick.CacheKey); err != nil {
		t.Fatalf("completeLyrics: %v", err)
	}

	var got lyricRow
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB(db)
	if err := db.Where(lyricPrimaryKeyFilter(pick.CacheKey, pick.LyricID)).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("after live: complete=%d lyric_len=%d romaji_len=%d", got.IsComplete, len(got.Lyric), len(got.Romaji))
	if got.IsComplete == 0 {
		t.Errorf("short snapshot lyric still incomplete after live LLM")
	}
	orig, trans := util.SplitOriginalAndTranslation(got.Lyric)
	if len(orig) == 0 || len(trans) != len(orig) {
		t.Fatalf("translation not aligned: orig=%d trans=%d", len(orig), len(trans))
	}
	for i := range orig {
		if orig[i].Timestamp != trans[i].Timestamp {
			t.Fatalf("timestamp mismatch at %d: %s vs %s", i, orig[i].Timestamp, trans[i].Timestamp)
		}
	}
	roma := util.ParseLrc(got.Romaji)
	if len(roma) == 0 {
		t.Fatal("expected romaji lines")
	}
	origTS := map[string]struct{}{}
	for _, ln := range orig {
		origTS[ln.Timestamp] = struct{}{}
	}
	for _, ln := range roma {
		if _, ok := origTS[ln.Timestamp]; !ok {
			t.Fatalf("romaji timestamp %s not in original", ln.Timestamp)
		}
	}
}
