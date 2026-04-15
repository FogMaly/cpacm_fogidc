package executor

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexCache struct {
	ID             string
	PromptCacheKey string
	ConversationID string
	SessionID      string
	Expire         time.Time
}

type codexBridgeReplayState struct {
	Payload []byte
	Expire  time.Time
	Stored  time.Time
}

// codexCacheMap stores prompt cache IDs keyed by model+user_id.
// Protected by codexCacheMu. Entries expire after 1 hour.
var (
	codexCacheMap        = make(map[string]codexCache)
	codexBridgeReplayMap = make(map[string]codexBridgeReplayState)
	codexBridgeReplayB   int64
	codexCacheMu         sync.RWMutex
)

// codexCacheCleanupInterval controls how often expired entries are purged.
const codexCacheCleanupInterval = 15 * time.Minute

var (
	codexBridgeReplayMaxEntries   = 512
	codexBridgeReplayMaxTotalSize = 256 * 1024 * 1024
	codexBridgeReplayMaxEntrySize = 1024 * 1024
)

// codexCacheCleanupOnce ensures the background cleanup goroutine starts only once.
var codexCacheCleanupOnce sync.Once

// startCodexCacheCleanup launches a background goroutine that periodically
// removes expired entries from codexCacheMap to prevent memory leaks.
func startCodexCacheCleanup() {
	go func() {
		ticker := time.NewTicker(codexCacheCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredCodexCache()
		}
	}()
}

// purgeExpiredCodexCache removes entries that have expired.
func purgeExpiredCodexCache() {
	now := time.Now()
	codexCacheMu.Lock()
	defer codexCacheMu.Unlock()
	for key, cache := range codexCacheMap {
		if cache.Expire.Before(now) {
			delete(codexCacheMap, key)
		}
	}
	for key, state := range codexBridgeReplayMap {
		if state.Expire.Before(now) {
			deleteCodexBridgeReplayLocked(key)
		}
	}
}

func deleteCodexBridgeReplayLocked(key string) {
	state, ok := codexBridgeReplayMap[key]
	if !ok {
		return
	}
	codexBridgeReplayB -= int64(len(state.Payload))
	if codexBridgeReplayB < 0 {
		codexBridgeReplayB = 0
	}
	delete(codexBridgeReplayMap, key)
}

func enforceCodexBridgeReplayBoundsLocked() {
	for {
		overEntries := codexBridgeReplayMaxEntries > 0 && len(codexBridgeReplayMap) > codexBridgeReplayMaxEntries
		overSize := codexBridgeReplayMaxTotalSize > 0 && codexBridgeReplayB > int64(codexBridgeReplayMaxTotalSize)
		if !overEntries && !overSize {
			return
		}

		oldestKey := ""
		var oldestAt time.Time
		for key, state := range codexBridgeReplayMap {
			at := state.Stored
			if at.IsZero() {
				at = state.Expire
			}
			if oldestKey == "" || at.Before(oldestAt) {
				oldestKey = key
				oldestAt = at
			}
		}
		if oldestKey == "" {
			codexBridgeReplayB = 0
			return
		}
		deleteCodexBridgeReplayLocked(oldestKey)
	}
}

// getCodexCache retrieves a cached entry, returning ok=false if not found or expired.
func getCodexCache(key string) (codexCache, bool) {
	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	codexCacheMu.RLock()
	cache, ok := codexCacheMap[key]
	codexCacheMu.RUnlock()
	if !ok || cache.Expire.Before(time.Now()) {
		return codexCache{}, false
	}
	return cache, true
}

// setCodexCache stores a cache entry.
func setCodexCache(key string, cache codexCache) {
	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	codexCacheMu.Lock()
	codexCacheMap[key] = cache
	codexCacheMu.Unlock()
}

func (c codexCache) requestPromptCacheKey() string {
	if strings.TrimSpace(c.PromptCacheKey) != "" {
		return strings.TrimSpace(c.PromptCacheKey)
	}
	return ""
}

