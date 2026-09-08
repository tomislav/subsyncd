package pack

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/asticode/go-astisub"
	"github.com/nwaples/rardecode/v2"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"

	"subsyncd/internal/domain"
)

var (
	zipMagic = []byte{'P', 'K', 3, 4}
	rarMagic = []byte{'R', 'a', 'r', '!', 0x1a, 0x07}
)

type ContentError struct{ Err error }

func (e *ContentError) Error() string { return "subtitle payload rejected: " + e.Err.Error() }
func (e *ContentError) Unwrap() error { return e.Err }

type archiveMember struct {
	name string
	mode fs.FileMode
	body []byte
}

func Extract(ctx context.Context, candidate domain.Candidate, src io.Reader, contentLength int64, dst string, limits Limits) (Manifest, error) {
	if err := validateLimits(limits); err != nil {
		return Manifest{}, err
	}
	if contentLength > limits.MaxCompressed {
		return Manifest{}, fmt.Errorf("compressed subtitle exceeds %d bytes", limits.MaxCompressed)
	}
	payload, err := io.ReadAll(io.LimitReader(src, limits.MaxCompressed+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read subtitle payload: %w", err)
	}
	if int64(len(payload)) > limits.MaxCompressed {
		return Manifest{}, fmt.Errorf("compressed subtitle exceeds %d bytes", limits.MaxCompressed)
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}

	members, err := decodePayload(ctx, payload, candidate, limits)
	if err != nil {
		if ctx.Err() != nil {
			return Manifest{}, ctx.Err()
		}
		return Manifest{}, &ContentError{Err: err}
	}
	manifest := Manifest{ProviderID: candidate.ProviderID, ResultID: candidate.ResultID, Language: candidate.Language, Checksum: checksum(payload), ArchiveType: payloadType(payload), Candidate: candidate}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return Manifest{}, fmt.Errorf("create extraction parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".subsyncd-extract-")
	if err != nil {
		return Manifest{}, fmt.Errorf("create extraction staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()

	seen := map[string]struct{}{}
	for _, raw := range members {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		safeName, err := safeMemberName(raw.name, limits.MaxDepth)
		if err != nil {
			return Manifest{}, &ContentError{Err: err}
		}
		lower := strings.ToLower(safeName)
		if _, exists := seen[lower]; exists {
			return Manifest{}, &ContentError{Err: fmt.Errorf("archive contains duplicate subtitle name %q", safeName)}
		}
		seen[lower] = struct{}{}
		text, extension, err := normalizeSubtitle(raw.body, filepath.Ext(safeName))
		if err != nil {
			return Manifest{}, &ContentError{Err: fmt.Errorf("validate subtitle %q: %w", safeName, err)}
		}
		if filepath.Ext(safeName) == "" {
			safeName += extension
		}
		path := filepath.Join(staging, safeName)
		if err := os.WriteFile(path, text, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("write normalized subtitle: %w", err)
		}
		member := memberEvidence(safeName, candidate.Title)
		member.NormalizedPath = path
		member.Checksum = checksum(text)
		member.ByteSize = int64(len(text))
		manifest.Members = append(manifest.Members, member)
	}
	if len(manifest.Members) == 0 {
		return Manifest{}, &ContentError{Err: fmt.Errorf("payload contains no supported subtitle files")}
	}
	if _, err := os.Lstat(dst); err == nil {
		return Manifest{}, fmt.Errorf("extraction destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("inspect extraction destination: %w", err)
	}
	if err := os.Rename(staging, dst); err != nil {
		return Manifest{}, fmt.Errorf("publish extracted subtitles: %w", err)
	}
	published = true
	for index := range manifest.Members {
		manifest.Members[index].NormalizedPath = filepath.Join(dst, manifest.Members[index].SafeName)
	}
	manifest.RuntimePack = IsRuntimePack(manifest)
	return manifest, nil
}

func payloadType(payload []byte) string {
	switch {
	case bytes.HasPrefix(payload, zipMagic):
		return "zip"
	case bytes.HasPrefix(payload, rarMagic):
		return "rar"
	default:
		return "plain"
	}
}

func validateLimits(limits Limits) error {
	if limits.MaxCompressed <= 0 || limits.MaxExpanded <= 0 || limits.MaxFiles <= 0 || limits.MaxDepth < 0 {
		return fmt.Errorf("archive limits must be positive")
	}
	return nil
}

func decodePayload(ctx context.Context, payload []byte, candidate domain.Candidate, limits Limits) ([]archiveMember, error) {
	switch {
	case bytes.HasPrefix(payload, zipMagic):
		return decodeZIP(ctx, payload, limits)
	case bytes.HasPrefix(payload, rarMagic):
		return decodeRAR(ctx, payload, limits)
	default:
		if int64(len(payload)) > limits.MaxExpanded {
			return nil, fmt.Errorf("plain subtitle exceeds %d expanded bytes", limits.MaxExpanded)
		}
		name := filepath.Base(strings.Split(candidate.DownloadRef, "?")[0])
		if name == "." || name == "/" || name == "" {
			name = "subtitle.srt"
		}
		return []archiveMember{{name: name, mode: 0o600, body: payload}}, nil
	}
}

func decodeZIP(ctx context.Context, payload []byte, limits Limits) ([]archiveMember, error) {
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return nil, fmt.Errorf("open ZIP archive: %w", err)
	}
	var members []archiveMember
	var declaredExpanded, actualExpanded int64
	files := 0
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateArchivePath(entry.Name, limits.MaxDepth); err != nil {
			return nil, err
		}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 || mode&os.ModeType != 0 && !mode.IsDir() {
			return nil, fmt.Errorf("archive member %q is not a regular file", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		files++
		if files > limits.MaxFiles {
			return nil, fmt.Errorf("archive contains more than %d files", limits.MaxFiles)
		}
		declaredExpanded += int64(entry.UncompressedSize64)
		if declaredExpanded > limits.MaxExpanded {
			return nil, fmt.Errorf("archive expands beyond %d bytes", limits.MaxExpanded)
		}
		if isArchiveExtension(entry.Name) {
			return nil, fmt.Errorf("nested archives are not allowed")
		}
		stream, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("open ZIP member: %w", err)
		}
		remaining := limits.MaxExpanded - actualExpanded
		body, readErr := io.ReadAll(io.LimitReader(stream, remaining+1))
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("read ZIP member")
		}
		if int64(len(body)) > remaining {
			return nil, fmt.Errorf("archive member expands beyond limit")
		}
		actualExpanded += int64(len(body))
		if actualExpanded > limits.MaxExpanded {
			return nil, fmt.Errorf("archive expands beyond %d bytes", limits.MaxExpanded)
		}
		if hasArchiveMagic(body) {
			return nil, fmt.Errorf("nested archives are not allowed")
		}
		if !isSubtitleExtension(entry.Name) {
			continue
		}
		members = append(members, archiveMember{name: entry.Name, mode: mode, body: body})
	}
	return members, nil
}

func decodeRAR(ctx context.Context, payload []byte, limits Limits) ([]archiveMember, error) {
	reader, err := rardecode.NewReader(&contextReader{ctx: ctx, reader: bytes.NewReader(payload)})
	if err != nil {
		return nil, fmt.Errorf("open RAR archive: %w", err)
	}
	var members []archiveMember
	var expanded int64
	files := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read RAR header: %w", err)
		}
		if err := validateArchivePath(header.Name, limits.MaxDepth); err != nil {
			return nil, err
		}
		if header.LinkType != 0 || header.Encrypted || header.HeaderEncrypted || header.Mode()&os.ModeType != 0 && !header.IsDir {
			return nil, fmt.Errorf("archive member %q is not a regular file", header.Name)
		}
		if header.IsDir {
			continue
		}
		files++
		if files > limits.MaxFiles {
			return nil, fmt.Errorf("archive contains more than %d files", limits.MaxFiles)
		}
		if !header.UnKnownSize {
			expanded += header.UnPackedSize
			if expanded > limits.MaxExpanded {
				return nil, fmt.Errorf("archive expands beyond %d bytes", limits.MaxExpanded)
			}
		}
		if isArchiveExtension(header.Name) {
			return nil, fmt.Errorf("nested archives are not allowed")
		}
		memberLimit := header.UnPackedSize
		if header.UnKnownSize {
			memberLimit = limits.MaxExpanded - expanded
		}
		body, err := io.ReadAll(io.LimitReader(reader, memberLimit+1))
		if err != nil || int64(len(body)) > memberLimit {
			return nil, fmt.Errorf("RAR member expands beyond limit")
		}
		if header.UnKnownSize {
			expanded += int64(len(body))
		}
		if hasArchiveMagic(body) {
			return nil, fmt.Errorf("nested archives are not allowed")
		}
		if !isSubtitleExtension(header.Name) {
			continue
		}
		members = append(members, archiveMember{name: header.Name, mode: header.Mode(), body: body})
	}
	return members, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(payload []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(payload)
}

func validateArchivePath(name string, maxDepth int) error {
	normalized := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(normalized, "/") || filepath.IsAbs(name) || len(normalized) >= 3 && ((normalized[0] >= 'A' && normalized[0] <= 'Z') || (normalized[0] >= 'a' && normalized[0] <= 'z')) && normalized[1] == ':' && normalized[2] == '/' {
		return fmt.Errorf("archive member has absolute path")
	}
	clean := filepath.ToSlash(filepath.Clean(normalized))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("archive member escapes extraction root")
	}
	depth := len(strings.Split(strings.Trim(clean, "/"), "/")) - 1
	if depth > maxDepth {
		return fmt.Errorf("archive member exceeds maximum depth")
	}
	return nil
}

func safeMemberName(name string, maxDepth int) (string, error) {
	if err := validateArchivePath(name, maxDepth); err != nil {
		return "", err
	}
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if base == "." || base == "" {
		return "", fmt.Errorf("archive member has no safe name")
	}
	return base, nil
}

func isSubtitleExtension(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt", ".ass", ".ssa", ".vtt":
		return true
	}
	return false
}

