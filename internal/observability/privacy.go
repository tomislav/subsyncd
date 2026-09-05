package observability

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"subsyncd/internal/domain"
)

const maximumTextRunes = 2048

type resolvedRoot struct {
	path string
}

func SafeText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(value) <= maximumTextRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maximumTextRunes])
}

// MediaTitle returns a bounded, single-line identity suitable for structured
// log fields. It intentionally contains no filesystem or provider data.
func MediaTitle(media domain.Media) string {
	title := strings.TrimSpace(media.Title)
	switch media.Ref.Kind {
	case domain.MediaMovie:
		if title != "" && media.Year > 0 {
			title = fmt.Sprintf("%s (%d)", title, media.Year)
		}
	case domain.MediaEpisode:
		if media.Season > 0 || media.Episode > 0 {
			title = fmt.Sprintf("%s - S%02dE%02d", title, media.Season, media.Episode)
		}
		if episodeTitle := strings.TrimSpace(media.EpisodeTitle); episodeTitle != "" {
			title += " - " + episodeTitle
		}
	}
	return SafeText(title)
}

func resolveRoot(root string) (resolvedRoot, bool) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return resolvedRoot{}, false
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return resolvedRoot{}, false
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return resolvedRoot{}, false
	}
	return resolvedRoot{path: filepath.Clean(resolved)}, true
}

func (e *Emitter) RelativePath(path string) (string, bool) {
	if e == nil || strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", false
		}
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if parentErr != nil || filepath.Base(absolute) == "." || filepath.Base(absolute) == string(filepath.Separator) {
			return "", false
		}
		resolved = filepath.Join(parent, filepath.Base(absolute))
	}
	resolved = filepath.Clean(resolved)
	for _, root := range e.roots {
		relative, relErr := filepath.Rel(root.path, resolved)
		if relErr != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		return filepath.ToSlash(relative), true
	}
	return "", false
}
