package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrBlocked means the origin answered 403/429/503 or a challenge page.
// The crawl stops at once; nothing is retried.
var ErrBlocked = errors.New("origin blocked or challenged the request")

var errNotFound = errors.New("404 not found")

// Fetcher does polite, one-at-a-time HTTP: a minimum gap between requests,
// an honest User-Agent, and no retries after a block.
type Fetcher struct {
	UA      string
	Referer string
	Gap     time.Duration
	Data    string
	client  *http.Client
	last    time.Time
	Fetched int
}

func NewFetcher(ua string, gap time.Duration, data string) *Fetcher {
	return &Fetcher{UA: ua, Gap: gap, Data: data, client: &http.Client{Timeout: 45 * time.Second}}
}

func (f *Fetcher) path(rel string) string { return filepath.Join(f.Data, "raw", rel) }

// Have reports whether rel is already on disk.
func (f *Fetcher) Have(rel string) bool {
	st, err := os.Stat(f.path(rel))
	return err == nil && st.Size() > 0
}

// Get returns the bytes for url, from disk if already stored, else from the
// network (then stored under raw/rel).
func (f *Fetcher) Get(url, rel string) ([]byte, error) {
	if f.Have(rel) {
		return os.ReadFile(f.path(rel))
	}
	bf := blockFile(url)
	if blocked, _ := os.ReadFile(filepath.Join(f.Data, bf)); len(blocked) > 0 {
		return nil, fmt.Errorf("%w: see %s/%s", ErrBlocked, f.Data, bf)
	}
	if d := f.Gap - time.Since(f.last); d > 0 {
		time.Sleep(d)
	}
	var body []byte
	var err error
	for attempt, wait := 0, 10*time.Second; attempt < 3; attempt, wait = attempt+1, wait*3 {
		f.last = time.Now()
		body, err = f.once(url)
		if err == nil || errors.Is(err, ErrBlocked) || errors.Is(err, errNotFound) {
			break
		}
		fmt.Fprintf(os.Stderr, "warn %s: %v\n", url, err)
		if attempt < 2 {
			time.Sleep(wait)
		}
	}
	f.last = time.Now()
	if err != nil {
		if errors.Is(err, ErrBlocked) {
			msg := fmt.Sprintf("%s\n%s\n%v\n", time.Now().UTC().Format(time.RFC3339), url, err)
			os.WriteFile(filepath.Join(f.Data, bf), []byte(msg), 0o644)
		}
		return nil, err
	}
	p := f.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p+".part", body, 0o644); err != nil {
		return nil, err
	}
	f.Fetched++
	return body, os.Rename(p+".part", p)
}

// blockFile is the per-host stop file: BLOCKED for pcm.gov.lb (the original name),
// BLOCKED-<host> for any other host, so one blocked host never stops another.
func blockFile(rawURL string) string {
	h := rawURL
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h = strings.SplitN(h, "/", 2)[0]
	if strings.HasSuffix(h, "pcm.gov.lb") {
		return "BLOCKED"
	}
	return "BLOCKED-" + h
}

func (f *Fetcher) once(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", f.UA)
	if f.Referer != "" {
		req.Header.Set("Referer", f.Referer)
	}
	req.Header.Set("Accept-Language", "ar,en;q=0.8")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 404:
		return nil, errNotFound
	case resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode == 503:
		return nil, fmt.Errorf("%w: HTTP %d", ErrBlocked, resp.StatusCode)
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	low := strings.ToLower(string(b[:min(len(b), 4000)]))
	for _, w := range []string{"captcha", "access denied", "just a moment", "unusual traffic"} {
		if strings.Contains(low, w) {
			return nil, fmt.Errorf("%w: page mentions %q", ErrBlocked, w)
		}
	}
	return b, nil
}
