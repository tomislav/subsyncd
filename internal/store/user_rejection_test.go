package store

import (
	"testing"
	"time"
)

// userRejection is the owner's rejection of an installed candidate.
func userRejection(mediaID int64, signature string, now time.Time) CandidateRejection {
	media := testMedia()
	return CandidateRejection{MediaID: mediaID, Language: "en", ProviderID: "alpha", ResultID: "bad", CandidateSignature: signature, ArtifactChecksum: "artifact", ReasonCode: "user_out_of_sync", ToolSignature: "lapse-1/policy-a",
		MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano(), RejectedAt: now}
}

func userLookup(mediaID int64, signature, tool string) CandidateRejectionLookup {
	media := testMedia()
	return CandidateRejectionLookup{MediaID: mediaID, Language: "en", ProviderID: "alpha", ResultID: "bad", CandidateSignature: signature, ToolSignature: tool,
		MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
}

func TestRecordUserRejectionQueuesReplaceSearch(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now.Add(30*24*time.Hour), SearchPriorityUpgrade); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordUserRejection(ctx, userRejection(mediaID, "signature", now), now); err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "en")
	if err != nil || status.State != "pending" || !status.NextAttemptAt.Equal(now) || status.Priority != SearchPriorityImport || !status.ReplaceInstalled {
		t.Fatalf("status = %+v/%v", status, err)
	}
	rejections, err := repo.ListCandidateRejections(ctx, mediaID, "en", now)
	if err != nil || len(rejections) != 1 || rejections[0].ReasonCode != "user_out_of_sync" {
		t.Fatalf("rejections = %+v/%v", rejections, err)
	}
	lease := leaseOne(t, repo, now)
	if !lease.ReplaceInstalled {
		t.Fatalf("lease = %+v, want ReplaceInstalled", lease)
	}
	// A completion that does not finish the replacement keeps the flag; one
	// that does clears it.
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "throttled", NextAttemptAt: now}); err != nil {
		t.Fatal(err)
	}
	if status, _ := repo.GetSearchStatus(ctx, mediaID, "en"); !status.ReplaceInstalled {
		t.Fatal("a throttled completion cleared replace_installed")
	}
	lease = leaseOne(t, repo, now)
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "installed", ClearReplaceInstalled: true}); err != nil {
		t.Fatal(err)
	}
	if status, _ := repo.GetSearchStatus(ctx, mediaID, "en"); status.ReplaceInstalled {
		t.Fatal("the finishing completion kept replace_installed")
	}
}

func TestRecordUserRejectionDuringLeaseRequestsRerun(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	lease := leaseOne(t, repo, now)
	if err := repo.RecordUserRejection(ctx, userRejection(mediaID, "signature", now), now); err != nil {
		t.Fatal(err)
	}
	// The running worker's completion must not settle the reject's search.
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "satisfied", ClearReplaceInstalled: true}); err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "en")
	if err != nil || status.State != "pending" || !status.ReplaceInstalled {
		t.Fatalf("status = %+v/%v", status, err)
	}
}

func TestUserRejectionMatchesAcrossToolSignatures(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordUserRejection(ctx, userRejection(mediaID, "signature", now), now); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.GetCandidateRejection(ctx, userLookup(mediaID, "signature", "lapse-2/policy-b")); err != nil || !found {
		t.Fatalf("user rejection under a new tool signature = %v/%v", found, err)
	}
	if _, found, err := repo.GetCandidateRejection(ctx, userLookup(mediaID, "other-signature", "lapse-1/policy-a")); err != nil || found {
		t.Fatalf("user rejection matched another candidate signature = %v/%v", found, err)
	}
	replaced := userLookup(mediaID, "signature", "lapse-1/policy-a")
	replaced.MediaSize++
	if _, found, err := repo.GetCandidateRejection(ctx, replaced); err != nil || found {
		t.Fatalf("user rejection matched a replaced media file = %v/%v", found, err)
	}
	// An installation from before the candidate signature was recorded is
	// rejected by provider and result ID: any signature matches.
	other := userRejection(mediaID, AnyCandidateSignature, now)
	other.ResultID = "legacy"
	if err := repo.RecordUserRejection(ctx, other, now); err != nil {
		t.Fatal(err)
	}
	lookup := userLookup(mediaID, "whatever/member-abc", "lapse-9/policy-z")
	lookup.ResultID = "legacy"
	if _, found, err := repo.GetCandidateRejection(ctx, lookup); err != nil || !found {
		t.Fatalf("legacy user rejection = %v/%v", found, err)
	}
	// A system rejection of the same key never overwrites the owner's reason.
	system := userRejection(mediaID, "signature", now)
	system.ReasonCode = "lapse_unsure"
	if err := repo.PutCandidateRejection(ctx, system); err != nil {
		t.Fatal(err)
	}
	if got, found, err := repo.GetCandidateRejection(ctx, userLookup(mediaID, "signature", "lapse-1/policy-a")); err != nil || !found || got.ReasonCode != "user_out_of_sync" {
		t.Fatalf("after system rejection = %+v/%v/%v", got, found, err)
	}
}

