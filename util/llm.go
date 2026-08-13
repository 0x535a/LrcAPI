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

func CompleteLyric(lyric, romaji string) (newLyric string, newRomaji string, changed bool, err error) {
	newLyric, newRomaji = lyric, romaji
	if !LLMEnabled() {
		return newLyric, newRomaji, false, nil
	}

	original, translation := originalAndTranslation(lyric)
	lang := detectLang(joinOriginalText(original))

	needTrans := (lang == LangOther || lang == LangJapanese || lang == LangKorean) && !hasChineseTranslation(translation)
	needRoma := (lang == LangJapanese || lang == LangKorean) && !hasRomaji(romaji)
	if !needTrans && !needRoma {
		return newLyric, newRomaji, false, nil
	}

	var idxs []int
	var texts []string
	for i, ln := range original {
		if strings.TrimSpace(ln.Text) != "" {
			idxs = append(idxs, i)
			texts = append(texts, ln.Text)
		}
	}
	if len(texts) == 0 {
		return newLyric, newRomaji, false, nil
	}

	if needTrans {
		out, e := llmTransform(translateSystemPrompt, texts)
		if e != nil {
			return newLyric, newRomaji, changed, e
		}
		if out != nil {
			transByIdx := make(map[int]string, len(idxs))
			for k, i := range idxs {
				transByIdx[i] = strings.TrimSpace(out[k])
			}
			newLyric = buildBlended(original, transByIdx)
			changed = true
		}
	}

	if needRoma {
		out, e := llmTransform(romajiSystemPrompt, texts)
		if e != nil {
			return newLyric, newRomaji, changed, e
		}
		if out != nil {
			var sb strings.Builder
			for k, i := range idxs {
				r := strings.TrimSpace(out[k])
				if r == "" {
					continue
				}
				sb.WriteString(original[i].Timestamp)
				sb.WriteString(r)
				sb.WriteString("\n")
			}
			newRomaji = strings.TrimRight(sb.String(), "\n")
			changed = true
		}
	}

	return newLyric, newRomaji, changed, nil
}
