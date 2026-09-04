package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

const (
	defaultPackTTL      = 24 * time.Hour
	defaultPackMaxBytes = 512 << 20
)

type cacheClock interface{ Now() time.Time }

type CachedMember struct {
	Path              string
	Checksum          string
	Candidate         domain.Candidate
	SelectionRule     string
	SelectionEvidence string
}

type Cache struct {
	root     string
	repo     *store.Repository
	clock    cacheClock
	maxBytes int64
	mu       sync.Mutex
}

func NewCache(root string, repository *store.Repository, clock cacheClock, maxBytes int64) (*Cache, error) {
	if root == "" || repository == nil || clock == nil {
		return nil, fmt.Errorf("pack cache root, repository, and clock are required")
	}
	if maxBytes <= 0 {
		maxBytes = defaultPackMaxBytes
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve pack cache root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create pack cache root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve pack cache symlinks: %w", err)
	}
	return &Cache{root: resolved, repo: repository, clock: clock, maxBytes: maxBytes}, nil
}

func (c *Cache) Find(ctx context.Context, media domain.Media, language domain.Language) (CachedMember, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, found, err := c.repo.GetReusablePackMember(ctx, store.PackLookup{SeriesIDs: media.ExternalIDs, SeriesTitle: media.Title, SeriesYear: media.Year, Season: media.Season, Episode: media.Episode, AbsoluteEpisode: media.AbsoluteEpisode, Language: language.String()}, c.clock.Now())
	if err != nil || !found {
		return CachedMember{}, found, err
	}
	if err := c.secureRegularPath(record.CachePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_ = c.invalidate(ctx, record)
			return CachedMember{}, false, nil
		}
		return CachedMember{}, false, err
	}
	payload, err := os.ReadFile(record.CachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_ = c.invalidate(ctx, record)
			return CachedMember{}, false, nil
		}
		return CachedMember{}, false, fmt.Errorf("read cached pack member: %w", err)
	}
	if checksum(payload) != record.Checksum {
		if err := c.invalidate(ctx, record); err != nil {
			return CachedMember{}, false, err
		}
		return CachedMember{}, false, nil
	}
	var candidate domain.Candidate
	if err := json.Unmarshal(record.CandidateJSON, &candidate); err != nil {
		if invalidateErr := c.invalidate(ctx, record); invalidateErr != nil {
			return CachedMember{}, false, invalidateErr
		}
		return CachedMember{}, false, nil
	}
	if err := c.repo.TouchPack(ctx, record.PackID, c.clock.Now()); err != nil {
		return CachedMember{}, false, err
	}
	rule, evidence := cachedEvidence(record, media)
	return CachedMember{Path: record.CachePath, Checksum: record.Checksum, Candidate: candidate, SelectionRule: rule, SelectionEvidence: evidence}, true, nil
}

