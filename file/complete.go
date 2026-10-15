package file

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"lrcAPI/util"

	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// 同一 cache_key 的补全任务去重，避免并发请求重复调用大模型。
var completeInflight sync.Map

var (
	completeFailLimit = 3
	completeCooldown  = 24 * time.Hour
)

type failState struct {
	count     int
	coolUntil time.Time
}

var completeFailures sync.Map

func resetCompleteFailures() {
	completeFailures.Range(func(k, _ any) bool {
		completeFailures.Delete(k)
		return true
	})
}

func failKey(cacheKey, lyricID string) string {
	return cacheKey + "#" + lyricID
}

func shouldSkipComplete(cacheKey, lyricID string) bool {
	v, ok := completeFailures.Load(failKey(cacheKey, lyricID))
	if !ok {
		return false
	}
	st := v.(failState)
	if st.coolUntil.IsZero() {
		return false
	}
	if time.Now().Before(st.coolUntil) {
		return true
	}
	completeFailures.Delete(failKey(cacheKey, lyricID))
	return false
}

func recordCompleteFail(cacheKey, lyricID string) {
	key := failKey(cacheKey, lyricID)
	var st failState
	if v, ok := completeFailures.Load(key); ok {
		st = v.(failState)
	}
	if !st.coolUntil.IsZero() && time.Now().Before(st.coolUntil) {
		return
	}
	st.count++
	if st.count >= completeFailLimit {
		st.coolUntil = time.Now().Add(completeCooldown)
		log.Printf("lyric completion: cooling down %q for %s after %d failures", key, completeCooldown, st.count)
	}
	completeFailures.Store(key, st)
}

func recordCompleteSuccess(cacheKey, lyricID string) {
	completeFailures.Delete(failKey(cacheKey, lyricID))
}

// computeComplete 计算一条歌词是否完备：fallback 占位恒完备，其余按内容判定。
func computeComplete(source, lyric, romaji string) bool {
	return source == "fallback" || util.IsLyricComplete(lyric, romaji)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// dbRow 是补全流程读出的单条歌词。
type dbRow struct {
	cacheKey string
	lyricID  string
	lyric    string
	romaji   string
	source   string
	complete bool
}

// CompleteLyricsAsync 是异步、无返回值的歌词补全入口。
//
// 它接收 title 与 artist，在后台协程中：
//  1. 按 `${artist} - ${title}` 在数据库中检索候选歌词；
//  2. 通过 IsLyricComplete 判断每条是否完备（外语缺中文翻译、日/韩缺罗马音即不完备）；
//  3. 对不完备的条目调用大模型补齐，并做行数一致性校验以防幻觉；
//  4. 将补齐后的结果写回数据库。
//
// 从客户端角度看该过程完全无感：调用后立即返回，补全在后台进行，
// 结果会在下一次查询同一歌曲时体现。
func CompleteLyricsAsync(title, artist string) {
	if !util.LLMEnabled() {
		return
	}
	cacheKey := artist + " - " + title
	// 已有同键任务在跑则跳过
	if _, loaded := completeInflight.LoadOrStore(cacheKey, struct{}{}); loaded {
		return
	}
	go func() {
		defer completeInflight.Delete(cacheKey)
		if err := completeLyrics(cacheKey); err != nil {
			util.ErrorPrinter(errors.Wrap(err, "completeLyrics"))
		}
	}()
}

// completeLyrics 补全单个 cache_key 下的歌词（同步执行，由协程调用）。
func completeLyrics(cacheKey string) error {
	rows, err := loadRows(cacheKey)
	if err != nil {
		return err
	}
	n, err := completeRows(rows)
	if n > 0 {
		log.Printf("lyric completion: updated %d entries for %q", n, cacheKey)
	}
	return err
}

// completeMany 批量补全多个 cache_key：把所有歌曲需要翻译/罗马音的行合并成
// 尽量少的 LLM 请求顺序处理，避免逐首并发打满上游限流。
func completeMany(cacheKeys []string) error {
	if len(cacheKeys) == 0 {
		return nil
	}
	rows, err := loadRowsMulti(cacheKeys)
	if err != nil {
		return err
	}
	n, err := completeRows(rows)
	if n > 0 {
		log.Printf("lyric completion: batch updated %d entries across %d songs", n, len(cacheKeys))
	}
	return err
}

// completeRows 对给定行（可跨多个 cache_key）批量补全并写回，返回更新行数。
// fallback 占位、已完备、以及处于失败冷却中的行会被跳过。
// 即使 LLM 调用出错，已完成的部分（如翻译成功但罗马音失败）也会写回，err 一并返回。
func completeRows(rows []dbRow) (int, error) {
	type candidate struct {
		row dbRow
		idx int
	}
	var cands []candidate
	var items []util.CompletionInput
	for _, r := range rows {
		if r.source == "fallback" || r.complete {
			continue
		}
		if shouldSkipComplete(r.cacheKey, r.lyricID) {
			continue
		}
		cands = append(cands, candidate{row: r, idx: len(items)})
		items = append(items, util.CompletionInput{Lyric: r.lyric, Romaji: r.romaji})
	}
	if len(items) == 0 {
		return 0, nil
	}

	results, batchErr := util.CompleteLyricsBatch(items)
	if batchErr != nil {
		// LLM 调用出错只记录日志并计入失败冷却，已完成的部分仍会写回；
		// 不作为致命错误向上返回（与原逐行补全行为一致）。
		util.ErrorPrinter(errors.Wrap(batchErr, "CompleteLyricsBatch"))
	}

	type update struct {
		cacheKey string
		lyricID  string
		lyric    string
		romaji   string
		complete bool
	}
	var updates []update
	for _, c := range cands {
		res := results[c.idx]
		nowComplete := computeComplete(c.row.source, res.Lyric, res.Romaji)
		if nowComplete {
			recordCompleteSuccess(c.row.cacheKey, c.row.lyricID)
		} else {
			recordCompleteFail(c.row.cacheKey, c.row.lyricID)
		}
		// 内容有更新，或存储的 is_complete 与实际不一致（自愈旧数据）时都写回
		if res.Changed || nowComplete != c.row.complete {
			updates = append(updates, update{
				cacheKey: c.row.cacheKey,
				lyricID:  c.row.lyricID,
				lyric:    res.Lyric,
				romaji:   res.Romaji,
				complete: nowComplete,
			})
		}
	}

	if len(updates) == 0 {
		return 0, nil
	}

	db, err := openDB()
	if err != nil {
		return 0, err
	}
	defer closeDB(db)
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, u := range updates {
			row := lyricRow{
				Lyric:      u.lyric,
				Romaji:     u.romaji,
				IsComplete: boolToInt(u.complete),
			}
			if err := tx.Model(&lyricRow{}).
				Where(lyricPrimaryKeyFilter(u.cacheKey, u.lyricID)).
				Select("Lyric", "Romaji", "IsComplete").
				Updates(row).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, errors.WithStack(err)
	}
	return len(updates), nil
}

// loadRows 读取某个 cache_key 下的全部歌词行后立即关闭连接。
func loadRows(cacheKey string) ([]dbRow, error) {
	return loadRowsMulti([]string{cacheKey})
}

// loadRowsMulti 读取多个 cache_key 下的全部歌词行后立即关闭连接。
func loadRowsMulti(cacheKeys []string) ([]dbRow, error) {
	if len(cacheKeys) == 0 {
		return nil, nil
	}
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer closeDB(db)

	var rows []lyricRow
	if err := db.Where("cache_key IN ?", cacheKeys).Find(&rows).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	out := make([]dbRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, dbRow{
			cacheKey: row.CacheKey,
			lyricID:  row.LyricID,
			lyric:    row.Lyric,
			romaji:   row.Romaji,
			source:   row.Source,
			complete: row.IsComplete != 0,
		})
	}
	return out, nil
}

