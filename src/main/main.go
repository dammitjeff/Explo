package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"explo/src/client"
	"explo/src/config"
	"explo/src/discovery"
	"explo/src/downloader"
	"explo/src/logging"
	"explo/src/models"
	"explo/src/util"
	"explo/src/web"
)

type Song struct {
	Title  string
	Artist string
	Album  string
}

func initHttpClient() *util.HttpClient {
	return util.NewHttp(util.HttpClientConfig{
		Timeout: 10,
	})
}

// writePlaylistCache downloads cover art and writes a tracklist JSON for the web UI.
// added maps "CleanTitle|Artist" → true for tracks that made it into the playlist; nil means status unknown.
func writePlaylistCache(cfgPath, playlist string, tracks []*models.Track, added map[string]bool) {
	type cachedTrack struct {
		Rank      int    `json:"rank"`
		Title     string `json:"title"`
		Artist    string `json:"artist"`
		Release   string `json:"release"`
		CoverURL  string `json:"coverUrl,omitempty"`
		InLibrary *bool  `json:"inLibrary,omitempty"`
	}
	type cache struct {
		Tracks []cachedTrack `json:"tracks"`
	}

	cfgDir := filepath.Dir(cfgPath)
	coversDir := filepath.Join(cfgDir, "cache", "covers")
	os.MkdirAll(coversDir, 0755)

	ct := make([]cachedTrack, len(tracks))
	for i, t := range tracks {
		localCover := ""
		if t.CoverURL != "" {
			// Use the CAA release MBID (second-to-last path segment) as filename.
			parts := strings.Split(strings.TrimRight(t.CoverURL, "/"), "/")
			mbid := parts[len(parts)-2]
			destPath := filepath.Join(coversDir, mbid+".jpg")
			if _, err := os.Stat(destPath); os.IsNotExist(err) {
				if resp, err := http.Get(t.CoverURL); err == nil { //nolint:noctx
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						if data, err := io.ReadAll(resp.Body); err == nil {
							os.WriteFile(destPath, data, 0644)
						}
					}
				}
			}
			localCover = "/api/covers/" + mbid + ".jpg"
		}
		var inLibrary *bool
		if added != nil {
			v := added[t.CleanTitle+"|"+t.Artist]
			inLibrary = &v
		}
		ct[i] = cachedTrack{
			Rank:      i + 1,
			Title:     t.CleanTitle,
			Artist:    t.Artist,
			Release:   t.Album,
			CoverURL:  localCover,
			InLibrary: inLibrary,
		}
	}

	raw, err := json.Marshal(cache{Tracks: ct})
	if err != nil {
		return
	}
	cacheDir := filepath.Join(cfgDir, "cache")
	os.MkdirAll(cacheDir, 0755)
	os.WriteFile(filepath.Join(cacheDir, playlist+".json"), raw, 0644)
}

// Inits debug, gets playlist name, if needed, handles deprecation
func setup(cfg *config.Config) {
	cfg.HandleDeprecation()
	notifyClient := logging.InitNotify(cfg.NotifyCfg)
	logging.Init(cfg.LogLevel, notifyClient)
	cfg.GenPlaylistName()
}

// coverScriptPath returns the path to generate-cover.js, checking CWD then executable dir.
func coverScriptPath() string {
	const rel = "scripts/generate-cover.js"
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), rel); func() bool { _, e := os.Stat(p); return e == nil }() {
			return p
		}
	}
	return rel
}

