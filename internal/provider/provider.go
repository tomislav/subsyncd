package provider

import (
	"context"
	"io"
	"net/http"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type SearchMode string

const (
	SearchExactHash SearchMode = "exact_hash"
	SearchBroad     SearchMode = "broad"
)

type Capabilities struct {
	ExactFileHash    bool
	SeasonPacks      bool
	DirectPackMember bool
}

type Operation string

const (
	OperationAll      Operation = "all"
	OperationSearch   Operation = "search"
	OperationDownload Operation = "download"
	OperationAuth     Operation = "auth"
)

type Throttle struct {
	ProviderID string
	Scope      Operation
	Reason     string
	Limit      int64
	Remaining  int64
	ResetAt    time.Time
	Disabled   bool
}

type SearchQuery struct {
	Media    domain.Media
	Language domain.Language
	Mode     SearchMode
}

type DownloadMetadata struct {
	Filename    string
	ContentType string
}

type Provider interface {
	ID() string
	Capabilities() Capabilities
	SupportsLanguage(domain.Language) bool
	Search(context.Context, SearchQuery) ([]domain.Candidate, error)
	Download(context.Context, domain.Candidate, io.Writer) (DownloadMetadata, error)
}

type Dependencies struct {
	HTTPClient *http.Client
	Store      *store.Store
	Clock      Clock
	Gate       *Gate
}

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type Factory func(id string, node yaml.Node, deps Dependencies) (Provider, error)
