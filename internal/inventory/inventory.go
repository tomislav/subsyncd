package inventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type Track struct {
	Index     int
	Path      string
	Language  domain.Language
	Codec     string
	Embedded  bool
	Forced    bool
	Default   bool
	SDH       bool
	Protected bool
	Checksum  string
}

type Inventory struct {
	Fingerprint domain.MediaFingerprint
	Tracks      []Track
}

func (i Inventory) Satisfies(language domain.Language, allowHI bool) bool {
	for _, track := range i.Tracks {
		if track.Language == "" || !domain.EquivalentLanguage(track.Language, language) || track.Forced {
			continue
		}
		if track.SDH && !allowHI {
			continue
		}
		return true
	}
	return false
}

type Repository interface {
	GetTrackInventory(context.Context, int64) (store.InventoryRecord, error)
	ReplaceTrackInventory(context.Context, int64, domain.MediaFingerprint, []store.TrackRecord) error
	GetInstallation(context.Context, int64, domain.Language) (store.Installation, bool, error)
}

type Service struct {
	Repository Repository
	Probe      Probe
}

func (s Service) Refresh(ctx context.Context, mediaID int64, media domain.Media, forceProbe bool) (Inventory, error) {
	info, err := os.Stat(media.Fingerprint.Path)
	if err != nil {
		return Inventory{}, fmt.Errorf("stat media: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Inventory{}, fmt.Errorf("media path %q is not a regular file", media.Fingerprint.Path)
	}
	fingerprint := domain.MediaFingerprint{Path: filepath.Clean(media.Fingerprint.Path), FileID: media.Ref.FileID, Size: info.Size(), ModTime: info.ModTime()}
	stored, err := s.Repository.GetTrackInventory(ctx, mediaID)
	if err != nil {
		return Inventory{}, fmt.Errorf("get cached inventory: %w", err)
	}

	var embedded []Track
	if !forceProbe && fingerprintsEqual(stored.Fingerprint, fingerprint) {
		for _, track := range stored.Tracks {
			if track.Embedded {
				embedded = append(embedded, fromStoreTrack(track))
			}
		}
	} else {
		embedded, err = s.Probe.Tracks(ctx, fingerprint.Path)
		if err != nil {
			return Inventory{}, err
		}
	}

	sidecars, err := ScanSidecars(fingerprint.Path, nil)
	if err != nil {
		return Inventory{}, err
	}
	for index := range sidecars {
		installation, exists, err := s.Repository.GetInstallation(ctx, mediaID, sidecars[index].Language)
		if err != nil {
			return Inventory{}, fmt.Errorf("get sidecar ownership: %w", err)
		}
		if exists && filepath.Clean(installation.Path) == filepath.Clean(sidecars[index].Path) && installation.Checksum == sidecars[index].Checksum {
			sidecars[index].Protected = false
		}
	}
	tracks := append(embedded, sidecars...)
	storedTracks := make([]store.TrackRecord, 0, len(tracks))
	for _, track := range tracks {
		storedTracks = append(storedTracks, toStoreTrack(mediaID, track))
	}
	if err := s.Repository.ReplaceTrackInventory(ctx, mediaID, fingerprint, storedTracks); err != nil {
		return Inventory{}, err
	}
	return Inventory{Fingerprint: fingerprint, Tracks: tracks}, nil
}

func fingerprintsEqual(a, b domain.MediaFingerprint) bool {
	return filepath.Clean(a.Path) == filepath.Clean(b.Path) && a.FileID == b.FileID && a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

func fromStoreTrack(track store.TrackRecord) Track {
	return Track{Index: track.Index, Path: track.Path, Language: domain.Language(track.Language), Codec: track.Codec, Embedded: track.Embedded, Forced: track.Forced, Default: track.Default, SDH: track.SDH, Protected: track.Protected, Checksum: track.Checksum}
}

func toStoreTrack(mediaID int64, track Track) store.TrackRecord {
	return store.TrackRecord{MediaID: mediaID, Index: track.Index, Path: track.Path, Language: track.Language.String(), Codec: track.Codec, Embedded: track.Embedded, Forced: track.Forced, Default: track.Default, SDH: track.SDH, Protected: track.Protected, Checksum: track.Checksum}
}
