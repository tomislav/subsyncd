package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"subsyncd/internal/config"
)

// A scope change permits another background discovery pass. Credentials and
// language changes do not invalidate completion; language backfill is separate.
func libraryDiscoveryScope(instance config.InstanceConfig, mediaRoots []string) string {
	mappings := append([]config.PathMapping(nil), instance.PathMappings...)
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].Remote != mappings[j].Remote {
			return mappings[i].Remote < mappings[j].Remote
		}
		return mappings[i].Local < mappings[j].Local
	})
	roots := append([]string(nil), mediaRoots...)
	sort.Strings(roots)
	data, _ := json.Marshal(struct {
		Version  string
		Type     string
		URL      string
		Mappings []config.PathMapping
		Roots    []string
	}{"library-v1", instance.Type, instance.URL, mappings, roots})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
