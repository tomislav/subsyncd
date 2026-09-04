package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func providerLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode provider log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func providerEvents(records []map[string]any, event string) []map[string]any {
	var matches []map[string]any
	for _, record := range records {
		if record["event"] == event {
			matches = append(matches, record)
		}
	}
	return matches
}