func (c *Cache) Put(ctx context.Context, manifest Manifest, expiresAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if manifest.ProviderID == "" || manifest.ResultID == "" || manifest.Language == "" || manifest.Checksum == "" || len(manifest.Members) == 0 {
		return fmt.Errorf("pack manifest identity and members are required")
	}
	key := cacheContentKey(manifest)
	finalDirectory := filepath.Join(c.root, key)
	if err := c.ensureWithinRoot(finalDirectory); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(c.root, ".publish-")
	if err != nil {
		return fmt.Errorf("create pack cache staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	seen := map[string]struct{}{}
	storedMembers := make([]Member, 0, len(manifest.Members))
	var byteSize int64
	for _, member := range manifest.Members {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := filepath.Base(member.SafeName)
		if name == "." || name == "" || name != member.SafeName {
			return fmt.Errorf("pack member has unsafe cache name")
		}
		lower := strings.ToLower(name)
		if _, exists := seen[lower]; exists {
			return fmt.Errorf("pack manifest contains duplicate member %q", name)
		}
		seen[lower] = struct{}{}
		info, err := os.Lstat(member.NormalizedPath)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("pack source member is not a regular file")
		}
		payload, err := os.ReadFile(member.NormalizedPath)
		if err != nil {
			return fmt.Errorf("read pack source member: %w", err)
		}
		if checksum(payload) != member.Checksum {
			return fmt.Errorf("pack source member checksum changed")
		}
		if err := os.WriteFile(filepath.Join(staging, name), payload, 0o600); err != nil {
			return fmt.Errorf("stage pack member: %w", err)
		}
		member.NormalizedPath = filepath.Join(finalDirectory, name)
		member.ByteSize = int64(len(payload))
		byteSize += int64(len(payload))
		storedMembers = append(storedMembers, member)
	}

	sanitized := manifest
	sanitized.Candidate = sanitizeCandidate(manifest.Candidate)
	sanitized.Members = storedMembers
	manifestJSON, err := json.Marshal(sanitized)
	if err != nil {
		return fmt.Errorf("encode pack manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), manifestJSON, 0o600); err != nil {
		return fmt.Errorf("stage pack manifest: %w", err)
	}
	created := false
	if _, err := os.Lstat(finalDirectory); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(staging, finalDirectory); err != nil {
			return fmt.Errorf("publish pack cache: %w", err)
		}
		created = true
	} else if err != nil {
		return fmt.Errorf("inspect pack cache destination: %w", err)
	} else if err := verifyPublishedMembers(finalDirectory, storedMembers); err != nil {
		return err
	}

	now := c.clock.Now()
	if expiresAt.IsZero() {
		expiresAt = now.Add(defaultPackTTL)
	}
	candidateJSON, err := json.Marshal(sanitized.Candidate)
	if err != nil {
		if created {
			_ = os.RemoveAll(finalDirectory)
		}
		return fmt.Errorf("encode cached candidate: %w", err)
	}
	season := sanitized.Candidate.Season
	if sanitized.Candidate.Pack != nil && sanitized.Candidate.Pack.Season != 0 {
		season = sanitized.Candidate.Pack.Season
	}
	entry := store.PackCacheEntry{ProviderID: manifest.ProviderID, ResultID: manifest.ResultID, SeriesKey: store.StrongestSeriesKey(sanitized.Candidate.ExternalIDs, sanitized.Candidate.Title, sanitized.Candidate.Year), Season: season, Language: manifest.Language.String(), ContentChecksum: manifest.Checksum, ManifestPath: filepath.Join(finalDirectory, "manifest.json"), CandidateJSON: candidateJSON, ByteSize: byteSize, ExpiresAt: expiresAt, LastAccessAt: now}
	records := make([]store.PackMemberRecord, 0, len(storedMembers))
	for _, member := range storedMembers {
		records = append(records, store.PackMemberRecord{SafeName: member.SafeName, CachePath: member.NormalizedPath, Checksum: member.Checksum, Season: member.Season, EpisodeFrom: member.EpisodeFrom, EpisodeTo: member.EpisodeTo, AbsoluteFrom: member.AbsoluteFrom, AbsoluteTo: member.AbsoluteTo, NormalizedTitle: member.NormalizedTitle, Forced: member.Forced})
	}
	if err := c.repo.PutPack(ctx, entry, records); err != nil {
		if created {
			_ = os.RemoveAll(finalDirectory)
		}
		return err
	}
	return c.evictLocked(ctx, now)
}

func (c *Cache) Evict(ctx context.Context, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictLocked(ctx, now)
}

func (c *Cache) evictLocked(ctx context.Context, now time.Time) error {
	entries, err := c.repo.ListPacksForEviction(ctx, now)
	if err != nil {
		return err
	}
	var total int64
	for _, entry := range entries {
		total += entry.ByteSize
	}
	for _, entry := range entries {
		if entry.ExpiresAt.After(now) && total <= c.maxBytes {
			continue
		}
		directory, err := c.cacheDirectory(entry.ManifestPath)
		if err != nil {
			return err
		}
		tombstone := filepath.Join(c.root, fmt.Sprintf(".evict-%d", entry.ID))
		renamed := false
		if _, statErr := os.Lstat(directory); statErr == nil {
			if err := os.Rename(directory, tombstone); err != nil {
				return fmt.Errorf("stage pack eviction: %w", err)
			}
			renamed = true
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect pack eviction path: %w", statErr)
		}
		if err := c.repo.DeletePack(ctx, entry.ID); err != nil {
			if renamed {
				_ = os.Rename(tombstone, directory)
			}
			return err
		}
		if renamed {
			_ = os.RemoveAll(tombstone)
		}
		total -= entry.ByteSize
	}
	remaining, err := c.repo.ListPacksForEviction(ctx, now)
	if err != nil {
		return err
	}
	known := map[string]struct{}{}
	for _, entry := range remaining {
		directory, err := c.cacheDirectory(entry.ManifestPath)
		if err != nil {
			return err
		}
		known[directory] = struct{}{}
	}
	children, err := os.ReadDir(c.root)
	if err != nil {
		return fmt.Errorf("list pack cache root: %w", err)
	}
	for _, child := range children {
		path := filepath.Join(c.root, child.Name())
		if _, exists := known[path]; exists {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove orphaned pack cache entry: %w", err)
		}
	}
	return nil
}

func (c *Cache) invalidate(ctx context.Context, record store.PackMemberRecord) error {
	directory, err := c.cacheDirectory(record.CachePath)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove invalid pack cache: %w", err)
	}
	return c.repo.DeletePack(ctx, record.PackID)
}

