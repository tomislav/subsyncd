package catalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

func (r *Radarr) ListLibrary(ctx context.Context) ([]domain.Media, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	library, ok := r.entity.(radarrLibraryClient)
	if !ok {
		return nil, fmt.Errorf("Radarr catalog does not support library enumeration")
	}
	movies, err := library.Movies(ctx)
	if err != nil {
		return nil, safeArrAPIError(r.client.instance, "movie_library", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if movies == nil {
		return nil, fmt.Errorf("Radarr library returned an incomplete movie collection")
	}
	sort.Slice(movies, func(i, j int) bool { return movies[i].ID < movies[j].ID })
	items := make([]domain.Media, 0, len(movies))
	type movieIdentity struct {
		hasFile bool
		fileID  int64
		path    string
	}
	seenEntities := make(map[int64]movieIdentity, len(movies))
	seenFiles := make(map[int64]int64, len(movies))
	for _, movie := range movies {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entityID := int64(movie.ID)
		if entityID <= 0 {
			return nil, fmt.Errorf("Radarr library movie has invalid identity")
		}
		if movie.HasFile && movie.MovieFile == nil {
			return nil, fmt.Errorf("Radarr movie %d reports a file without file details", entityID)
		}
		identity := movieIdentity{hasFile: movie.HasFile}
		if movie.MovieFile != nil {
			identity.fileID = int64(movie.MovieFile.ID)
			identity.path = normalizeRemote(movie.MovieFile.Path)
		}
		if existing, duplicate := seenEntities[entityID]; duplicate {
			if existing != identity {
				return nil, fmt.Errorf("Radarr movie %d has conflicting duplicate library records", entityID)
			}
			continue
		}
		seenEntities[entityID] = identity
		if !movie.HasFile {
			continue
		}
		fileID := int64(movie.MovieFile.ID)
		if fileID <= 0 {
			return nil, fmt.Errorf("Radarr movie %d has invalid current file identity", entityID)
		}
		if existing, duplicate := seenFiles[fileID]; duplicate {
			if existing != entityID {
				return nil, fmt.Errorf("Radarr file %d belongs to multiple movies", fileID)
			}
			continue
		}
		seenFiles[fileID] = entityID
		if _, err := MapPath(movie.MovieFile.Path, r.mappings, r.mediaRoots); err != nil {
			if IsOutsideScope(err) {
				continue
			}
			return nil, fmt.Errorf("map current Radarr movie %d: %w", entityID, err)
		}
		item, err := r.GetMedia(ctx, domain.MediaRef{Instance: r.client.instance, Kind: domain.MediaMovie, FileID: fileID})
		if err != nil {
			return nil, fmt.Errorf("hydrate Radarr library entity %d: %w", entityID, err)
		}
		if item.EntityID != entityID {
			return nil, fmt.Errorf("Radarr library entity %d resolved to entity %d", entityID, item.EntityID)
		}
		items = append(items, item)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

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
	if file.ID != ref.FileID {
		return domain.Media{}, fmt.Errorf("Radarr file %d returned identity %d", ref.FileID, file.ID)
	}
	if file.MovieID <= 0 {
		return domain.Media{}, fmt.Errorf("Radarr file %d has no movie identity", ref.FileID)
	}
	var movie radarrMovie
	if err := r.client.getJSON(ctx, "/api/v3/movie/"+strconv.FormatInt(file.MovieID, 10), nil, &movie); err != nil {
		return domain.Media{}, err
	}
	if movie.ID != file.MovieID {
		return domain.Media{}, fmt.Errorf("Radarr movie %d returned identity %d", file.MovieID, movie.ID)
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
	finalized := make([]HistoryChange, 0, len(changes))
	var deferred error
nextChange:
	for index := range changes {
		change := changes[index]
		movie, err := r.entity.MovieByID(ctx, int(change.EntityID))
		if err != nil {
			if arrapi.IsNotFound(err) {
				change.Type = EventDelete
				change.State = HistoryAbsent
				finalized = append(finalized, change)
				continue
			}
			return nil, safeArrAPIError(r.client.instance, "movie_current", err)
		}
		for currentRead := 0; currentRead < 2; currentRead++ {
			if !movie.HasFile || movie.MovieFile == nil {
				change.Type = EventDelete
				change.State = HistoryAbsent
				finalized = append(finalized, change)
				continue nextChange
			}
			if movie.MovieFile.ID <= 0 {
				return nil, fmt.Errorf("Radarr movie %d has invalid current file identity", change.EntityID)
			}
			if _, err := MapPath(movie.MovieFile.Path, r.mappings, r.mediaRoots); err != nil {
				if IsOutsideScope(err) {
					change.Type = EventDelete
					change.State = HistoryOutsideScope
					finalized = append(finalized, change)
					continue nextChange
				}
				if errors.Is(err, errMappedPathUnavailable) {
					if currentRead == 0 {
						movie, err = r.entity.MovieByID(ctx, int(change.EntityID))
						if err != nil {
							if arrapi.IsNotFound(err) {
								change.Type = EventDelete
								change.State = HistoryAbsent
								finalized = append(finalized, change)
								continue nextChange
							}
							return nil, safeArrAPIError(r.client.instance, "movie_current_recheck", err)
						}
						continue
					}
					deferred = errors.Join(deferred, fmt.Errorf("%w: Radarr movie %d has unavailable current media", ErrHistoryDeferred, change.EntityID))
					continue nextChange
				}
				return nil, fmt.Errorf("map current Radarr movie %d: %w", change.EntityID, err)
			}
			break
		}
		ref := domain.MediaRef{Instance: r.client.instance, Kind: domain.MediaMovie, FileID: int64(movie.MovieFile.ID)}
		item, err := r.GetMedia(ctx, ref)
		if err != nil {
			if errors.Is(err, errMappedPathUnavailable) {
				rechecked, recheckErr := r.entity.MovieByID(ctx, int(change.EntityID))
				if recheckErr != nil {
					if arrapi.IsNotFound(recheckErr) {
						change.Type = EventDelete
						change.State = HistoryAbsent
						finalized = append(finalized, change)
						continue
					}
					return nil, safeArrAPIError(r.client.instance, "movie_current_recheck", recheckErr)
				}
				if !rechecked.HasFile || rechecked.MovieFile == nil {
					change.Type = EventDelete
					change.State = HistoryAbsent
					finalized = append(finalized, change)
					continue
				}
				if rechecked.MovieFile.ID <= 0 {
					return nil, fmt.Errorf("Radarr movie %d has invalid current file identity", change.EntityID)
				}
				if _, mapErr := MapPath(rechecked.MovieFile.Path, r.mappings, r.mediaRoots); mapErr != nil {
					if IsOutsideScope(mapErr) {
						change.Type = EventDelete
						change.State = HistoryOutsideScope
						finalized = append(finalized, change)
						continue
					}
					if errors.Is(mapErr, errMappedPathUnavailable) {
						deferred = errors.Join(deferred, fmt.Errorf("%w: Radarr movie %d has unavailable current media", ErrHistoryDeferred, change.EntityID))
						continue
					}
					return nil, fmt.Errorf("map rechecked Radarr movie %d: %w", change.EntityID, mapErr)
				}
				ref = domain.MediaRef{Instance: r.client.instance, Kind: domain.MediaMovie, FileID: int64(rechecked.MovieFile.ID)}
				item, err = r.GetMedia(ctx, ref)
				if err != nil {
					if errors.Is(err, errMappedPathUnavailable) {
						deferred = errors.Join(deferred, fmt.Errorf("%w: Radarr movie %d has unavailable current media", ErrHistoryDeferred, change.EntityID))
						continue
					}
					return nil, fmt.Errorf("hydrate rechecked Radarr history entity %d: %w", change.EntityID, err)
				}
			} else {
				return nil, fmt.Errorf("hydrate Radarr history entity %d: %w", change.EntityID, err)
			}
		}
		if item.EntityID != change.EntityID {
			return nil, fmt.Errorf("Radarr history entity %d resolved to entity %d", change.EntityID, item.EntityID)
		}
		if change.Type != EventRename {
			change.Type = EventImport
		}
		change.State = HistoryPresent
		change.Media = item
		finalized = append(finalized, change)
	}
	return finalized, deferred
}