func TestClearCandidateRejectionsKeepsUserRejections(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordUserRejection(ctx, userRejection(mediaID, "signature", now), now); err != nil {
		t.Fatal(err)
	}
	system := userRejection(mediaID, "signature", now)
	system.ResultID, system.ReasonCode = "system", "lapse_unsure"
	if err := repo.PutCandidateRejection(ctx, system); err != nil {
		t.Fatal(err)
	}
	// "userx" is not a user reason: the prefix is matched literally.
	lookalike := system
	lookalike.ResultID, lookalike.ReasonCode = "lookalike", "userxreason"
	if err := repo.PutCandidateRejection(ctx, lookalike); err != nil {
		t.Fatal(err)
	}
	if err := repo.ClearCandidateRejections(ctx, mediaID, "en"); err != nil {
		t.Fatal(err)
	}
	rejections, err := repo.ListCandidateRejections(ctx, mediaID, "en", now)
	if err != nil || len(rejections) != 1 || rejections[0].ResultID != "bad" {
		t.Fatalf("rejections after retry-rejected = %+v/%v", rejections, err)
	}
	if err := repo.PutCandidateRejection(ctx, system); err != nil {
		t.Fatal(err)
	}
	cleared, err := repo.ClearUserRejections(ctx, mediaID, "en")
	if err != nil || cleared != 1 {
		t.Fatalf("ClearUserRejections() = %d/%v", cleared, err)
	}
	rejections, err = repo.ListCandidateRejections(ctx, mediaID, "en", now)
	if err != nil || len(rejections) != 1 || rejections[0].ResultID != "system" {
		t.Fatalf("rejections after undo = %+v/%v", rejections, err)
	}
}

func TestRemoveInstallationIsOneTransaction(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/show.en.srt", Checksum: "sum", ProviderID: "alpha", CandidateID: "bad",
		MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	request := NotificationRequest{Notifier: "silo", DedupeKey: "removed-key", PayloadJSON: []byte(`{}`), NextAttemptAt: now}
	removal := InstallationRemoval{Installation: installation, History: SearchHistoryDetail{MediaTitle: "Show - S01E02", Manual: true}, At: now, Notifications: []NotificationRequest{request}}

	// A record that changed since it was read is not removed.
	stale := removal
	stale.Installation.Checksum = "other"
	if _, err := repo.RemoveInstallation(ctx, stale); err == nil {
		t.Fatal("removal of a changed installation succeeded")
	}
	if _, found, _ := repo.GetInstallation(ctx, mediaID, "en"); !found {
		t.Fatal("failed removal deleted the installation")
	}
	enqueued, err := repo.RemoveInstallation(ctx, removal)
	if err != nil || len(enqueued) != 1 || !enqueued[0].Inserted {
		t.Fatalf("RemoveInstallation() = %+v/%v", enqueued, err)
	}
	if _, found, _ := repo.GetInstallation(ctx, mediaID, "en"); found {
		t.Fatal("installation remains")
	}
	entries, err := repo.ListHistory(ctx, HistoryQuery{})
	if err != nil || len(entries) != 1 || entries[0].Outcome != "removed" || !entries[0].Manual {
		t.Fatalf("history = %+v/%v", entries, err)
	}
	leases, err := repo.LeaseDueNotifications(ctx, now, 10, time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("notifications = %+v/%v", leases, err)
	}
	languages, err := repo.InstalledLanguages(ctx, mediaID)
	if err != nil || len(languages) != 0 {
		t.Fatalf("installed languages = %v/%v", languages, err)
	}
}
