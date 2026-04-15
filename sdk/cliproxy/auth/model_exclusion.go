package auth

import "strings"

func authExcludedModelPatterns(auth *Auth) []string {
	if auth == nil || len(auth.Attributes) == 0 {
		return nil
	}
	raw := strings.TrimSpace(auth.Attributes["excluded_models"])
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		pattern := strings.ToLower(strings.TrimSpace(part))
		if pattern != "" {
			out = append(out, pattern)
		}
	}
	return out
}

func authExcludesAllModels(auth *Auth) bool {
	for _, pattern := range authExcludedModelPatterns(auth) {
		if pattern == "*" {
			return true
		}
	}
	return false
}

func authExcludesModel(auth *Auth, models ...string) bool {
	patterns := authExcludedModelPatterns(auth)
	if len(patterns) == 0 {
		return false
	}
	candidates := make(map[string]struct{}, len(models)*4)
	for _, model := range models {
		appendModelExclusionCandidates(candidates, auth, model)
	}
	for _, pattern := range patterns {
		if pattern == "*" {
			return true
		}
		for candidate := range candidates {
			if matchExcludedModelPattern(pattern, candidate) {
				return true
			}
		}
	}
	return false
}

func appendModelExclusionCandidates(dst map[string]struct{}, auth *Auth, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		dst[value] = struct{}{}
		if canonical := strings.ToLower(strings.TrimSpace(canonicalModelKey(value))); canonical != "" {
			dst[canonical] = struct{}{}
		}
		if prefix := strings.Trim(strings.ToLower(strings.TrimSpace(authPrefix(auth))), "/"); prefix != "" && !strings.Contains(value, "/") {
			dst[prefix+"/"+value] = struct{}{}
		}
	}
	add(model)
	if idx := strings.Index(model, "/"); idx >= 0 && idx < len(model)-1 {
		add(model[idx+1:])
	}
}

func authPrefix(auth *Auth) string {
	if auth == nil {
		return ""
	}
	return auth.Prefix
}

func matchExcludedModelPattern(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = strings.ToLower(strings.TrimSpace(model))
	if pattern == "" || model == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	pi := 0
	si := 0
	star := -1
	match := 0
	for si < len(model) {
		if pi < len(pattern) && pattern[pi] == model[si] {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			match = si
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			match++
			si = match
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
