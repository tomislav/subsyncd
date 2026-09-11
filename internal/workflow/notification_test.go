package workflow

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

func TestNotificationOutboxDistinguishesReplacementWithIdenticalSubtitle(t *testing.T) {
	for name, mutate := range map[string]func(*store.Installation){
		"file ID":              func(i *store.Installation) { i.MediaFileID++ },
		"media path":           func(i *store.Installation) { i.MediaPath = "/media/replacement.mkv" },
		"size":                 func(i *store.Installation) { i.MediaSize++ },
		"mtime":                func(i *store.Installation) { i.MediaModTimeNS++ },
		"subtitle destination": func(i *store.Installation) { i.Path = "/media/replacement.hr.srt" },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, filepath.Join(t.TempDir(), "outbox.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			original := store.Installation{MediaID: 1, Language: "hr", Checksum: "same-subtitle-content", Path: "/media/movie.hr.srt", MediaPath: "/media/movie.mkv", MediaFileID: 10, MediaSize: 100, MediaModTimeNS: 123}
			replacement := original
			mutate(&replacement)
			for index, installation := range []store.Installation{original, original, replacement, replacement} {
				requests, err := notificationRequests(domain.Media{}, installation, []string{"silo"}, time.Now().Add(time.Duration(index)*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				inserted, err := db.Repository().EnqueueNotification(ctx, requests[0])
				if err != nil {
					t.Fatal(err)
				}
				want := index == 0 || index == 2
				if inserted != want {
					t.Fatalf("installation %d: new notification = %v, want %v", index, inserted, want)
				}
			}
		})
	}
}