func (c *Cache) secureRegularPath(path string) error {
	if _, err := c.cacheDirectory(path); err != nil {
		return err
	}
	relative, _ := filepath.Rel(c.root, path)
	current := c.root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("pack cache path contains a symlink")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cached pack member is not a regular file")
	}
	return nil
}

func (c *Cache) cacheDirectory(path string) (string, error) {
	if err := c.ensureWithinRoot(path); err != nil {
		return "", err
	}
	absolute, _ := filepath.Abs(path)
	relative, _ := filepath.Rel(c.root, absolute)
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || len(parts[0]) != sha256.Size*2 {
		return "", fmt.Errorf("pack cache path does not use the content-addressed layout")
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return "", fmt.Errorf("pack cache directory is not a content hash")
	}
	return filepath.Join(c.root, parts[0]), nil
}

func (c *Cache) ensureWithinRoot(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve pack cache path: %w", err)
	}
	relative, err := filepath.Rel(c.root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == "." {
		return fmt.Errorf("pack cache path is outside configured root")
	}
	return nil
}

func verifyPublishedMembers(directory string, members []Member) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("existing pack cache destination is not a real directory")
	}
	for _, member := range members {
		path := filepath.Join(directory, member.SafeName)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("existing pack cache member is not a regular file")
		}
		payload, err := os.ReadFile(path)
		if err != nil || checksum(payload) != member.Checksum {
			return fmt.Errorf("existing pack cache content does not match manifest")
		}
	}
	return nil
}

func sanitizeCandidate(candidate domain.Candidate) domain.Candidate {
	candidate.DownloadRef = ""
	if candidate.Pack != nil {
		pack := *candidate.Pack
		pack.DirectMembers = append([]domain.PackMemberRef(nil), candidate.Pack.DirectMembers...)
		for index := range pack.DirectMembers {
			pack.DirectMembers[index].DownloadRef = ""
		}
		candidate.Pack = &pack
	}
	return candidate
}

func cacheContentKey(manifest Manifest) string {
	value := sha256.Sum256([]byte(manifest.ProviderID + "\x00" + manifest.ResultID + "\x00" + manifest.Language.String() + "\x00" + manifest.Checksum))
	return hex.EncodeToString(value[:])
}

func cachedEvidence(record store.PackMemberRecord, media domain.Media) (string, string) {
	if record.EpisodeFrom > 0 && record.EpisodeFrom <= media.Episode && media.Episode <= record.EpisodeTo {
		return "cached_episode", fmt.Sprintf("cached member range %d-%d contains episode %d", record.EpisodeFrom, record.EpisodeTo, media.Episode)
	}
	return "cached_absolute_episode", fmt.Sprintf("cached member range %d-%d contains absolute episode %d", record.AbsoluteFrom, record.AbsoluteTo, media.AbsoluteEpisode)
}
