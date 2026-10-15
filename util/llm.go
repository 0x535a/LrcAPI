package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

var llmHTTPClient = &http.Client{
	Timeout:   180 * time.Second,
	Transport: NewIPv4Transport(),
}

var (
	llmChunkSize     = 40
	llmRetryAttempts = 3
	llmRetryBackoff  = []time.Duration{time.Second, 2 * time.Second}
	invokeChat       = chatCompleteOnce
)

func SwapInvokeChat(fn func(system, user string) (string, error)) func() {
	prev := invokeChat
	invokeChat = fn
	return func() { invokeChat = prev }
}

func SwapLLMRetry(attempts int, backoff []time.Duration) func() {
	prevA, prevB := llmRetryAttempts, llmRetryBackoff
	llmRetryAttempts = attempts
	llmRetryBackoff = backoff
	return func() {
		llmRetryAttempts = prevA
		llmRetryBackoff = prevB
	}
}

func LLMEnabled() bool {
	return OpenAIAPIKey != "" && OpenAIBaseURL != "" && OpenAIModel != ""
}

// LogLLMConfigWarning 在启动时输出 LLM 配置状态，避免补全功能
// 因配置缺失而静默关闭（历史上生产环境曾因此长期未生效且无任何日志）。
func LogLLMConfigWarning() {
	if LLMEnabled() {
		log.Printf("LLM lyric completion enabled (model %s via %s)", OpenAIModel, OpenAIBaseURL)
		return
	}
	missing := make([]string, 0, 3)
	if OpenAIAPIKey == "" {
		missing = append(missing, "OPENAI_API_KEY")
	}
	if OpenAIBaseURL == "" {
		missing = append(missing, "OPENAI_BASE_URL")
	}
	if OpenAIModel == "" {
		missing = append(missing, "OPENAI_MODEL")
	}
	log.Printf("WARNING: LLM lyric completion disabled: missing %s; foreign lyrics will not be auto-translated/romanized", strings.Join(missing, ", "))
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func isTransientLLMError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "unexpected end of json"):
		return true
	case strings.Contains(s, "timeout"):
		return true
	case strings.Contains(s, "deadline exceeded"):
		return true
	case strings.Contains(s, "llm http 429"):
		return true
	case strings.Contains(s, "llm http 5"):
		return true
	case strings.Contains(s, "connection reset"):
		return true
	case strings.Contains(s, "connection refused"):
		return true
	case strings.Contains(s, "no such host"):
		return true
	case strings.Contains(s, "eof"):
		return true
	default:
		return false
	}
}

func chatComplete(system, user string) (string, error) {
	if !LLMEnabled() {
		return "", errors.New("LLM not configured")
	}
	var last error
	for attempt := 0; attempt < llmRetryAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Second
			if attempt-1 < len(llmRetryBackoff) {
				delay = llmRetryBackoff[attempt-1]
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
		s, err := invokeChat(system, user)
		if err == nil {
			return s, nil
		}
		last = err
		if !isTransientLLMError(err) {
			return "", err
		}
	}
	return "", last
}

