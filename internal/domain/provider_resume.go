package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
)

// ProviderResumeSignature scopes provider progress to an opaque route identity
// and the exact media fingerprint. Only the digest is persisted; paths are never
// included in resume records or logs.
func ProviderResumeSignature(route string, fingerprint MediaFingerprint) string {
	digest := sha256.New()
	for _, part := range []string{"provider-resume-v1", route, fingerprint.Path,
		strconv.FormatInt(fingerprint.FileID, 10), strconv.FormatInt(fingerprint.Size, 10),
		strconv.FormatInt(fingerprint.ModTime.UnixNano(), 10)} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(part))
	}
	return hex.EncodeToString(digest.Sum(nil))
}
