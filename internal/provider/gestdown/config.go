package gestdown

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
	"subsyncd/internal/domain"
)

const defaultBaseURL = "https://api.gestdown.info"
const maxDownloadBytes = 20 << 20

type Config struct {
	Type                  string  `yaml:"type"`
	BaseURL               string  `yaml:"base_url"`
	RequestsPerSecond     float64 `yaml:"requests_per_second"`
	Burst                 int     `yaml:"burst"`
	MaxConcurrent         int     `yaml:"max_concurrent"`
	MaxDownloadBytes      int64   `yaml:"max_download_bytes"`
	AllowInsecureForTests bool    `yaml:"-"`
}

func DecodeConfig(payload []byte) (Config, error) {
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(payload))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("invalid Gestdown configuration")
	}
	c.applyDefaults()
	return c, c.Validate()
}
func (c *Config) applyDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if c.RequestsPerSecond == 0 {
		c.RequestsPerSecond = 1
	}
	if c.Burst == 0 {
		c.Burst = 1
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 1
	}
	if c.MaxDownloadBytes == 0 {
		c.MaxDownloadBytes = maxDownloadBytes
	}
}
func (c Config) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(c.AllowInsecureForTests && u.Scheme == "http")) {
		return fmt.Errorf("Gestdown base_url must be an HTTPS origin")
	}
	if c.RequestsPerSecond <= 0 || c.Burst <= 0 || c.MaxConcurrent <= 0 || c.MaxDownloadBytes <= 0 || c.MaxDownloadBytes > maxDownloadBytes {
		return fmt.Errorf("Gestdown request limits must be positive and downloads at most 20 MiB")
	}
	return nil
}

// Gestdown accepts culture tags but returns English culture names. Keep regional
// variants distinct; never infer the returned language from the request filter.
// Contract: AddictedProxy.Culture/Service/CultureParser.cs in the official source.
var languageNames = map[domain.Language]string{
	"sq": "Albanian", "ar": "Arabic", "hy": "Armenian", "az": "Azerbaijani", "eu": "Basque", "bn": "Bengali", "bs": "Bosnian", "bg": "Bulgarian", "ca": "Catalan", "zh": "Chinese", "hr": "Croatian", "cs": "Czech", "da": "Danish", "nl": "Dutch", "en": "English", "eo": "Esperanto", "et": "Estonian", "fi": "Finnish", "fr": "French", "fr-CA": "French (Canada)", "gl": "Galician", "ka": "Georgian", "de": "German", "el": "Greek", "he": "Hebrew", "hi": "Hindi", "hu": "Hungarian", "is": "Icelandic", "id": "Indonesian", "it": "Italian", "ja": "Japanese", "ko": "Korean", "lv": "Latvian", "lt": "Lithuanian", "mk": "Macedonian", "ms": "Malay", "no": "Norwegian", "fa": "Persian", "pl": "Polish", "pt": "Portuguese", "pt-BR": "Portuguese (Brazil)", "ro": "Romanian", "ru": "Russian", "sr": "Serbian", "sk": "Slovak", "sl": "Slovenian", "es": "Spanish", "sv": "Swedish", "tl": "Tagalog", "ta": "Tamil", "te": "Telugu", "th": "Thai", "tr": "Turkish", "uk": "Ukrainian", "ur": "Urdu", "vi": "Vietnamese", "yue": "Cantonese",
}

func returnedLanguage(raw string) (domain.Language, bool) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "portuguese (brazilian)":
		return "pt-BR", true
	case "french (canadian)":
		return "fr-CA", true
	case "galego":
		return "gl", true
	case "euskera":
		return "eu", true
	case "català":
		return "ca", true
	}
	for tag, name := range languageNames {
		if strings.EqualFold(raw, name) || strings.EqualFold(raw, string(tag)) {
			return tag, true
		}
	}
	return "", false
}
