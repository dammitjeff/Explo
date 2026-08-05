package util

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"explo/src/logging"
)

type HttpClientConfig struct {
	Timeout int
}

type HttpClient struct {
	Client    *http.Client
	UserAgent string
}

func NewHttp(cfg HttpClientConfig) *HttpClient {
	return &HttpClient{
		Client: &http.Client{
			Timeout: time.Duration(cfg.Timeout) * time.Second,
		},
		UserAgent: "Explo (+https://github.com/LumePart/explo))",
	}
}

func (c *HttpClient) MakeRequest(method, url string, payload io.Reader, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize request: %s", err.Error())
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("User-Agent", c.UserAgent)

	for key, value := range headers {
		req.Header.Add(key, value)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %s", err.Error())
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("response body close failed", "context", err.Error())
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %s", err.Error())
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Debug("response info", logging.RuntimeAttr(string(body)))
		return nil, fmt.Errorf("got %d from %s", resp.StatusCode, url)
	}

	return body, nil
}

func ParseResp[T any](body []byte, target *T) error {

	if err := json.Unmarshal(body, target); err != nil {
		slog.Debug("response info", logging.RuntimeAttr(string(body)))
		return fmt.Errorf("error unmarshaling response body: %s", err.Error())
	}
	return nil
}

// browserUA mimics a desktop browser; the default Go UA gets throttled harder.
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// fetchBytes GETs url with a browser user-agent, retrying with backoff.
func fetchBytes(url string) ([]byte, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
		req, err := http.NewRequest("GET", url, nil) //nolint:noctx
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", browserUA)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("status %d from %s", resp.StatusCode, url)
			// a 4xx won't fix itself on retry, but a 429 will
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				break
			}
			continue
		}
		data, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr == nil && len(data) > 0 {
			return data, nil
		}
		lastErr = rerr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no data from %s", url)
	}
	return nil, lastErr
}

// DownloadFile downloads a URL to destPath, creating parent directories as needed.
// No-op if destPath already exists. Returns the resolved local path on success.
func DownloadFile(url, destPath string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("empty url")
	}
	if _, err := os.Stat(destPath); err == nil {
		return destPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	data, err := fetchBytes(url)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return destPath, nil
}

var caaReleaseRe = regexp.MustCompile(`coverartarchive\.org/release/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

const coverFetchSpacing = 300 * time.Millisecond

var (
	coverFetchMu   sync.Mutex
	lastCoverFetch time.Time
)

// coverThrottle spaces cover downloads so a burst doesn't trip Apple's rate limit.
// Global, so concurrent playlist imports share one budget.
func coverThrottle() {
	coverFetchMu.Lock()
	defer coverFetchMu.Unlock()
	if d := coverFetchSpacing - time.Since(lastCoverFetch); d > 0 {
		time.Sleep(d)
	}
	lastCoverFetch = time.Now()
}

// coverID names a cached cover. CAA covers keep their MBID so the background-art
// picker can still resolve them; other sources hash, having no stable path segment.
func coverID(url string) string {
	if m := caaReleaseRe.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	sum := sha1.Sum([]byte(url))
	return hex.EncodeToString(sum[:])[:16]
}

// DownloadCover downloads coverURL into coversDir and returns cover api and filesystem path.
// Returns "" if url is empty.
func DownloadCover(url, coversDir string) (string, string) {
	if url == "" {
		return "", ""
	}
	id := coverID(url)
	destPath := filepath.Join(coversDir, id+".jpg")
	if _, err := os.Stat(destPath); os.IsNotExist(err) {
		coverThrottle()
		if data, err := fetchBytes(url); err != nil {
			slog.Warn("cover download failed", "url", url, "err", err.Error())
		} else if err := os.WriteFile(destPath, data, 0644); err != nil {
			slog.Error("failed writing cover", "path", destPath, "err", err.Error())
		}
	}
	apiURL := fmt.Sprintf("/api/covers/%s.jpg", id)
	return apiURL, destPath
}
