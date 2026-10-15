package handler

import (
	"github.com/gin-gonic/gin"
	"log"
	"lrcAPI/file"
	"lrcAPI/request"
	"lrcAPI/util"
	"net/http"
	"strconv"
)

func lyricHandler(c *gin.Context) {
	if c.Request.Header.Get("Authorization") != pwd {
		log.Println("authorization required")
		c.JSON(404, gin.H{})
		return
	}
	var lyricRequest request.Request
	lyricRequest.Processor.Title = c.Query("title")
	lyricRequest.Processor.Artist = c.Query("artist")
	delOp := c.Query("delOp")
	lyricRequest.File.FolderName = lyricRequest.Processor.Artist + " - " + lyricRequest.Processor.Title
	if delOp == "true" {
		log.Printf("Delete Tmp for %s\n", lyricRequest.Processor.Artist)
		if err := lyricRequest.File.RemoveLyric(); err != nil {
			util.ErrorPrinter(err)
		}
		c.JSON(http.StatusOK, gin.H{
			"Delete": lyricRequest.File.FolderName,
		})
		return
	}
	if err := lyricRequest.File.ReadLyric(); err == nil {
		log.Println("found exist")
		// 后台异步补齐缺失的翻译/罗马音，对客户端无感
		file.CompleteLyricsAsync(lyricRequest.Processor.Title, lyricRequest.Processor.Artist)
		c.JSON(http.StatusOK, lyricRequest.File.InfoLyric)
		return
	}
	if err := lyricRequest.Processor.Process(); err != nil {
		util.ErrorPrinter(err)
	}
	for index, value := range lyricRequest.Processor.InfoLyric {
		lyricType := value.Type
		if lyricType == "" {
			lyricType = "lrc"
		}
		lyricRequest.File.InfoLyric = append(lyricRequest.File.InfoLyric, file.InfoLyric{
			ID:         strconv.Itoa(index),
			Title:      value.Title,
			Artist:     value.Artist,
			Lyric:      value.Lyric,
			Romaji:     value.Romaji,
			Type:       lyricType,
			Source:     value.Source,
			IsComplete: value.Source == "fallback" || util.IsLyricComplete(value.Lyric, value.Romaji),
		})
	}
	if err := lyricRequest.File.WriteLyric(); err != nil {
		util.ErrorPrinter(err)
	}
	// 首次查询后同样触发后台补全，下次查询即可拿到补齐后的数据
	file.CompleteLyricsAsync(lyricRequest.Processor.Title, lyricRequest.Processor.Artist)
	c.JSON(http.StatusOK, lyricRequest.File.InfoLyric)
}

// backfillHandler 触发一次全库存量补全：把所有不完备歌曲按批合并成尽量少的
// LLM 请求顺序处理（避免逐首并发打满上游限流）。异步执行，立即返回。
// 可选 query 参数 batch=每批合并的歌曲数（默认 12）。
func backfillHandler(c *gin.Context) {
	if c.Request.Header.Get("Authorization") != pwd {
		log.Println("authorization required")
		c.JSON(404, gin.H{})
		return
	}
	batch := 0
	if v := c.Query("batch"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			batch = n
		}
	}
	if !file.BackfillAsync(batch) {
		c.JSON(http.StatusConflict, gin.H{"status": "backfill already running or LLM disabled"})
		return
	}
	log.Printf("backfill triggered (batch=%d)", batch)
	c.JSON(http.StatusAccepted, gin.H{"status": "started", "batch": batch})
}
