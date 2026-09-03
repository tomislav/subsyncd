package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"subsyncd/internal/domain"
)

type OwnedSidecar struct {
	Path     string
	Checksum string
}

var supportedSidecarExtensions = map[string]struct{}{
	".srt": {},
	".ass": {},
	".ssa": {},
	".vtt": {},
}

func ScanSidecars(mediaPath string, ownership map[domain.Language]OwnedSidecar) ([]Track, error) {
	directory := filepath.Dir(mediaPath)
	stem := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("scan sidecars: %w", err)
	}
	var tracks []Track
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if _, ok := supportedSidecarExtensions[extension]; !ok {
			continue
		}
		base := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if !strings.HasPrefix(base, stem+".") {
			continue
		}
		language, forced, sdh, ok := parseSidecarSuffix(strings.TrimPrefix(base, stem+"."))
		if !ok {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		checksum, err := checksumFile(path)
		if err != nil {
			return nil, fmt.Errorf("checksum sidecar %q: %w", path, err)
		}
		owned, managed := ownership[language]
		protected := !managed || filepath.Clean(owned.Path) != filepath.Clean(path) || owned.Checksum != checksum
		tracks = append(tracks, Track{Path: path, Language: language, Codec: strings.TrimPrefix(extension, "."), Forced: forced, SDH: sdh, Protected: protected, Checksum: checksum})
	}
	return tracks, nil
}

func parseSidecarSuffix(raw string) (domain.Language, bool, bool, bool) {
	tokens := strings.Split(raw, ".")
	if len(tokens) == 0 {
		return "", false, false, false
	}
	language, err := domain.ParseLanguage(tokens[0])
	if err != nil {
		return "", false, false, false
	}
	forced := false
	sdh := false
	for _, token := range tokens[1:] {
		switch strings.ToLower(token) {
		case "forced":
			forced = true
		case "sdh", "hi", "cc":
			sdh = true
		default:
			return "", false, false, false
		}
	}
	return language, forced, sdh, true
}

func checksumFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