func (c codexCache) requestConversationID() string {
	if strings.TrimSpace(c.ConversationID) != "" {
		return strings.TrimSpace(c.ConversationID)
	}
	if promptCacheKey := c.requestPromptCacheKey(); promptCacheKey != "" {
		return promptCacheKey
	}
	return strings.TrimSpace(c.ID)
}

func (c codexCache) requestSessionID() string {
	if strings.TrimSpace(c.SessionID) != "" {
		return strings.TrimSpace(c.SessionID)
	}
	if promptCacheKey := c.requestPromptCacheKey(); promptCacheKey != "" {
		return promptCacheKey
	}
	return strings.TrimSpace(c.ID)
}

func codexResponseCacheKey(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ""
	}
	return "response:" + strings.ToLower(responseID)
}

func getCodexResponseCache(responseID string) (codexCache, bool) {
	key := codexResponseCacheKey(responseID)
	if key == "" {
		return codexCache{}, false
	}
	return getCodexCache(key)
}

func setCodexResponseCache(responseIDs []string, cache codexCache) {
	if cache.ID == "" || len(responseIDs) == 0 {
		return
	}
	if cache.Expire.IsZero() {
		cache.Expire = time.Now().Add(1 * time.Hour)
	}
	for _, responseID := range responseIDs {
		key := codexResponseCacheKey(responseID)
		if key == "" {
			continue
		}
		setCodexCache(key, cache)
	}
}

func codexBridgeReplayKey(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ""
	}
	return "bridge-replay:" + strings.ToLower(responseID)
}

func getCodexBridgeReplay(responseID string) ([]byte, bool) {
	key := codexBridgeReplayKey(responseID)
	if key == "" {
		return nil, false
	}
	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	now := time.Now()
	codexCacheMu.RLock()
	state, ok := codexBridgeReplayMap[key]
	codexCacheMu.RUnlock()
	if !ok {
		return nil, false
	}
	if state.Expire.Before(now) || len(state.Payload) == 0 {
		codexCacheMu.Lock()
		if cur, exists := codexBridgeReplayMap[key]; exists {
			if cur.Expire.Before(now) || len(cur.Payload) == 0 {
				deleteCodexBridgeReplayLocked(key)
			}
		}
		codexCacheMu.Unlock()
		return nil, false
	}
	return bytes.Clone(state.Payload), true
}

func setCodexBridgeReplay(responseIDs []string, payload []byte, expire time.Time) {
	if len(responseIDs) == 0 || len(payload) == 0 {
		return
	}
	if codexBridgeReplayMaxEntrySize > 0 && len(payload) > codexBridgeReplayMaxEntrySize {
		return
	}
	if expire.IsZero() {
		expire = time.Now().Add(1 * time.Hour)
	}
	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	copied := bytes.Clone(payload)
	now := time.Now()
	codexCacheMu.Lock()
	for _, responseID := range responseIDs {
		key := codexBridgeReplayKey(responseID)
		if key == "" {
			continue
		}
		if _, exists := codexBridgeReplayMap[key]; exists {
			deleteCodexBridgeReplayLocked(key)
		}
		codexBridgeReplayMap[key] = codexBridgeReplayState{
			Payload: copied,
			Expire:  expire,
			Stored:  now,
		}
		codexBridgeReplayB += int64(len(copied))
	}
	enforceCodexBridgeReplayBoundsLocked()
	codexCacheMu.Unlock()
}

func normalizeOpenAIResponsesReplayRequest(raw []byte) []byte {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return raw
	}
	input := gjson.GetBytes(raw, "input")
	switch {
	case input.Type == gjson.String:
		text := input.String()
		message := `[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`
		message, _ = sjson.Set(message, "0.content.0.text", text)
		out, _ := sjson.SetRawBytes(raw, "input", []byte(message))
		return out
	case input.Exists() && input.IsArray():
		normalized := normalizeOpenAIResponsesReplayInput(input.Array())
		if normalized == "" {
			normalized = `[]`
		}
		out, err := sjson.SetRawBytes(raw, "input", []byte(normalized))
		if err == nil {
			return out
		}
	}
	return raw
}