// IncompleteCacheKeys 返回库中所有仍不完备（非 fallback）歌曲的 cache_key。
func IncompleteCacheKeys() ([]string, error) {
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	defer closeDB(db)

	var keys []string
	if err := db.Model(&lyricRow{}).
		Where("is_complete = ? AND source <> ?", 0, "fallback").
		Distinct().
		Pluck("cache_key", &keys).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return keys, nil
}

// defaultBackfillBatch 是全库回填时每批合并的歌曲数。
const defaultBackfillBatch = 12

// backfillRunning 保证同一时刻只有一个全库回填任务在跑。
var backfillRunning atomic.Bool

// BackfillAsync 触发一次全库存量补全：把所有不完备歌曲按批（每批 batchSize 首）
// 合并成尽量少的 LLM 请求顺序处理。已有回填在跑或 LLM 未配置时返回 false。
func BackfillAsync(batchSize int) bool {
	if !util.LLMEnabled() {
		log.Printf("backfill: LLM not configured, skip")
		return false
	}
	if batchSize <= 0 {
		batchSize = defaultBackfillBatch
	}
	if !backfillRunning.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer backfillRunning.Store(false)
		if err := backfillAll(batchSize); err != nil {
			util.ErrorPrinter(errors.Wrap(err, "backfill"))
		}
	}()
	return true
}

// backfillAll 顺序处理所有不完备歌曲（每批 batchSize 首合并请求）。
func backfillAll(batchSize int) error {
	keys, err := IncompleteCacheKeys()
	if err != nil {
		return err
	}
	// 跳过正被在线请求占用的 key，避免重复补全
	todo := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, loaded := completeInflight.LoadOrStore(k, struct{}{}); loaded {
			continue
		}
		todo = append(todo, k)
	}
	defer func() {
		for _, k := range todo {
			completeInflight.Delete(k)
		}
	}()

	if len(todo) == 0 {
		log.Printf("backfill: nothing to do")
		return nil
	}
	log.Printf("backfill: %d songs to complete, batch=%d", len(todo), batchSize)
	for i := 0; i < len(todo); i += batchSize {
		end := min(i+batchSize, len(todo))
		if err := completeMany(todo[i:end]); err != nil {
			// 单批失败不阻断整体回填
			util.ErrorPrinter(errors.Wrap(err, "backfill batch"))
		}
	}
	log.Printf("backfill: done (%d songs processed)", len(todo))
	return nil
}
