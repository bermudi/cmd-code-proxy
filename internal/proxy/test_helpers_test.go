package proxy

import "strings"

func extractSSEData(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}
