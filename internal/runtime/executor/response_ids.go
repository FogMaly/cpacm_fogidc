package executor

import (
	"bytes"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

func extractOpenAIResponseIDs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	add := func(payload []byte) {
		for _, path := range []string{"response.id", "id"} {
			responseID := strings.TrimSpace(gjson.GetBytes(payload, path).String())
			if responseID == "" {
				continue
			}
			seen[responseID] = struct{}{}
			return
		}
	}

	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		add(payload)
	}
	if len(seen) == 0 {
		add(raw)
	}
	if len(seen) == 0 {
		return nil
	}

	out := make([]string, 0, len(seen))
	for responseID := range seen {
		out = append(out, responseID)
	}
	sort.Strings(out)
	return out
}
