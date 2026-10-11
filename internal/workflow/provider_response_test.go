package workflow

import (
	"context"
	"errors"
	"testing"

	"subsyncd/internal/provider"
)

func TestProviderMessagesAreNotSubtitleRejections(t *testing.T) {
	adapter := &fakeProvider{id: "provider",
		payloads: map[string][]byte{
			"html":        []byte("\ufeff  <!DOCTYPE html><html><body>Please log in</body></html>"),
			"typed-html":  []byte("<p>Daily download limit reached</p>"),
			"json":        []byte(`{"status":false,"error":"download limit"}`),
			"placeholder": workflowZIP(t, map[string]string{"titlovi_titl_plugin_message.srt": installSRT}),
		},
		contentTypes: map[string]string{"typed-html": "text/html; charset=utf-8", "json": "application/json"},
	}
	service := &Service{Providers: map[string]provider.Provider{"provider": adapter}}
	for _, id := range []string{"html", "typed-html", "json", "placeholder"} {
		_, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate(id), t.TempDir(), 0)
		var message *ProviderResponseError
		if !errors.As(err, &message) || rejectionReasonCode(err) != "" {
			t.Errorf("%s: error = %T %v; want a provider error that is not a rejection", id, err, err)
		}
	}
}

func TestSubtitlesThatLookLikeMessagesAreStillRead(t *testing.T) {
	adapter := &fakeProvider{id: "provider",
		payloads: map[string][]byte{
			"microdvd": []byte("{1}{1}25.000\n{25}{50}Hello\n"),
			"italic":   []byte("<i>\n"),
			"pack":     workflowZIP(t, map[string]string{"Movie.srt": installSRT}),
		},
		filenames: map[string]string{"microdvd": "Movie.sub", "italic": "Movie.srt", "pack": "Movie.zip"},
	}
	service := &Service{Providers: map[string]provider.Provider{"provider": adapter}}
	for _, id := range []string{"microdvd", "pack"} {
		if _, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate(id), t.TempDir(), 0); err != nil {
			t.Errorf("%s: error = %v", id, err)
		}
	}
	// A subtitle starting with a tag is judged as a subtitle, not a page.
	_, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate("italic"), t.TempDir(), 0)
	var message *ProviderResponseError
	if errors.As(err, &message) {
		t.Fatalf("error = %v; a tag in a subtitle is not an HTML page", err)
	}
}