func normalizeOpenAIResponsesReplayInput(items []gjson.Result) string {
	out := `[]`
	for _, item := range items {
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		role := strings.TrimSpace(item.Get("role").String())
		if itemType == "" && role != "" {
			itemType = "message"
		}

		switch itemType {
		case "message", "":
			normalized := normalizeOpenAIResponsesReplayMessage(item, role)
			if normalized == "" {
				continue
			}
			out, _ = sjson.SetRaw(out, "-1", normalized)
		default:
			out, _ = sjson.SetRaw(out, "-1", item.Raw)
		}
	}
	return out
}

func normalizeOpenAIResponsesReplayMessage(item gjson.Result, role string) string {
	content := item.Get("content")
	if content.Type == gjson.String {
		if strings.TrimSpace(content.String()) == "" {
			return ""
		}
		return item.Raw
	}
	if !content.Exists() || !content.IsArray() {
		return ""
	}

	out := item.Raw
	out, _ = sjson.Delete(out, "content")
	normalizedContent := `[]`
	added := 0
	content.ForEach(func(_, part gjson.Result) bool {
		normalized := normalizeOpenAIResponsesReplayContentPart(role, part)
		if normalized == "" {
			return true
		}
		normalizedContent, _ = sjson.SetRaw(normalizedContent, "-1", normalized)
		added++
		return true
	})
	if added == 0 {
		return ""
	}
	out, _ = sjson.SetRaw(out, "content", normalizedContent)
	return out
}

func normalizeOpenAIResponsesReplayContentPart(role string, part gjson.Result) string {
	partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
	switch partType {
	case "", "text", "input_text", "output_text", "summary_text", "refusal":
		return normalizeOpenAIResponsesReplayTextPart(role, part.Get("text").String())
	case "input_image":
		imageURL := strings.TrimSpace(part.Get("image_url").String())
		if imageURL == "" {
			imageURL = strings.TrimSpace(part.Get("image_url.url").String())
		}
		if imageURL == "" {
			return ""
		}
		out := `{"type":"input_image","image_url":""}`
		out, _ = sjson.Set(out, "image_url", imageURL)
		return out
	default:
		return normalizeOpenAIResponsesReplayTextPart(role, part.Get("text").String())
	}
}

func normalizeOpenAIResponsesReplayTextPart(role, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	partType := "input_text"
	if strings.EqualFold(strings.TrimSpace(role), "assistant") {
		partType = "output_text"
	}
	out := `{"type":"","text":""}`
	out, _ = sjson.Set(out, "type", partType)
	out, _ = sjson.Set(out, "text", text)
	return out
}

func extractOpenAIResponsesCompletedPayloads(raw []byte) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	if gjson.GetBytes(raw, "output").Exists() {
		return [][]byte{bytes.Clone(raw)}
	}
	if response := gjson.GetBytes(raw, "response"); response.Exists() && response.Get("output").Exists() {
		return [][]byte{[]byte(response.Raw)}
	}

	var out [][]byte
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		root := gjson.ParseBytes(payload)
		switch strings.TrimSpace(root.Get("type").String()) {
		case "response.completed":
			if response := root.Get("response"); response.Exists() && response.Get("output").Exists() {
				out = append(out, []byte(response.Raw))
			}
		default:
			if root.Get("output").Exists() {
				out = append(out, bytes.Clone(payload))
			}
		}
	}
	return out
}

func buildOpenAIResponsesReplayTemplate(requestRaw, responseRaw []byte) []byte {
	requestRaw = normalizeOpenAIResponsesReplayRequest(requestRaw)
	if len(requestRaw) == 0 || !gjson.ValidBytes(requestRaw) {
		return nil
	}
	root := gjson.ParseBytes(requestRaw)
	if !root.IsObject() {
		return nil
	}

	template := bytes.Clone(requestRaw)
	template, _ = sjson.DeleteBytes(template, "stream")
	template, _ = sjson.DeleteBytes(template, "previous_response_id")

	output := gjson.GetBytes(responseRaw, "output")
	if !output.Exists() || !output.IsArray() {
		return template
	}

	combined := `[]`
	if input := gjson.GetBytes(template, "input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			combined, _ = sjson.SetRaw(combined, "-1", item.Raw)
			return true
		})
	}
	output.ForEach(func(_, item gjson.Result) bool {
		switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
		case "message", "function_call", "custom_tool_call":
			normalized := normalizeOpenAIResponsesReplayOutputItem(item)
			if normalized == "" {
				return true
			}
			combined, _ = sjson.SetRaw(combined, "-1", normalized)
		}
		return true
	})

	template, _ = sjson.SetRawBytes(template, "input", []byte(combined))
	return template
}

