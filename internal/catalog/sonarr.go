package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

type Sonarr struct {
	client     *arrClient
	entity     sonarrEntityClient
	mappings   []config.PathMapping
	mediaRoots []string
}

func (s *Sonarr) ListLibrary(ctx context.Context) ([]domain.Media, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	library, ok := s.entity.(sonarrLibraryClient)
	if !ok {
		return nil, fmt.Errorf("Sonarr catalog does not support library enumeration")
	}
	series, err := library.Series(ctx)
	if err != nil {
		return nil, safeArrAPIError(s.client.instance, "series_library", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if series == nil {
		return nil, fmt.Errorf("Sonarr library returned an incomplete series collection")
	}
	sort.Slice(series, func(i, j int) bool { return series[i].ID < series[j].ID })
	items := make([]domain.Media, 0)
	seenSeries := make(map[int64]struct{}, len(series))
	type fileIdentity struct {
		seriesID int64
		path     string
	}
	seenFiles := make(map[int64]fileIdentity)
	for _, show := range series {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seriesID := int64(show.ID)
		if seriesID <= 0 {
			return nil, fmt.Errorf("Sonarr library series has invalid identity")
		}
		if _, duplicate := seenSeries[seriesID]; duplicate {
			continue
		}
		seenSeries[seriesID] = struct{}{}
		files, err := library.EpisodeFiles(ctx, show.ID)
		if err != nil {
			return nil, safeArrAPIError(s.client.instance, "episode_file_library", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if files == nil {
			return nil, fmt.Errorf("Sonarr series %d returned an incomplete file collection", seriesID)
		}
		sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			fileID := int64(file.ID)
			if fileID <= 0 {
				return nil, fmt.Errorf("Sonarr series %d has invalid file identity", seriesID)
			}
			if int64(file.SeriesID) != seriesID {
				return nil, fmt.Errorf("Sonarr file %d has mismatched series identity", fileID)
			}
			identity := fileIdentity{seriesID: seriesID, path: normalizeRemote(file.Path)}
			if existing, duplicate := seenFiles[fileID]; duplicate {
				if existing != identity {
					return nil, fmt.Errorf("Sonarr file %d belongs to multiple series", fileID)
				}
				continue
			}
			seenFiles[fileID] = identity
			if _, err := MapPath(file.Path, s.mappings, s.mediaRoots); err != nil {
				if IsOutsideScope(err) {
					continue
				}
				return nil, fmt.Errorf("map current Sonarr file %d: %w", fileID, err)
			}
			media, _, err := s.hydrateMedia(ctx, domain.MediaRef{Instance: s.client.instance, Kind: domain.MediaEpisode, FileID: fileID})
			if err != nil {
				return nil, fmt.Errorf("hydrate Sonarr library file %d: %w", fileID, err)
			}
			if media.SeriesID != seriesID {
				return nil, fmt.Errorf("Sonarr library file %d resolved to series %d", fileID, media.SeriesID)
			}
			items = append(items, media)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func NewSonarr(instance, rawURL, apiKey string, mappings []config.PathMapping, mediaRoots []string, events *observability.Emitter) (*Sonarr, error) {
	client, err := newArrClient(instance, rawURL, apiKey, nil)
	if err != nil {
		return nil, err
	}
	entity, err := newSonarrEntityClient(instance, rawURL, apiKey, events)
	if err != nil {
		return nil, err
	}
	return &Sonarr{client: client, entity: entity, mappings: mappings, mediaRoots: mediaRoots}, nil
}

type arrQuality struct {
	Quality struct {
		Name       string `json:"name"`
		Resolution int    `json:"resolution"`
		Source     string `json:"source"`
	} `json:"quality"`
}

type arrMediaInfo struct {
	RunTime string `json:"runTime"`
}

type sonarrEpisodeFile struct {
	ID           int64        `json:"id"`
	SeriesID     int64        `json:"seriesId"`
	Path         string       `json:"path"`
	RelativePath string       `json:"relativePath"`
	Size         int64        `json:"size"`
	DateAdded    time.Time    `json:"dateAdded"`
	SceneName    string       `json:"sceneName"`
	ReleaseGroup string       `json:"releaseGroup"`
	Quality      arrQuality   `json:"quality"`
	MediaInfo    arrMediaInfo `json:"mediaInfo"`
}

type sonarrEpisode struct {
	ID                    int64  `json:"id"`
	SeriesID              int64  `json:"seriesId"`
	EpisodeFileID         int64  `json:"episodeFileId"`
	SeasonNumber          int    `json:"seasonNumber"`
	EpisodeNumber         int    `json:"episodeNumber"`
	AbsoluteEpisodeNumber int    `json:"absoluteEpisodeNumber"`
	Title                 string `json:"title"`
}

type arrAlternateTitle struct {
	Title string `json:"title"`
}

type sonarrSeries struct {
	ID              int64               `json:"id"`
	Title           string              `json:"title"`
	AlternateTitles []arrAlternateTitle `json:"alternateTitles"`
	Year            int                 `json:"year"`
	IMDbID          string              `json:"imdbId"`
	TVDBID          int64               `json:"tvdbId"`
}

func (s *Sonarr) GetMedia(ctx context.Context, ref domain.MediaRef) (domain.Media, error) {
	media, _, err := s.hydrateMedia(ctx, ref)
	return media, err
}

func (s *Sonarr) hydrateMedia(ctx context.Context, ref domain.MediaRef) (domain.Media, []int64, error) {
	if ref.Instance != s.client.instance || ref.Kind != domain.MediaEpisode || ref.FileID <= 0 {
		return domain.Media{}, nil, fmt.Errorf("invalid Sonarr media reference")
	}
	var file sonarrEpisodeFile
	if err := s.client.getJSON(ctx, "/api/v3/episodefile/"+strconv.FormatInt(ref.FileID, 10), nil, &file); err != nil {
		return domain.Media{}, nil, err
	}
	if file.ID != ref.FileID {
		return domain.Media{}, nil, fmt.Errorf("Sonarr file %d returned identity %d", ref.FileID, file.ID)
	}
	if file.SeriesID <= 0 {
		return domain.Media{}, nil, fmt.Errorf("Sonarr file %d has no series identity", ref.FileID)
	}
	var episodes []sonarrEpisode
	if err := s.client.getJSON(ctx, "/api/v3/episode", url.Values{"episodeFileId": {strconv.FormatInt(ref.FileID, 10)}}, &episodes); err != nil {
		return domain.Media{}, nil, err
	}
	if len(episodes) == 0 {
		return domain.Media{}, nil, fmt.Errorf("Sonarr file %d has no episode", ref.FileID)
	}
	sort.Slice(episodes, func(i, j int) bool {
		if episodes[i].SeasonNumber != episodes[j].SeasonNumber {
			return episodes[i].SeasonNumber < episodes[j].SeasonNumber
		}
		if episodes[i].EpisodeNumber != episodes[j].EpisodeNumber {
			return episodes[i].EpisodeNumber < episodes[j].EpisodeNumber
		}
		if episodes[i].AbsoluteEpisodeNumber != episodes[j].AbsoluteEpisodeNumber {
			return episodes[i].AbsoluteEpisodeNumber < episodes[j].AbsoluteEpisodeNumber
		}
		return episodes[i].ID < episodes[j].ID
	})
	episodeIDs := make([]int64, len(episodes))
	seenEpisodes := make(map[int64]struct{}, len(episodes))
	for index := range episodes {
		if episodes[index].ID <= 0 {
			return domain.Media{}, nil, fmt.Errorf("Sonarr file %d has an episode without identity", ref.FileID)
		}
		if episodes[index].SeriesID != file.SeriesID {
			return domain.Media{}, nil, fmt.Errorf("Sonarr file %d has an episode with mismatched series identity", ref.FileID)
		}
		if episodes[index].EpisodeFileID != 0 && episodes[index].EpisodeFileID != ref.FileID {
			return domain.Media{}, nil, fmt.Errorf("Sonarr file %d has an episode attached to file %d", ref.FileID, episodes[index].EpisodeFileID)
		}
		if _, duplicate := seenEpisodes[episodes[index].ID]; duplicate {
			return domain.Media{}, nil, fmt.Errorf("Sonarr file %d has duplicate episode identity %d", ref.FileID, episodes[index].ID)
		}
		seenEpisodes[episodes[index].ID] = struct{}{}
		episodeIDs[index] = episodes[index].ID
	}
	episode := episodes[0]
	seriesID := file.SeriesID
	var series sonarrSeries
	if err := s.client.getJSON(ctx, "/api/v3/series/"+strconv.FormatInt(seriesID, 10), nil, &series); err != nil {
		return domain.Media{}, nil, err
	}
	if series.ID != seriesID {
		return domain.Media{}, nil, fmt.Errorf("Sonarr series %d returned identity %d", seriesID, series.ID)
	}
	path, err := MapPath(file.Path, s.mappings, s.mediaRoots)
	if err != nil {
		return domain.Media{}, nil, fmt.Errorf("map Sonarr file %d: %w", ref.FileID, err)
	}
	media := domain.Media{
		EntityID:         episode.ID,
		SeriesID:         seriesID,
		Ref:              ref,
		Fingerprint:      domain.MediaFingerprint{Path: path, FileID: ref.FileID, Size: file.Size, ModTime: file.DateAdded},
		Title:            series.Title,
		EpisodeTitle:     episode.Title,
		AlternateTitles:  alternateTitleStrings(series.AlternateTitles),
		Year:             series.Year,
		Season:           episode.SeasonNumber,
		Episode:          episode.EpisodeNumber,
		AbsoluteEpisode:  episode.AbsoluteEpisodeNumber,
		ExternalIDs:      domain.ExternalIDs{IMDb: series.IMDbID, TVDB: series.TVDBID},
		OriginalFilename: file.SceneName,
		ReleaseName:      file.SceneName,
		ReleaseGroup:     file.ReleaseGroup,
		Source:           file.Quality.Quality.Source,
		StreamingService: streamingServiceFromSceneName(file.SceneName),
		Resolution:       resolutionName(file.Quality.Quality.Resolution),
		Quality:          file.Quality.Quality.Name,
		Duration:         parseRuntime(file.MediaInfo.RunTime),
	}
	if len(episodes) > 1 {
		media.UnsupportedReason = domain.UnsupportedMultiEpisode
	}
	return media, episodeIDs, nil
}

func (s *Sonarr) ListChanges(ctx context.Context, since, through time.Time) ([]HistoryChange, error) {
	history, err := readHistoryWindow(ctx, s.entity, since, through)
	if err != nil {
		return nil, safeArrAPIError(s.client.instance, "history", err)
	}
	changes, err := reduceHistoryByEntity(history, domain.MediaEpisode, through, func(record arrapi.HistoryRecord) int64 { return int64(record.EpisodeID) })
	if err != nil {
		return nil, err
	}
	type hydratedFile struct {
		media      domain.Media
		episodeIDs []int64
	}
	hydrated := make(map[int64]hydratedFile)
	present := make(map[int64]HistoryChange)
	finalized := make([]HistoryChange, 0, len(changes))
	var deferred error
nextChange:
	for _, change := range changes {
		episode, err := s.entity.EpisodeByID(ctx, int(change.EntityID))
		if err != nil {
			if arrapi.IsNotFound(err) {
				change.Type = EventDelete
				change.State = HistoryAbsent
				finalized = append(finalized, change)
				continue
			}
			return nil, safeArrAPIError(s.client.instance, "episode_current", err)
		}
		var fileID int64
		for currentRead := 0; currentRead < 2; currentRead++ {
			if !episode.HasFile || episode.EpisodeFile == nil {
				change.Type = EventDelete
				change.State = HistoryAbsent
				finalized = append(finalized, change)
				continue nextChange
			}
			fileID = int64(episode.EpisodeFile.ID)
			if fileID <= 0 {
				return nil, fmt.Errorf("Sonarr episode %d has invalid current file identity", change.EntityID)
			}
			if _, err := MapPath(episode.EpisodeFile.Path, s.mappings, s.mediaRoots); err != nil {
				if IsOutsideScope(err) {
					change.Type = EventDelete
					change.State = HistoryOutsideScope
					finalized = append(finalized, change)
					continue nextChange
				}
				if errors.Is(err, errMappedPathUnavailable) {
					if currentRead == 0 {
						episode, err = s.entity.EpisodeByID(ctx, int(change.EntityID))
						if err != nil {
							if arrapi.IsNotFound(err) {
								change.Type = EventDelete
								change.State = HistoryAbsent
								finalized = append(finalized, change)
								continue nextChange
							}
							return nil, safeArrAPIError(s.client.instance, "episode_current_recheck", err)
						}
						continue
					}
					deferred = errors.Join(deferred, fmt.Errorf("%w: Sonarr episode %d has unavailable current media", ErrHistoryDeferred, change.EntityID))
					continue nextChange
				}
				return nil, fmt.Errorf("map current Sonarr episode %d: %w", change.EntityID, err)
			}
			break
		}
		item, ok := hydrated[fileID]
		if !ok {
			media, episodeIDs, err := s.hydrateMedia(ctx, domain.MediaRef{Instance: s.client.instance, Kind: domain.MediaEpisode, FileID: fileID})
			if err != nil {
				if errors.Is(err, errMappedPathUnavailable) {
					rechecked, recheckErr := s.entity.EpisodeByID(ctx, int(change.EntityID))
					if recheckErr != nil {
						if arrapi.IsNotFound(recheckErr) {
							change.Type = EventDelete
							change.State = HistoryAbsent
							finalized = append(finalized, change)
							continue
						}
						return nil, safeArrAPIError(s.client.instance, "episode_current_recheck", recheckErr)
					}
					if !rechecked.HasFile || rechecked.EpisodeFile == nil {
						change.Type = EventDelete
						change.State = HistoryAbsent
						finalized = append(finalized, change)
						continue
					}
					fileID = int64(rechecked.EpisodeFile.ID)
					if fileID <= 0 {
						return nil, fmt.Errorf("Sonarr episode %d has invalid current file identity", change.EntityID)
					}
					if _, mapErr := MapPath(rechecked.EpisodeFile.Path, s.mappings, s.mediaRoots); mapErr != nil {
						if IsOutsideScope(mapErr) {
							change.Type = EventDelete
							change.State = HistoryOutsideScope
							finalized = append(finalized, change)
							continue
						}
						if errors.Is(mapErr, errMappedPathUnavailable) {
							deferred = errors.Join(deferred, fmt.Errorf("%w: Sonarr episode %d has unavailable current media", ErrHistoryDeferred, change.EntityID))
							continue
						}
						return nil, fmt.Errorf("map rechecked Sonarr episode %d: %w", change.EntityID, mapErr)
					}
					media, episodeIDs, err = s.hydrateMedia(ctx, domain.MediaRef{Instance: s.client.instance, Kind: domain.MediaEpisode, FileID: fileID})
					if err != nil {
						if errors.Is(err, errMappedPathUnavailable) {
							deferred = errors.Join(deferred, fmt.Errorf("%w: Sonarr episode %d has unavailable current media", ErrHistoryDeferred, change.EntityID))
							continue
						}
						return nil, fmt.Errorf("hydrate rechecked Sonarr history entity %d: %w", change.EntityID, err)
					}
				} else {
					return nil, fmt.Errorf("hydrate Sonarr history entity %d: %w", change.EntityID, err)
				}
			}
			item = hydratedFile{media: media, episodeIDs: episodeIDs}
			hydrated[fileID] = item
		}
		attached := false
		for _, episodeID := range item.episodeIDs {
			if episodeID == change.EntityID {
				attached = true
				break
			}
		}
		if !attached {
			return nil, fmt.Errorf("Sonarr history episode %d is not attached to current file %d", change.EntityID, fileID)
		}
		change.EntityID = item.media.EntityID
		change.Media = item.media
		change.State = HistoryPresent
		if change.Type != EventRename {
			change.Type = EventImport
		}
		present[change.EntityID] = change
	}
	for _, change := range present {
		finalized = append(finalized, change)
	}
	sortHistoryChanges(finalized)
	return finalized, deferred
}

func alternateTitleStrings(titles []arrAlternateTitle) []string {
	result := make([]string, 0, len(titles))
	for _, title := range titles {
		if title.Title != "" {
			result = append(result, title.Title)
		}
	}
	return result
}

func resolutionName(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value) + "p"
}

func parseRuntime(raw string) time.Duration {
	parsed, err := time.Parse("15:04:05", raw)
	if err != nil {
		return 0
	}
	return time.Duration(parsed.Hour())*time.Hour + time.Duration(parsed.Minute())*time.Minute + time.Duration(parsed.Second())*time.Second
}
