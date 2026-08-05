package playlist

import (
	"bytes"
	"encoding/json"
	"explo/src/discovery"
	"explo/src/models"
	"explo/src/util"
	"explo/src/web/backend/app"
	"explo/src/web/backend/defs"
	"explo/src/web/backend/settings"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const lbAPIBase = "https://api.listenbrainz.org/1"

// PlaylistTrack represents a single track fetched from any playlist source.
// Replaces the raw [][4]string{title, artist, album, coverURL} pattern
// with named fields and adds MainArtist for accurate matching in music servers.
type PlaylistTrack struct {
	Title      string
	Artist     string
	MainArtist string
	Artists    []string
	Album      string
	CoverURL   string
}


type Playlist struct {
	settings *settings.Settings
	cfg app.Config
}

// validPlaylistTypes is derived from playlistDefs — no manual sync needed.
var validPlaylistTypes = func() map[string]bool {
	m := make(map[string]bool, len(defs.PlaylistDefs))
	for k := range defs.PlaylistDefs {
		m[k] = true
	}
	return m
}()

func NewPlaylist(cfg app.Config, settings *settings.Settings) *Playlist {
	return &Playlist{cfg: cfg, settings: settings}
}

// isValidPlaylistID accepts built-in playlist types and custom-* IDs (blocks path traversal).
func isValidPlaylistID(t string) bool {
	return validPlaylistTypes[t] || defs.CustomIDRe.MatchString(t)
}

// ── LB fallback ──────────────────────────────────────────────────────────────

func fetchOnRepeatTracks(username string) ([]PlaylistTrack, error) {
	tracks, err := discovery.FetchTopRecordings(util.NewHttp(util.HttpClientConfig{Timeout: 30}), username)
	if err != nil {
		return nil, err
	}
	return modelTracksToPlaylistTracks(tracks), nil
}

func fetchMostRecentLBPlaylist(username, playlistType string) ([]PlaylistTrack, error) {
	tracks, err := discovery.FetchMostRecentPlaylistByType(util.NewHttp(util.HttpClientConfig{Timeout: 30}), username, playlistType)
	if err != nil {
		return nil, err
	}
	return modelTracksToPlaylistTracks(tracks), nil
}

func modelTracksToPlaylistTracks(tracks []*models.Track) []PlaylistTrack {
	out := make([]PlaylistTrack, len(tracks))
	for i, t := range tracks {
		out[i] = PlaylistTrack{
			Title:      t.CleanTitle,
			Artist:     t.Artist,
			MainArtist: t.MainArtist,
			Album:      t.Album,
			CoverURL:   t.CoverURL,
		}
	}
	return out
}

// writePlaylistCache downloads cover art and writes a tracklist JSON for the web UI.
// added maps "CleanTitle|Artist" → true for tracks that made it into the playlist; nil means status unknown.
func WritePlaylistCache(cfgPath, playlist string, tracks []*models.Track, added map[string]bool) {
	type cache struct {
		Tracks []CachedTrack `json:"tracks"`
	}

	coversDir := filepath.Join(cfgPath, "cache", "covers")
	if err := os.MkdirAll(coversDir, 0755); err != nil {
		slog.Error("failed making directory", "msg", err.Error())
	}

	ct := make([]CachedTrack, len(tracks))
	for i, t := range tracks {
		// only re-download genuinely remote covers; already-cached /api/covers paths stay as-is
		apiPath, coverPath := t.CoverURL, t.CoverPath
		if strings.HasPrefix(t.CoverURL, "http") {
			apiPath, coverPath = util.DownloadCover(t.CoverURL, coversDir)
		}
		var inLibrary *bool
		if added != nil {
			v := added[t.CleanTitle+"|"+t.Artist]
			inLibrary = &v
		}
		ct[i] = CachedTrack{
			Rank:       i + 1,
			Title:      t.CleanTitle,
			Artist:     t.Artist,
			MainArtist: t.MainArtist,
			Artists:    t.Artists,
			Release:    t.Album,
			CoverURL:   apiPath,
			CoverPath:  coverPath,
			InLibrary:  inLibrary,
		}
	}

	raw, err := json.Marshal(cache{Tracks: ct})
	if err != nil {
		return
	}
	cacheDir := filepath.Join(cfgPath, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		slog.Error("failed creating cache dir", "msg", err.Error())
	}
	if err := os.WriteFile(filepath.Join(cacheDir, playlist+".json"), raw, 0644); err != nil {
		slog.Error("failed writing json file", "msg", err.Error())
	}
}

func lbGet(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Error("failed to close response", "msg", err.Error())
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LB returned %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// CachedTrack is the canonical shape of a track in a playlist cache file.
type CachedTrack struct {
	Rank       int      `json:"rank"`
	Title      string   `json:"title"`
	Artist     string   `json:"artist"`
	MainArtist string   `json:"mainArtist,omitempty"`
	Artists    []string `json:"artists,omitempty"`
	Release    string   `json:"release"`
	CoverURL   string   `json:"coverUrl,omitempty"`
	CoverPath  string   `json:"coverPath,omitempty"`
	InLibrary  *bool    `json:"inLibrary,omitempty"`
}

// writePreliminaryCache writes the track cache with remote cover URLs immediately.
// Returns false if the write fails.
func writePreliminaryCache(cfgDir, playlistType string, tracks []PlaylistTrack) bool {
	ct := make([]CachedTrack, len(tracks))
	for i, t := range tracks {
		ct[i] = CachedTrack{Rank: i + 1, Title: t.Title, Artist: t.Artist, MainArtist: t.MainArtist, Artists: t.Artists, Release: t.Album, CoverURL: t.CoverURL}
	}
	if !writeTrackCache(cfgDir, playlistType, ct) {
		return false
	}
	slog.Info("prefetch: cache written", "playlist", playlistType, "covers", "remote")
	return true
}

// downloadAndCacheCovers downloads cover art and rewrites the cache with local URLs.
// Safe to call in a goroutine.
func downloadAndCacheCovers(cfgDir, playlistType string, tracks []PlaylistTrack) {
	coversDir := filepath.Join(cfgDir, "cache", "covers")
	if err := os.MkdirAll(coversDir, 0755); err != nil {
		slog.Error("prefetch: failed to create covers dir", "err", err.Error())
		return
	}
	ct := make([]CachedTrack, len(tracks))
	for i, t := range tracks {
		if i > 0 {
			// space requests so a burst doesn't trip Apple's rate limit
			time.Sleep(300 * time.Millisecond)
		}
		APIPath, coverPath := util.DownloadCover(t.CoverURL, coversDir)
		ct[i] = CachedTrack{Rank: i + 1, Title: t.Title, Artist: t.Artist, MainArtist: t.MainArtist, Artists: t.Artists, Release: t.Album, CoverURL: APIPath, CoverPath: coverPath}
	}
	if writeTrackCache(cfgDir, playlistType, ct) {
		slog.Info("prefetch: cache updated", "playlist", playlistType, "covers", "local")
	}
}

func writePrefetchCache(cfgDir, playlistType string, tracks []PlaylistTrack) {
	if !writePreliminaryCache(cfgDir, playlistType, tracks) {
		return
	}
	downloadAndCacheCovers(cfgDir, playlistType, tracks)
}

// ── Background art ───────────────────────────────────────────────────────────

type sitewideReleasesResp struct {
	Payload struct {
		Releases []struct {
			ReleaseMbid string `json:"release_mbid"`
		} `json:"releases"`
	} `json:"payload"`
}

// minBackgroundPx is the smallest edge a cached cover needs to be usable as background art.
const minBackgroundPx = 600

// localCoverSize returns the dimensions of a cached cover, or 0,0 if it can't be read.
func localCoverSize(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// randomLocalCoverHiRes picks a random cached cover to use as background art and returns
// its API URL. Apple and Spotify covers are already large enough to serve as-is; CAA
// thumbnails are 250px, so a 1200px version is fetched once and cached as {mbid}-bg.jpg.
func randomLocalCoverHiRes(coversDir string) string {
	entries, err := os.ReadDir(coversDir)
	if err != nil {
		return ""
	}
	var stems []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, "-bg.jpg") {
			continue
		}
		stems = append(stems, strings.TrimSuffix(name, ".jpg"))
	}
	if len(stems) == 0 {
		return ""
	}
	rand.Shuffle(len(stems), func(i, j int) { stems[i], stems[j] = stems[j], stems[i] })

	for _, stem := range stems[:min(8, len(stems))] {
		bgFile := stem + "-bg.jpg"
		bgPath := filepath.Join(coversDir, bgFile)
		if _, err := os.Stat(bgPath); err == nil {
			return "/api/covers/" + bgFile
		}
		if w, h := localCoverSize(filepath.Join(coversDir, stem+".jpg")); w >= minBackgroundPx && h >= minBackgroundPx {
			return "/api/covers/" + stem + ".jpg"
		}
		// too small to use directly; only MBID-named covers can be upgraded via CAA
		if len(stem) != 36 || !lbMBIDRe.MatchString(stem) {
			continue
		}
		resp, err := http.Get("https://coverartarchive.org/release/" + stem + "/front-1200") //nolint:noctx
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close() // nolint:errcheck
			}
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close() // nolint:errcheck
		if err != nil {
			continue
		}
		if err := os.WriteFile(bgPath, data, 0644); err != nil {
			slog.Error("background-art: failed to write hi-res cover", "err", err.Error())
			continue
		}
		return "/api/covers/" + bgFile
	}
	return ""
}