func isArchiveExtension(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz":
		return true
	}
	return false
}

func hasArchiveMagic(payload []byte) bool {
	return bytes.HasPrefix(payload, zipMagic) || bytes.HasPrefix(payload, rarMagic)
}

func normalizeSubtitle(payload []byte, extension string) ([]byte, string, error) {
	text, err := decodeText(payload)
	if err != nil {
		return nil, "", err
	}
	text = bytes.ReplaceAll(text, []byte("\r\n"), []byte("\n"))
	text = bytes.ReplaceAll(text, []byte("\r"), []byte("\n"))
	text = bytes.TrimPrefix(text, []byte{0xef, 0xbb, 0xbf})
	extension = strings.ToLower(extension)
	var subtitles *astisub.Subtitles
	parse := func(ext string) (*astisub.Subtitles, error) {
		switch ext {
		case ".srt":
			return astisub.ReadFromSRT(bytes.NewReader(text))
		case ".ass", ".ssa":
			return astisub.ReadFromSSAWithOptions(bytes.NewReader(text), astisub.SSAOptions{})
		case ".vtt":
			return astisub.ReadFromWebVTT(bytes.NewReader(text))
		default:
			return nil, fmt.Errorf("unsupported subtitle extension")
		}
	}
	if extension != "" {
		subtitles, err = parse(extension)
	} else {
		for _, attempt := range []string{".srt", ".vtt", ".ass"} {
			subtitles, err = parse(attempt)
			if err == nil && subtitles != nil && len(subtitles.Items) != 0 {
				extension = attempt
				break
			}
		}
	}
	if err != nil || subtitles == nil || len(subtitles.Items) == 0 {
		return nil, "", fmt.Errorf("subtitle syntax is invalid")
	}
	if len(subtitles.Items) > 100_000 {
		return nil, "", fmt.Errorf("subtitle contains too many cues")
	}
	previous := subtitles.Items[0].StartAt
	for _, item := range subtitles.Items {
		if item.StartAt < 0 || item.EndAt < item.StartAt || item.StartAt < previous {
			return nil, "", fmt.Errorf("subtitle timestamps are not nonnegative and monotonic")
		}
		previous = item.StartAt
	}
	return text, extension, nil
}

func decodeText(payload []byte) ([]byte, error) {
	if bytes.IndexByte(payload, 0) >= 0 {
		return nil, fmt.Errorf("subtitle contains NUL bytes")
	}
	if utf8.Valid(payload) {
		return append([]byte(nil), payload...), nil
	}
	decoded, _, err := transform.Bytes(charmap.Windows1250.NewDecoder(), payload)
	if err != nil || !utf8.Valid(decoded) {
		return nil, fmt.Errorf("subtitle encoding is unsupported")
	}
	return decoded, nil
}

func checksum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
