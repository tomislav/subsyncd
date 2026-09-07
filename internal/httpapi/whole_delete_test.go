package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/store"
)

func TestWholeDeletionHTTPAcceptsAndDeduplicates(t *testing.T) {
	for _, test := range []struct{ kind, body string }{
		{"radarr", `{"eventType":"MovieDelete","movie":{"id":42},"deletedFiles":true}`},
		{"sonarr", `{"eventType":"SeriesDelete","series":{"id":42},"deletedFiles":true}`},
	} {
		t.Run(test.kind, func(t *testing.T) {
			db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Repository().EnsureInstance(t.Context(), "main", test.kind, "http://arr.invalid", time.Now()); err != nil {
				t.Fatal(err)
			}
			wakes := 0
			handler := catalog.WebhookHandler{Instance: "main", InstanceType: test.kind, Store: db.Repository(), Now: time.Now, OnApplied: func() { wakes++ }}
			server := Server{Instances: map[string]Instance{"main": {Token: "secret", Handler: handler}}}
			for i := 0; i < 2; i++ {
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/webhooks/main?token=secret", strings.NewReader(test.body)))
				if response.Code != http.StatusNoContent {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			}
			if wakes != 1 {
				t.Fatalf("wakes=%d", wakes)
			}
		})
	}
}