func chatCompleteOnce(system, user string) (string, error) {
	if !LLMEnabled() {
		return "", errors.New("LLM not configured")
	}
	reqBody := chatRequest{
		Model: OpenAIModel,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		Stream:      false,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", errors.WithStack(err)
	}
	endpoint := OpenAIBaseURL + "/chat/completions"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", errors.WithStack(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+OpenAIAPIKey)

	resp, err := llmHTTPClient.Do(req)
	if err != nil {
		return "", errors.WithStack(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf("LLM HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return "", errors.New("unexpected end of JSON input")
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", errors.WithStack(err)
	}
	if parsed.Error != nil {
		return "", errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("LLM returned no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

const translateSystemPrompt = `你是专业的歌词翻译。将用户提供的编号歌词翻译成简体中文。
严格要求：
1. 每一行输入都带编号，输出必须使用相同编号，第 i 行输出对应第 i 行输入。
2. 不要遗漏编号，不要合并行，不要添加注释、空行、时间戳或任何额外文字。
3. 对已经是中文的行，原样输出该行中文。
4. 只输出「编号. 译文」，不要输出任何解释。`

const romajiSystemPrompt = `你是专业的歌词罗马音转写。将用户提供的编号日语或韩语歌词转写为罗马音（日语用平文式 Hepburn 罗马字，韩语用韩语罗马字）。
严格要求：
1. 每一行输入都带编号，输出必须使用相同编号，第 i 行输出对应第 i 行输入。
2. 不要遗漏编号，不要合并行，不要添加注释、空行、时间戳或任何额外文字。
3. 罗马音使用小写拉丁字母，词间用空格分隔。
4. 只输出「编号. 罗马音」，不要输出任何解释。`

var numberedLineRe = regexp.MustCompile(`^\s*(\d+)\s*[\.\)\:：．、]\s*(.*)$`)

func formatNumbered(lines []string) string {
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%d. %s\n", i+1, line)
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseNumbered(content string, n int) []string {
	if n <= 0 {
		return nil
	}
	content = stripCodeFence(content)
	out := make([]string, n)
	got := make([]bool, n)
	count := 0
	for _, raw := range strings.Split(content, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		m := numberedLineRe.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil || idx < 1 || idx > n {
			continue
		}
		if !got[idx-1] {
			got[idx-1] = true
			count++
		}
		out[idx-1] = strings.TrimSpace(m[2])
	}
	if count == n {
		return out
	}
	plain := parseTransformLines(content)
	if len(plain) != n {
		return nil
	}
	for i, p := range plain {
		if m := numberedLineRe.FindStringSubmatch(strings.TrimSpace(p)); m != nil {
			if idx, err := strconv.Atoi(m[1]); err == nil && idx == i+1 {
				plain[i] = strings.TrimSpace(m[2])
			}
		}
	}
	return plain
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "```") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func parseTransformLines(content string) []string {
	content = stripCodeFence(content)
	out := strings.Split(content, "\n")
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func llmTransformChunkOnce(system string, lines []string) ([]string, error) {
	content, err := chatComplete(system, formatNumbered(lines))
	if err != nil {
		return nil, err
	}
	out := parseNumbered(content, len(lines))
	if out == nil {
		log.Printf("llmTransform: line align failed: want %d lines", len(lines))
		return nil, nil
	}
	return out, nil
}

func llmTransformChunk(system string, lines []string) ([]string, error) {
	out, err := llmTransformChunkOnce(system, lines)
	if err != nil {
		return nil, err
	}
	if out != nil {
		return out, nil
	}
	if len(lines) == 1 {
		out, err = llmTransformChunkOnce(system, lines)
		if err != nil {
			return nil, err
		}
		if out != nil {
			return out, nil
		}
		log.Printf("llmTransform: giving up on single line")
		return nil, nil
	}
	mid := len(lines) / 2
	left, err := llmTransformChunk(system, lines[:mid])
	if err != nil || left == nil {
		return nil, err
	}
	right, err := llmTransformChunk(system, lines[mid:])
	if err != nil || right == nil {
		return nil, err
	}
	return append(left, right...), nil
}

func llmTransform(system string, lines []string) ([]string, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	size := llmChunkSize
	if size <= 0 {
		size = len(lines)
	}
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i += size {
		end := i + size
		if end > len(lines) {
			end = len(lines)
		}
		chunk, err := llmTransformChunk(system, lines[i:end])
		if err != nil {
			return nil, err
		}
		if chunk == nil {
			return nil, nil
		}
		out = append(out, chunk...)
	}
	return out, nil
}

func buildBlended(original []LyricLine, trans map[int]string) string {
	var sb strings.Builder
	for i, ln := range original {
		sb.WriteString(ln.Timestamp)
		sb.WriteString(ln.Text)
		sb.WriteString("\n")
		if t, ok := trans[i]; ok && strings.TrimSpace(t) != "" {
			sb.WriteString(ln.Timestamp)
			sb.WriteString(t)
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// CompletionInput 是批量补全的输入单元。
type CompletionInput struct {
	Lyric  string
	Romaji string
}

// CompletionOutput 是批量补全的输出单元，与输入 items 一一对应。
type CompletionOutput struct {
	Lyric   string
	Romaji  string
	Changed bool
}

// CompleteLyricsBatch 对多条歌词批量补全。
//
// 与逐条调用 CompleteLyric 不同，它把所有条目中「需要翻译」的原文行合并成一次
// 编号请求、所有「需要罗马音」的原文行合并成另一次请求（内部仍按 llmChunkSize
// 分块并顺序执行），从而把请求数从「每条歌词各 2 发」降到「每批最多 2 类请求」，
// 并避免大量并发请求打满上游限流（429）。
//
// 返回值与 items 一一对应。即使中途出错，已完成的部分（例如翻译成功但罗马音
// 失败）也会保留在结果里，同时返回 err，调用方可据此写回部分内容。
func CompleteLyricsBatch(items []CompletionInput) ([]CompletionOutput, error) {
	results := make([]CompletionOutput, len(items))
	for i, it := range items {
		results[i] = CompletionOutput{Lyric: it.Lyric, Romaji: it.Romaji}
	}
	if len(items) == 0 || !LLMEnabled() {
		return results, nil
	}

	// lineRef 记录合并后某一行属于哪个条目的第几条原文行。
	type lineRef struct {
		item    int
		origIdx int
	}
	type parsedItem struct {
		original []LyricLine
	}

	parsed := make([]parsedItem, len(items))
	var transLines []string
	var transRefs []lineRef
	var romaLines []string
	var romaRefs []lineRef

	for i, it := range items {
		original, translation := originalAndTranslation(it.Lyric)
		lang := detectLang(joinOriginalText(original))
		needTrans := (lang == LangOther || lang == LangJapanese || lang == LangKorean) && !hasChineseTranslation(translation)
		needRoma := (lang == LangJapanese || lang == LangKorean) && !hasRomaji(it.Romaji)
		parsed[i] = parsedItem{original: original}
		if !needTrans && !needRoma {
			continue
		}
		for j, ln := range original {
			if strings.TrimSpace(ln.Text) == "" {
				continue
			}
			if needTrans {
				transLines = append(transLines, ln.Text)
				transRefs = append(transRefs, lineRef{item: i, origIdx: j})
			}
			if needRoma {
				romaLines = append(romaLines, ln.Text)
				romaRefs = append(romaRefs, lineRef{item: i, origIdx: j})
			}
		}
	}

	// 一次（分块顺序的）翻译请求，覆盖本批所有需要翻译的行。
	if len(transLines) > 0 {
		out, err := llmTransform(translateSystemPrompt, transLines)
		if err != nil {
			return results, err
		}
		if out != nil {
			byItem := make(map[int]map[int]string)
			for k, ref := range transRefs {
				if byItem[ref.item] == nil {
					byItem[ref.item] = make(map[int]string)
				}
				byItem[ref.item][ref.origIdx] = strings.TrimSpace(out[k])
			}
			for item, trans := range byItem {
				results[item].Lyric = buildBlended(parsed[item].original, trans)
				results[item].Changed = true
			}
		}
	}

	// 一次（分块顺序的）罗马音请求，覆盖本批所有需要罗马音的行。
	if len(romaLines) > 0 {
		out, err := llmTransform(romajiSystemPrompt, romaLines)
		if err != nil {
			return results, err
		}
		if out != nil {
			byItem := make(map[int][]LyricLine)
			for k, ref := range romaRefs {
				r := strings.TrimSpace(out[k])
				if r == "" {
					continue
				}
				byItem[ref.item] = append(byItem[ref.item], LyricLine{
					Timestamp: parsed[ref.item].original[ref.origIdx].Timestamp,
					Text:      r,
				})
			}
			for i := range results {
				lines, ok := byItem[i]
				if !ok {
					continue
				}
				var sb strings.Builder
				for _, ln := range lines {
					sb.WriteString(ln.Timestamp)
					sb.WriteString(ln.Text)
					sb.WriteString("\n")
				}
				results[i].Romaji = strings.TrimRight(sb.String(), "\n")
				results[i].Changed = true
			}
		}
	}

	return results, nil
}

// CompleteLyric 补全单条歌词，等价于对单元素切片调用 CompleteLyricsBatch。
func CompleteLyric(lyric, romaji string) (newLyric string, newRomaji string, changed bool, err error) {
	res, err := CompleteLyricsBatch([]CompletionInput{{Lyric: lyric, Romaji: romaji}})
	if len(res) == 0 {
		return lyric, romaji, false, err
	}
	return res[0].Lyric, res[0].Romaji, res[0].Changed, err
}