func generateAndUploadArtwork(mc *client.Client, cfgPath, playlistType string, tracks []*models.Track) {
	coversDir := filepath.Join(filepath.Dir(cfgPath), "cache", "covers")
	var coverPaths []string
	var artists []string
	seen := map[string]bool{}
	for _, t := range tracks {
		if t.CoverURL != "" {
			parts := strings.Split(strings.TrimRight(t.CoverURL, "/"), "/")
			mbid := parts[len(parts)-2]
			localPath := filepath.Join(coversDir, mbid+".jpg")
			if _, err := os.Stat(localPath); err == nil {
				coverPaths = append(coverPaths, localPath)
			}
		}
		if !seen[t.MainArtist] && t.MainArtist != "" {
			seen[t.MainArtist] = true
			artists = append(artists, t.MainArtist)
		}
	}
	if len(artists) > 3 {
		artists = artists[:3]
	}

	payload, _ := json.Marshal(map[string]any{
		"playlistType": playlistType,
		"artists":      artists,
		"coverUrls":    coverPaths,
	})
	cmd := exec.Command("node", coverScriptPath())
	cmd.Stdin = bytes.NewReader(payload)
	png, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			slog.Warn("failed to generate playlist artwork", "err", string(ee.Stderr))
		} else {
			slog.Warn("failed to generate playlist artwork", "err", err)
		}
		return
	}

	plexClient, ok := mc.API.(*client.Plex)
	if !ok {
		return
	}
	if err := plexClient.UploadArtwork(png); err != nil {
		slog.Warn("failed to upload playlist artwork", "err", err)
		return
	}
	slog.Info("playlist artwork uploaded")
}

func main() {
	if os.Getenv("WEB_UI") == "true" {
		cfgPath := os.Getenv("WEB_CFG_PATH")
		if cfgPath == "" {
			cfgPath = ".env"
		}
		exploPath, err := os.Executable()
		if err != nil {
			log.Fatal("could not determine executable path: ", err)
		}
		addr := os.Getenv("WEB_ADDR")
		if addr == "" {
			addr = ":7288"
		}
		srv := web.NewServer(cfgPath, exploPath)
		log.Fatal(srv.Start(addr))
	}

	var cfg config.Config
	if err := cfg.GetFlags(); err != nil {
		log.Fatal(err)
	}
	cfg.ReadEnv()
	cfg.MergeFlags()
	setup(&cfg)
	slog.Info("Starting Explo...")

	httpClient := initHttpClient()
	discovery := discovery.NewDiscoverer(cfg.DiscoveryCfg, httpClient)
	tracks, err := discovery.Discover()
	if err != nil {
		slog.Error(err.Error(), "notify", true)
		os.Exit(1)
	}
	allTracks := append([]*models.Track(nil), tracks...)

	mc, err := client.NewClient(&cfg)
	if err != nil {
		slog.Error(err.Error(), "notify", true)
		os.Exit(1)
	}
	downloader, err := downloader.NewDownloader(&cfg.DownloadCfg, httpClient, cfg.Flags.ExcludeLocal)
	if err != nil {
		slog.Error(err.Error(), "notify", true)
		os.Exit(1)
	}
	if !cfg.Persist {
		err := mc.DeletePlaylist()
		if err != nil {
			slog.Warn(err.Error(), "notify", true)
		}
		if cfg.DownloadCfg.UseSubDir {
			downloader.DeleteSongs()
		}
	}
	if cfg.Flags.DownloadMode != "force" {
		if err := mc.CheckTracks(tracks); err != nil { // Check if tracks exist on system before downloading
			slog.Warn(err.Error(), "notify", true)
		}
	}

	if cfg.Flags.DownloadMode != "skip" {
		downloader.StartDownload(&tracks)
		if len(tracks) == 0 {
			slog.Error("couldn't download any tracks", "notify", true)
			os.Exit(1)
		}
	}

	added := make(map[string]bool)
	for _, t := range tracks {
		added[t.CleanTitle+"|"+t.Artist] = true
	}
	writePlaylistCache(cfg.Flags.CfgPath, cfg.Flags.Playlist, allTracks, added)

	if err := mc.CreatePlaylist(tracks); err != nil {
		slog.Warn(err.Error())
	} else {
		slog.Info("playlist created successfully", "system", cfg.System, "playlistName", cfg.ClientCfg.PlaylistName, "notify", true)
		generateAndUploadArtwork(mc, cfg.Flags.CfgPath, cfg.Flags.Playlist, allTracks)
	}
}