func mergeOpenAIResponsesReplayTemplate(previousTemplate, currentRequest []byte) ([]byte, bool) {
	previousTemplate = normalizeOpenAIResponsesReplayRequest(previousTemplate)
	currentRequest = normalizeOpenAIResponsesReplayRequest(currentRequest)
	if len(previousTemplate) == 0 || !gjson.ValidBytes(previousTemplate) {
		return nil, false
	}
	if len(currentRequest) == 0 || !gjson.ValidBytes(currentRequest) {
		return bytes.Clone(previousTemplate), true
	}

	out := bytes.Clone(previousTemplate)
	currentRoot := gjson.ParseBytes(currentRequest)
	if !currentRoot.IsObject() {
		return out, true
	}
	currentHasFunctionCallOutput := false
	if input := gjson.GetBytes(currentRequest, "input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "function_call_output") {
				currentHasFunctionCallOutput = true
				return false
			}
			return true
		})
	}
	currentRoot.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" || name == "input" {
			return true
		}
		out, _ = sjson.SetRawBytes(out, name, []byte(value.Raw))
		return true
	})
	if currentHasFunctionCallOutput && !currentRoot.Get("tool_choice").Exists() {
		// OpenAI continuations commonly omit tool_choice on the tool-output turn.
		// Reusing a prior "required"/specific tool choice can incorrectly force
		// another tool call instead of letting the model produce the final answer.
		out, _ = sjson.DeleteBytes(out, "tool_choice")
	}

	combined := `[]`
	if input := gjson.GetBytes(previousTemplate, "input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			combined, _ = sjson.SetRaw(combined, "-1", item.Raw)
			return true
		})
	}
	if input := gjson.GetBytes(currentRequest, "input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			combined, _ = sjson.SetRaw(combined, "-1", item.Raw)
			return true
		})
	}
	out, _ = sjson.SetRawBytes(out, "input", []byte(combined))
	return out, true
}

func normalizeOpenAIResponsesReplayOutputItem(item gjson.Result) string {
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "message":
		role := strings.TrimSpace(item.Get("role").String())
		if role == "" {
			role = "assistant"
		}
		minimal := `{"type":"message","role":"","content":[]}`
		minimal, _ = sjson.Set(minimal, "role", role)
		if normalized := normalizeOpenAIResponsesReplayMessage(gjson.Parse(minimalWithContent(item, minimal)), role); normalized != "" {
			return normalized
		}
	case "function_call":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			callID = strings.TrimSpace(item.Get("id").String())
		}
		name := strings.TrimSpace(item.Get("name").String())
		arguments := item.Get("arguments").String()
		out := `{"type":"function_call","call_id":"","name":"","arguments":""}`
		out, _ = sjson.Set(out, "call_id", callID)
		out, _ = sjson.Set(out, "name", name)
		out, _ = sjson.Set(out, "arguments", arguments)
		return out
	case "custom_tool_call":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			callID = strings.TrimSpace(item.Get("id").String())
		}
		name := strings.TrimSpace(item.Get("name").String())
		input := item.Get("input").String()
		out := `{"type":"custom_tool_call","call_id":"","name":"","input":""}`
		out, _ = sjson.Set(out, "call_id", callID)
		out, _ = sjson.Set(out, "name", name)
		out, _ = sjson.Set(out, "input", input)
		return out
	}
	return ""
}

func minimalWithContent(item gjson.Result, minimal string) string {
	content := item.Get("content")
	if !content.Exists() {
		return minimal
	}
	out, err := sjson.SetRaw(minimal, "content", content.Raw)
	if err != nil {
		return minimal
	}
	return out
}