// fetchSitewideCovers downloads cover art for the top global LB albums and
// returns a "/api/covers/<mbid>.jpg" URL for the first one that meets the
// minimum resolution requirement (1000px).
func fetchSitewideCovers(coversDir string) string {
	if err := os.MkdirAll(coversDir, 0755); err != nil {
		return ""
	}
	body, err := lbGet(lbAPIBase + "/stats/sitewide/releases?count=10&range=week")
	if err != nil {
		slog.Warn("background-art: LB sitewide fetch failed", "err", err)
		return ""
	}
	var resp sitewideReleasesResp
	if err := json.Unmarshal(body, &resp); err != nil {
		slog.Warn("background-art: LB sitewide parse failed", "err", err)
		return ""
	}
	for _, rel := range resp.Payload.Releases {
		if rel.ReleaseMbid == "" {
			continue
		}
		url := "https://coverartarchive.org/release/" + rel.ReleaseMbid + "/front-1200"

		dlResp, err := http.Get(url) //nolint:noctx
		if err != nil || dlResp.StatusCode != http.StatusOK {
			if dlResp != nil {
				dlResp.Body.Close() // nolint:errcheck
			}
			continue
		}
		data, err := io.ReadAll(dlResp.Body)
		dlResp.Body.Close() //nolint:errcheck
		if err != nil {
			continue
		}

		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width < 1000 || cfg.Height < 1000 {
			continue
		}

		destPath := filepath.Join(coversDir, rel.ReleaseMbid+".jpg")
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			slog.Error("background-art: failed to write sitewide cover", "err", err.Error())
			continue
		}
		return "/api/covers/" + rel.ReleaseMbid + ".jpg"
	}
	return ""
}

func writeTrackCache(cfgDir, playlistType string, tracks []CachedTrack) bool {
	type cache struct {
		Tracks []CachedTrack `json:"tracks"`
	}
	raw, err := json.Marshal(cache{Tracks: tracks})
	if err != nil {
		slog.Error("prefetch: failed to marshal cache", "err", err.Error())
		return false
	}
	cacheDir := filepath.Join(cfgDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		slog.Error("prefetch: failed to create cache dir", "err", err.Error())
		return false
	}
	if err := os.WriteFile(filepath.Join(cacheDir, playlistType+".json"), raw, 0644); err != nil {
		slog.Error("prefetch: failed to write cache", "err", err.Error())
		return false
	}
	return true
}
