package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"subsyncd/internal/config"
)

var ErrOutsideScope = errors.New("media is outside configured scope")

var errMappedPathUnavailable = errors.New("mapped media path is unavailable")

func IsOutsideScope(err error) bool {
	return errors.Is(err, ErrOutsideScope)
}

// MapPath errors never include the remote path or a mapped path outside the
// media roots: log redaction only replaces configured local roots.
func MapPath(remote string, mappings []config.PathMapping, mediaRoots []string) (string, error) {
	normalizedRemote := normalizeRemote(remote)
	type candidate struct {
		remote string
		local  string
	}
	candidates := make([]candidate, 0, len(mappings))
	for _, mapping := range mappings {
		prefix := strings.TrimSuffix(normalizeRemote(mapping.Remote), "/")
		if normalizeRemote(mapping.Remote) == "/" {
			prefix = "/"
		}
		if prefix == "" || !hasPathPrefix(normalizedRemote, prefix) {
			continue
		}
		candidates = append(candidates, candidate{remote: prefix, local: mapping.Local})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: remote path does not match a configured mapping", ErrOutsideScope)
	}
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i].remote) > len(candidates[j].remote) })
	selected := candidates[0]
	remainder := strings.TrimPrefix(normalizedRemote, selected.remote)
	remainder = strings.TrimPrefix(remainder, "/")
	parts := strings.Split(remainder, "/")
	for _, part := range parts {
		if part == ".." {
			return "", errors.New("remote path contains traversal")
		}
	}
	mapped := filepath.Clean(filepath.Join(append([]string{selected.local}, parts...)...))
	if !filepath.IsAbs(mapped) {
		return "", errors.New("mapped path is not absolute")
	}
	resolved, err := resolveExistingParents(mapped)
	if err != nil {
		return "", err
	}
	for _, root := range mediaRoots {
		resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return "", fmt.Errorf("resolve media root %q: %w", root, err)
		}
		if pathContained(resolvedRoot, resolved) {
			return mapped, nil
		}
	}
	return "", fmt.Errorf("%w: mapped path is outside configured media roots", ErrOutsideScope)
}

func normalizeRemote(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

func hasPathPrefix(path, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(path, "/")
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}

func resolveExistingParents(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				if os.IsNotExist(err) {
					return "", fmt.Errorf("%w: resolve mapped path: %v", errMappedPathUnavailable, pathErrorCause(err))
				}
				return "", fmt.Errorf("resolve mapped path: %w", pathErrorCause(err))
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect mapped path: %w", pathErrorCause(err))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("cannot resolve mapped path")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathContained(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// pathErrorCause drops the path an os.PathError carries; resolution runs
// before the media-root check, so that path may not be redactable.
func pathErrorCause(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
