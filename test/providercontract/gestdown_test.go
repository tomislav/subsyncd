//go:build provider_contract

package providercontract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/provider/gestdown"
)

// Gestdown needs no credentials, so a separate explicit opt-in protects ordinary
// and tagged CI runs from accidentally contacting the public service.
func TestGestdownLiveSearchDownload(t *testing.T) {
	if os.Getenv("GESTDOWN_LIVE_TEST") != "1" {
		t.Skip("set GESTDOWN_LIVE_TEST=1 to contact the public API")
	}
	adapter := build(t, "gestdown-contract", gestdown.Factory, map[string]string{"type": "gestdown"})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Breaking Bad", Season: 1, Episode: 1, ExternalIDs: domain.ExternalIDs{TVDB: 81189}}
	results, err := adapter.Search(ctx, provider.SearchQuery{Mode: provider.SearchBroad, Language: "en", Media: media})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range results {
		if candidate.HearingImpaired {
			continue
		}
		var download bytes.Buffer
		_, err := adapter.Download(ctx, candidate, &download)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := pack.Extract(ctx, candidate, bytes.NewReader(download.Bytes()), int64(download.Len()), filepath.Join(t.TempDir(), "extracted"), pack.DefaultLimits())
		if err != nil {
			t.Fatalf("real download failed extraction/validation: %v", err)
		}
		if _, err := pack.SelectSingleEpisode(manifest, candidate, media, false); err != nil {
			t.Fatalf("real download failed episode selection: %v", err)
		}
		t.Logf("live API: %d candidates; downloaded and validated %d bytes, SHA-256 %x", len(results), download.Len(), sha256.Sum256(download.Bytes()))
		return
	}
	t.Fatal("public API returned no completed non-HI English subtitles")
}
