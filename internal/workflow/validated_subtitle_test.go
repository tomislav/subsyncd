package workflow

import "subsyncd/internal/domain"

// validatedSubtitle runs the installer's source checks on path: the read
// (readSubtitleSource) and the validation (validateSubtitlePayload) that
// Installer.Install applies, without publishing anything.
func validatedSubtitle(path string, media domain.Media) ([]byte, error) {
	payload, err := readSubtitleSource(path)
	if err != nil {
		return nil, err
	}
	if err := validateSubtitlePayload(path, payload, media); err != nil {
		return nil, err
	}
	return payload, nil
}
