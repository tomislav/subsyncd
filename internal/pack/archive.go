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
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asticode/go-astisub"
	"github.com/nwaples/rardecode/v2"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"subsyncd/internal/domain"

	"subsyncd/internal/subtitleorder"
)

var (
	zipMagic = []byte{'P', 'K', 3, 4}
	rarMagic = []byte{'R', 'a', 'r', '!', 0x1a, 0x07}
)

// negativeTiming finds an SRT or WebVTT timing line with a negative time,
// which astisub reads as positive ("-00:00:05,000" as five seconds).
var negativeTiming = regexp.MustCompile(`(?m)^[\t ]*-[0-9].*-->|-->[\t ]*-[0-9]`)

// maxArchiveEntries bounds the files of any kind an archive may hold; only
// subtitle files count against Limits.MaxFiles, so a pack with scans, NFOs and
// samples beside its subtitles is still read.
const maxArchiveEntries = 10_000

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

	var subtitles []extractedMember
	var firstUnreadable error
	for _, raw := range members {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		if macOSMetadata(raw.name) {
			continue
		}
		safeName, err := safeMemberName(raw.name, limits.MaxDepth)
		if err != nil {
			return Manifest{}, &ContentError{Err: err}
		}
		memberExtension := filepath.Ext(safeName)
		var text []byte
		var extension string
		if strings.EqualFold(memberExtension, ".sub") {
			decoded, err := decodeSubtitle(raw.body)
			if err != nil || !isMicroDVD(decoded) {
				// A binary VobSub .sub (with its .idx) is not a text
				// subtitle; it is ignored like any other extra file.
				continue
			}
			converted, err := microDVDToSRT(decoded)
			if err != nil {
				// Nor is a MicroDVD file without a usable frame rate an
				// unreadable subtitle that makes selection doubt the rest:
				// it is ignored, and only explains an archive with nothing
				// else in it.
				if firstUnreadable == nil {
					firstUnreadable = fmt.Errorf("validate subtitle %q: %w", safeName, err)
				}
				continue
			}
			text, extension, err = validateSubtitle(converted, ".srt")
		} else {
			text, extension, err = normalizeSubtitle(raw.body, memberExtension)
		}
		if err != nil {
			// One unreadable file must not cost the other episodes in a
			// season pack; selection decides whether the target is here.
			manifest.SkippedMembers++
			if firstUnreadable == nil {
				firstUnreadable = fmt.Errorf("validate subtitle %q: %w", safeName, err)
			}
			continue
		}
		if filepath.Ext(safeName) == "" {
			safeName += extension
		} else if strings.EqualFold(filepath.Ext(safeName), ".sub") {
			// Movie.sub becomes Movie.sub.srt, so it cannot collide with a
			// Movie.srt shipped beside it.
			safeName += extension
		}
		subtitles = append(subtitles, extractedMember{path: strings.ReplaceAll(raw.name, `\`, "/"), name: safeName, text: text, sum: checksum(text)})
	}
	subtitles, err = assignMemberNames(subtitles)
	if err != nil {
		return Manifest{}, &ContentError{Err: err}
	}
	for _, subtitle := range subtitles {
		path := filepath.Join(staging, subtitle.name)
		if err := os.WriteFile(path, subtitle.text, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("write normalized subtitle: %w", err)
		}
		member := memberEvidence(subtitle.name, candidate.Title)
		member.NormalizedPath = path
		member.Checksum = subtitle.sum
		member.ByteSize = int64(len(subtitle.text))
		manifest.Members = append(manifest.Members, member)
	}
	if len(manifest.Members) == 0 {
		if firstUnreadable != nil {
			return Manifest{}, &ContentError{Err: firstUnreadable}
		}
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
	files, entries := 0, 0
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// macOS metadata is skipped before any path check: a __MACOSX copy
		// sits one folder deeper than the file it describes.
		if macOSMetadata(entry.Name) {
			continue
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
		if entries++; entries > maxArchiveEntries {
			return nil, fmt.Errorf("archive contains more than %d entries", maxArchiveEntries)
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
		if HasArchiveMagic(body) {
			return nil, fmt.Errorf("nested archives are not allowed")
		}
		if !isSubtitleExtension(entry.Name) {
			continue
		}
		if files++; files > limits.MaxFiles {
			return nil, fmt.Errorf("archive contains more than %d subtitle files", limits.MaxFiles)
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
	files, entries := 0, 0
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
		if macOSMetadata(header.Name) {
			continue
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
		if entries++; entries > maxArchiveEntries {
			return nil, fmt.Errorf("archive contains more than %d entries", maxArchiveEntries)
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
		if HasArchiveMagic(body) {
			return nil, fmt.Errorf("nested archives are not allowed")
		}
		if !isSubtitleExtension(header.Name) {
			continue
		}
		if files++; files > limits.MaxFiles {
			return nil, fmt.Errorf("archive contains more than %d subtitle files", limits.MaxFiles)
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

// extractedMember is a readable subtitle of an archive before its name in the
// extraction directory is settled.
type extractedMember struct {
	path string // the member's path inside the archive, with "/" separators
	name string
	text []byte
	sum  string
}

// assignMemberNames gives each subtitle a unique (case-insensitive) file name
// independent of the archive's member order. Identical copies under one name,
// as with the per-encoding ANSI/ and UTF/ copies uploaders ship, are kept
// once. Of different subtitles sharing a name, the one with the first archive
// path keeps it; each other one is prefixed with its folder ("UTF - name.srt"),
// or, with no folder, numbered ("name (2).srt") in
// path order; so is one whose prefixed name is taken or too long. The result
// keeps archive order.
func assignMemberNames(subtitles []extractedMember) ([]extractedMember, error) {
	groups := map[string][]int{}
	var keys []string
	for index, subtitle := range subtitles {
		key := strings.ToLower(subtitle.name)
		if _, exists := groups[key]; !exists {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], index)
	}
	sort.Strings(keys)
	taken := map[string]bool{}
	for _, key := range keys {
		taken[key] = true
	}
	keep := make([]bool, len(subtitles))
	for _, key := range keys {
		group := groups[key]
		sort.SliceStable(group, func(i, j int) bool {
			left, right := subtitles[group[i]], subtitles[group[j]]
			if left.path != right.path {
				return left.path < right.path
			}
			return left.sum < right.sum
		})
		kept := map[string]bool{}
		for position, index := range group {
			subtitle := &subtitles[index]
			if kept[subtitle.sum] {
				continue
			}
			kept[subtitle.sum] = true
			keep[index] = true
			if position == 0 {
				continue
			}
			name := folderPrefix(subtitle.path) + subtitle.name
			if name == subtitle.name || taken[strings.ToLower(name)] || len(name) > 255 {
				extension := filepath.Ext(subtitle.name)
				stem := strings.TrimSuffix(subtitle.name, extension)
				for number := 2; ; number++ {
					name = fmt.Sprintf("%s (%d)%s", stem, number, extension)
					if !taken[strings.ToLower(name)] {
						break
					}
				}
			}
			// A name longer than filesystems allow could not be written; it is
			// a content problem of this archive, not a reason to retry.
			if len(name) > 255 {
				return nil, fmt.Errorf("archive contains duplicate subtitle name %q", subtitle.name)
			}
			taken[strings.ToLower(name)] = true
			subtitle.name = name
		}
	}
	var named []extractedMember
	for index, subtitle := range subtitles {
		if keep[index] {
			named = append(named, subtitle)
		}
	}
	return named, nil
}

// folderPrefix names a member's folder as a file-name prefix ("UTF/x.srt" ->
// "UTF - "), so different subtitles that share a name stay apart.
func folderPrefix(name string) string {
	folder := filepath.Base(filepath.Dir(strings.ReplaceAll(name, `\`, "/")))
	if folder == "." || folder == "/" || folder == "" {
		return ""
	}
	return folder + " - "
}

func isSubtitleExtension(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt", ".ass", ".ssa", ".vtt", ".sub":
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

// HasArchiveMagic reports whether payload starts like a ZIP or RAR archive.
func HasArchiveMagic(payload []byte) bool {
	return bytes.HasPrefix(payload, zipMagic) || bytes.HasPrefix(payload, rarMagic)
}

func normalizeSubtitle(payload []byte, extension string) ([]byte, string, error) {
	text, err := decodeSubtitle(payload)
	if err != nil {
		return nil, "", err
	}
	extension = strings.ToLower(extension)
	if extension == ".sub" || extension == "" && isMicroDVD(text) {
		converted, err := microDVDToSRT(text)
		if err != nil {
			return nil, "", err
		}
		text, extension = converted, ".srt"
	}
	return validateSubtitle(text, extension)
}

// decodeSubtitle decodes a subtitle's text to UTF-8 with "\n" line endings
// (CR-only files included) and no byte order mark.
func decodeSubtitle(payload []byte) ([]byte, error) {
	text, err := decodeText(payload)
	if err != nil {
		return nil, err
	}
	text = bytes.ReplaceAll(text, []byte("\r\n"), []byte("\n"))
	text = bytes.ReplaceAll(text, []byte("\r"), []byte("\n"))
	return bytes.TrimPrefix(text, []byte{0xef, 0xbb, 0xbf}), nil
}

// validateSubtitle parses decoded text as extension (or the first format that
// reads it when extension is empty), then repairs and orders its cues.
func validateSubtitle(text []byte, extension string) ([]byte, string, error) {
	var err error
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
	if extension != ".ass" && extension != ".ssa" && negativeTiming.Match(text) {
		return nil, "", fmt.Errorf("subtitle timestamps are invalid")
	}
	for _, item := range subtitles.Items {
		if item.StartAt < 0 || item.EndAt < 0 {
			return nil, "", fmt.Errorf("subtitle timestamps are invalid")
		}
	}
	// Uploads often carry misplaced cues, a mistyped time or an empty cue;
	// repair and order them rather than discard the subtitle. LAPSE still
	// checks the timing.
	cues, err := planRepairs(subtitles.Items, extension)
	if err != nil {
		return nil, "", err
	}
	repaired, err := subtitleorder.Repair(text, extension, cues)
	if errors.Is(err, subtitleorder.ErrUnsafe) && onlyTidies(subtitles.Items, cues) {
		// The records could not be matched to the cues for a repair that only
		// tidies up (an unclosed <i>, an empty or repeated cue); the file
		// plays as it is, so keep it rather than reject it.
		return text, extension, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("subtitle timestamps are out of order: %w", err)
	}
	return repaired, extension, nil
}

// onlyTidies reports whether every cue, dropped ones included, already starts
// in file order as parsed and none the repair keeps needs new times (a
// mistyped start, or an end before its start), so the subtitle plays
// correctly without the repair. A dropped cue still stays in the kept text,
// where install would reject it out of order.
func onlyTidies(items []*astisub.Item, cues []subtitleorder.Cue) bool {
	var previous time.Duration
	for i, item := range items {
		if item.StartAt < previous {
			return false
		}
		previous = item.StartAt
		if cues[i].Drop {
			continue
		}
		if cues[i].Retime || item.EndAt < item.StartAt {
			return false
		}
	}
	return true
}

// macOSMetadata reports archive entries macOS adds beside real files: the
// __MACOSX folder and AppleDouble "._name" resource forks, which carry a
// subtitle's name but hold binary metadata.
func macOSMetadata(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	for _, part := range strings.Split(name, "/") {
		if part == "__MACOSX" || strings.HasPrefix(part, "._") {
			return true
		}
	}
	return false
}

func decodeText(payload []byte) ([]byte, error) {
	// UTF-16 is only recognized by its byte order mark; without one its NUL
	// bytes reject it below.
	if bytes.HasPrefix(payload, []byte{0xff, 0xfe}) || bytes.HasPrefix(payload, []byte{0xfe, 0xff}) {
		decoded, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder(), payload)
		if err != nil || !utf8.Valid(decoded) {
			return nil, fmt.Errorf("subtitle encoding is unsupported")
		}
		payload = decoded
	}
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
