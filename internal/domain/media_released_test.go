package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMediaJSONOmitsUnknownReleaseDate(t *testing.T) {
	data, err := json.Marshal(Media{Title: "Show"})
	if err != nil || strings.Contains(string(data), "released_at") {
		t.Fatalf("undated media JSON = %s err=%v", data, err)
	}
	data, err = json.Marshal(Media{Title: "Show", ReleasedAt: time.Date(2024, 4, 15, 0, 0, 0, 0, time.UTC)})
	if err != nil || !strings.Contains(string(data), `"released_at":"2024-04-15T00:00:00Z"`) {
		t.Fatalf("dated media JSON = %s err=%v", data, err)
	}
}
