package catalog

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
)

type Radarr struct {
	client     *arrClient
	mappings   []config.PathMapping
	mediaRoots []string
}

func NewRadarr(instance, rawURL, apiKey string, mappings []config.PathMapping, mediaRoots []string) (*Radarr, error) {
	client, err := newArrClient(instance, rawURL, apiKey, nil)
	if err != nil {
		return nil, err
	}
	return &Radarr{client: client, mappings: mappings, mediaRoots: mediaRoots}, nil
}

type radarrMovieFile struct {
	ID           int64        `json:"id"`
	MovieID      int64        `json:"movieId"`
	Path         string       `json:"path"`
	RelativePath string       `json:"relativePath"`
	Size         int64        `json:"size"`
	DateAdded    time.Time    `json:"dateAdded"`
	SceneName    string       `json:"sceneName"`
	ReleaseGroup string       `json:"releaseGroup"`
	Edition      string       `json:"edition"`
	Quality      arrQuality   `json:"quality"`
	MediaInfo    arrMediaInfo `json:"mediaInfo"`
}

type radarrMovie struct {
	ID              int64               `json:"id"`
	Title           string              `json:"title"`
	AlternateTitles []arrAlternateTitle `json:"alternateTitles"`
	Year            int                 `json:"year"`
	IMDbID          string              `json:"imdbId"`
	TMDBID          int64               `json:"tmdbId"`
}

func (r *Radarr) GetMedia(ctx context.Context, ref domain.MediaRef) (domain.Media, error) {
	if ref.Instance != r.client.instance || ref.Kind != domain.MediaMovie || ref.FileID <= 0 {
		return domain.Media{}, fmt.Errorf("invalid Radarr media reference")
	}
	var file radarrMovieFile
	if err := r.client.getJSON(ctx, "/api/v3/moviefile/"+strconv.FormatInt(ref.FileID, 10), nil, &file); err != nil {
		return domain.Media{}, err
	}
	var movie radarrMovie
	if err := r.client.getJSON(ctx, "/api/v3/movie/"+strconv.FormatInt(file.MovieID, 10), nil, &movie); err != nil {
		return domain.Media{}, err
	}
	path, err := MapPath(file.Path, r.mappings, r.mediaRoots)
	if err != nil {
		return domain.Media{}, fmt.Errorf("map Radarr file %d: %w", ref.FileID, err)
	}
	return domain.Media{
		Ref:              ref,
		Fingerprint:      domain.MediaFingerprint{Path: path, FileID: ref.FileID, Size: file.Size, ModTime: file.DateAdded},
		Title:            movie.Title,
		AlternateTitles:  alternateTitleStrings(movie.AlternateTitles),
		Year:             movie.Year,
		ExternalIDs:      domain.ExternalIDs{IMDb: movie.IMDbID, TMDB: movie.TMDBID},
		OriginalFilename: file.SceneName,
		ReleaseName:      file.SceneName,
		ReleaseGroup:     file.ReleaseGroup,
		Source:           file.Quality.Quality.Source,
		Resolution:       resolutionName(file.Quality.Quality.Resolution),
		Edition:          file.Edition,
		Quality:          file.Quality.Quality.Name,
		Duration:         parseRuntime(file.MediaInfo.RunTime),
	}, nil
}

func (r *Radarr) ListMediaChangedSince(ctx context.Context, since time.Time) ([]domain.Media, error) {
	var history []struct {
		MovieFileID int64 `json:"movieFileId"`
	}
	query := url.Values{"date": {since.UTC().Format(time.RFC3339Nano)}, "includeMovie": {"true"}}
	if err := r.client.getJSON(ctx, "/api/v3/history/since", query, &history); err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{}, len(history))
	media := make([]domain.Media, 0, len(history))
	for _, record := range history {
		if record.MovieFileID <= 0 {
			continue
		}
		if _, exists := seen[record.MovieFileID]; exists {
			continue
		}
		seen[record.MovieFileID] = struct{}{}
		item, err := r.GetMedia(ctx, domain.MediaRef{Instance: r.client.instance, Kind: domain.MediaMovie, FileID: record.MovieFileID})
		if err != nil {
			return nil, fmt.Errorf("hydrate Radarr history file %d: %w", record.MovieFileID, err)
		}
		media = append(media, item)
	}
	return media, nil
}
