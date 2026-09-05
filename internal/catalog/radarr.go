package catalog

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

type Radarr struct {
	client     *arrClient
	entity     radarrEntityClient
	mappings   []config.PathMapping
	mediaRoots []string
}

func NewRadarr(instance, rawURL, apiKey string, mappings []config.PathMapping, mediaRoots []string, events *observability.Emitter) (*Radarr, error) {
	client, err := newArrClient(instance, rawURL, apiKey, nil)
	if err != nil {
		return nil, err
	}
	entity, err := newRadarrEntityClient(instance, rawURL, apiKey, events)
	if err != nil {
		return nil, err
	}
	return &Radarr{client: client, entity: entity, mappings: mappings, mediaRoots: mediaRoots}, nil
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
		StreamingService: streamingServiceFromSceneName(file.SceneName),
		Resolution:       resolutionName(file.Quality.Quality.Resolution),
		Edition:          file.Edition,
		Quality:          file.Quality.Quality.Name,
		Duration:         parseRuntime(file.MediaInfo.RunTime),
	}, nil
}

func (r *Radarr) ListChanges(ctx context.Context, since, through time.Time) ([]HistoryChange, error) {
	history, err := readHistoryWindow(ctx, r.entity, since, through)
	if err != nil {
		return nil, safeArrAPIError(r.client.instance, "history", err)
	}
	changes, err := reduceHistoryByEntity(history, domain.MediaMovie, through, func(record arrapi.HistoryRecord) int64 { return int64(record.MovieID) })
	if err != nil {
		return nil, err
	}
	for index := range changes {
		movie, err := r.entity.MovieByID(ctx, int(changes[index].EntityID))
		if err != nil {
			if arrapi.IsNotFound(err) {
				changes[index].Type = EventDelete
				changes[index].State = HistoryAbsent
				continue
			}
			return nil, safeArrAPIError(r.client.instance, "movie_current", err)
		}
		if !movie.HasFile || movie.MovieFile == nil {
			changes[index].Type = EventDelete
			changes[index].State = HistoryAbsent
			continue
		}
		if movie.MovieFile.ID <= 0 {
			return nil, fmt.Errorf("Radarr movie %d has invalid current file identity", changes[index].EntityID)
		}
		if _, err := MapPath(movie.MovieFile.Path, r.mappings, r.mediaRoots); err != nil {
			if IsOutsideScope(err) {
				changes[index].Type = EventDelete
				changes[index].State = HistoryOutsideScope
				continue
			}
			return nil, fmt.Errorf("map current Radarr movie %d: %w", changes[index].EntityID, err)
		}
		ref := domain.MediaRef{Instance: r.client.instance, Kind: domain.MediaMovie, FileID: int64(movie.MovieFile.ID)}
		item, err := r.GetMedia(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("hydrate Radarr history entity %d: %w", changes[index].EntityID, err)
		}
		if item.EntityID != changes[index].EntityID {
			return nil, fmt.Errorf("Radarr history entity %d resolved to entity %d", changes[index].EntityID, item.EntityID)
		}
		if changes[index].Type != EventRename {
			changes[index].Type = EventImport
		}
		changes[index].State = HistoryPresent
		changes[index].Media = item
	}
	return changes, nil
}
