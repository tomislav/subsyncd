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
	if file.MovieID <= 0 {
		return domain.Media{}, fmt.Errorf("Radarr file %d has no movie identity", ref.FileID)
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
		EntityID:         file.MovieID,
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

func (r *Radarr) ListChangesSince(ctx context.Context, since time.Time) ([]HistoryChange, error) {
	var history []arrHistoryRecord
	query := url.Values{"date": {since.UTC().Format(time.RFC3339Nano)}, "includeMovie": {"true"}}
	if err := r.client.getJSON(ctx, "/api/v3/history/since", query, &history); err != nil {
		return nil, err
	}
	changes, err := reduceHistory(r.client.instance, domain.MediaMovie, history, func(record arrHistoryRecord) int64 { return record.MovieFileID }, radarrHistoryEvent)
	if err != nil {
		return nil, err
	}
	for index := range changes {
		if changes[index].Type == EventDelete {
			continue
		}
		item, err := r.GetMedia(ctx, changes[index].Ref)
		if err != nil {
			return nil, fmt.Errorf("hydrate Radarr history file %d: %w", changes[index].Ref.FileID, err)
		}
		changes[index].Media = item
	}
	return changes, nil
}
