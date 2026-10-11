package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"subsyncd/internal/inventory"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

func TestLapseVerdictOnUnreadableMediaDefersInsteadOfRejecting(t *testing.T) {
	ctx := context.Background()
	request := serviceRequest(t)
	request.Media.EntityID = 1
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	request.MediaID, _, err = db.Repository().UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	repository := &countingRejections{WorkflowRepository: db.Repository()}
	service.Repository = repository
	candidate := broadCandidate("unheard")
	// The mount dropped while LAPSE ran: it heard nothing in a file it could
	// not read.
	if err := os.Remove(request.Media.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []string{"nothing", "unsure"} {
		recorded, err := service.recordCandidateRejection(ctx, request, candidate, "checksum", &syncer.VerdictError{Verdict: verdict})
		if recorded || !MediaUnavailable(err) {
			t.Fatalf("%s verdict on missing media: recorded=%t err=%v, want a media-unavailable deferral", verdict, recorded, err)
		}
	}
	if repository.puts != 0 {
		t.Fatalf("recorded %d rejections, want none", repository.puts)
	}
}

type countingRejections struct {
	WorkflowRepository
	puts int
}

func (r *countingRejections) PutCandidateRejection(ctx context.Context, rejection store.CandidateRejection) error {
	r.puts++
	return r.WorkflowRepository.PutCandidateRejection(ctx, rejection)
}
