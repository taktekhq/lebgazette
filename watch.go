package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// watch polls sources that publish official PDFs of laws, decrees and rulings
// (or signal that one is out). Manners: one request at a time, a gap per host,
// an honest User-Agent, and a stop file per host on any 403/429/challenge, so
// one blocked host never stops the others and nothing is retried around a block.

var errHostBlocked = errors.New("host blocked")

// Item is one row of data/watch/items.tsv.
type Item struct {
	FirstSeen  string
	Source     string
	Kind       string // pdf | alert
	Title      string
	ArticleURL string
	PDFURL     string
	Date       string
	SHA        string
	Bytes      int
	Status     string // official | unjudged | draft | alert
	Reason     string
	ByOrigin   bool // the source itself is the issuing body (CC, ministries)
}

type watcher struct {
	data     string
	ua       string
	gap      time.Duration
	last     map[string]time.Time
	client   *http.Client
	items    []Item
	known    map[string]bool // every URL examined (article, pdf, alert link)
	fresh    []Item
	requests int
	errs     []string
	maxFetch int
	judge    string
	model    string
	pcmBlock string // the crawler's BLOCKED file
}

func newWatcher(data, ua string, gap time.Duration, judge string, maxFetch int) *watcher {
	w := &watcher{data: data, ua: ua, gap: gap, last: map[string]time.Time{}, known: map[string]bool{},
		client: &http.Client{Timeout: 60 * time.Second}, judge: judge, model: defaultGemini, maxFetch: maxFetch,
		pcmBlock: filepath.Join(data, "BLOCKED")}
	os.MkdirAll(w.p("stop"), 0o755)
	os.MkdirAll(w.p("pdf"), 0o755)
	os.MkdirAll(w.p("outbox"), 0o755)
	w.load()
	return w
}

func (w *watcher) p(rel ...string) string {
	return filepath.Join(append([]string{w.data, "watch"}, rel...)...)
}

func (w *watcher) itemsPath() string { return w.p("items.tsv") }

func tsvEsc(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

func (w *watcher) load() {
	b, err := os.ReadFile(w.itemsPath())
	if err == nil {
		for _, ln := range strings.Split(string(b), "\n")[1:] {
			c := strings.Split(ln, "\t")
			if len(c) < 12 {
				continue
			}
			n := 0
			fmt.Sscan(c[8], &n)
			it := Item{c[0], c[1], c[2], c[3], c[4], c[5], c[6], c[7], n, c[9], c[10], c[11] == "1"}
			w.items = append(w.items, it)
			w.known[it.ArticleURL] = true
			w.known[it.PDFURL] = true
		}
	}
	if b, err := os.ReadFile(w.p("seen.txt")); err == nil {
		for _, u := range strings.Fields(string(b)) {
			w.known[u] = true
		}
	}
}

func (w *watcher) save() error {
	var sb strings.Builder
	sb.WriteString("first_seen\tsource\tkind\ttitle\tarticle_url\tpdf_url\tdate\tsha256\tbytes\tstatus\treason\tby_origin\n")
	for _, it := range w.items {
		o := "0"
		if it.ByOrigin {
			o = "1"
		}
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", it.FirstSeen, it.Source, it.Kind, tsvEsc(it.Title),
			it.ArticleURL, it.PDFURL, it.Date, it.SHA, it.Bytes, it.Status, tsvEsc(it.Reason), o)
	}
	tmp := w.itemsPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.itemsPath())
}

func (w *watcher) markSeen(u string) {
	w.known[u] = true
	f, err := os.OpenFile(w.p("seen.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, u)
		f.Close()
	}
}

func (w *watcher) stopFile(host string) string { return w.p("stop", host) }

func (w *watcher) hostStopped(host string) bool {
	_, err := os.Stat(w.stopFile(host))
	return err == nil
}

// fetch returns the body and content type. 403, 429, 503 and challenge pages
// write DATA/watch/stop/<host> and return errHostBlocked.
func (w *watcher) fetch(rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", err
	}
	host := u.Hostname()
	if w.hostStopped(host) {
		return nil, "", fmt.Errorf("%w: %s (delete %s to resume)", errHostBlocked, host, w.stopFile(host))
	}
	if d := w.gap - time.Since(w.last[host]); d > 0 {
		time.Sleep(d)
	}
	w.last[host] = time.Now()
	w.requests++
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", w.ua)
	req.Header.Set("Accept-Language", "ar,en;q=0.8")
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 60<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	block := func(why string) ([]byte, string, error) {
		msg := fmt.Sprintf("%s\n%s\n%s\n", time.Now().UTC().Format(time.RFC3339), rawURL, why)
		os.WriteFile(w.stopFile(host), []byte(msg), 0o644)
		return nil, "", fmt.Errorf("%w: %s: %s", errHostBlocked, host, why)
	}
	switch resp.StatusCode {
	case 200:
	case 403, 429, 503:
		return block(fmt.Sprintf("HTTP %d", resp.StatusCode))
	default:
		return nil, "", fmt.Errorf("HTTP %d for %s", resp.StatusCode, rawURL)
	}
	if strings.Contains(ct, "html") {
		head := strings.ToLower(string(b[:min(len(b), 8000)]))
		if strings.Contains(head, "<title>just a moment") || strings.Contains(head, "cf-chl") || strings.Contains(head, "attention required") ||
			(len(b) < 8000 && (strings.Contains(head, "captcha") || strings.Contains(head, "challenge"))) {
			return block("challenge page")
		}
	}
	return b, ct, nil
}

// ---- items -----------------------------------------------------------

var rePDF = regexp.MustCompile(`(?i)(?:href|src|data)\s*=\s*["']([^"']+?\.pdf(?:\?[^"']*)?)["']`)

func pdfLinks(base string, body []byte) []string {
	bu, _ := url.Parse(base)
	seen := map[string]bool{}
	var out []string
	for _, m := range rePDF.FindAllStringSubmatch(string(body), -1) {
		r, err := url.Parse(strings.TrimSpace(strings.ReplaceAll(m[1], "&amp;", "&")))
		if err != nil {
			continue
		}
		s := bu.ResolveReference(r).String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func nameTitle(pdfURL string) string {
	u, err := url.Parse(pdfURL)
	if err != nil {
		return pdfURL
	}
	n, _ := url.PathUnescape(filepath.Base(u.Path))
	return strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(strings.TrimSuffix(strings.TrimSuffix(n, ".pdf"), ".PDF")))
}

// addPDF downloads a PDF once (by sha256) and records it.
func (w *watcher) addPDF(src, title, articleURL, date, pdfURL string, byOrigin bool) {
	if w.known[pdfURL] {
		return
	}
	b, _, err := w.fetch(pdfURL)
	if err != nil {
		w.err(src, err)
		return
	}
	w.markSeen(pdfURL)
	if len(b) < 5 || string(b[:5]) != "%PDF-" {
		w.err(src, fmt.Errorf("not a PDF: %s", pdfURL))
		return
	}
	sha := sha256hex(b)
	path := w.p("pdf", sha+".pdf")
	if _, err := os.Stat(path); err != nil {
		os.WriteFile(path, b, 0o644)
	}
	if title == "" {
		title = nameTitle(pdfURL)
	}
	it := Item{FirstSeen: time.Now().UTC().Format(time.RFC3339), Source: src, Kind: "pdf", Title: title, ArticleURL: articleURL,
		PDFURL: pdfURL, Date: date, SHA: sha, Bytes: len(b), Status: "unjudged", ByOrigin: byOrigin}
	w.items = append(w.items, it)
	w.fresh = append(w.fresh, it)
}

func (w *watcher) addAlert(src, title, link, date string) {
	if w.known[link] {
		return
	}
	w.markSeen(link)
	it := Item{FirstSeen: time.Now().UTC().Format(time.RFC3339), Source: src, Kind: "alert", Title: title, ArticleURL: link, Date: date, Status: "alert"}
	w.items = append(w.items, it)
	w.fresh = append(w.fresh, it)
}

func (w *watcher) err(src string, e error) {
	w.errs = append(w.errs, fmt.Sprintf("%s: %v", src, e))
	fmt.Fprintf(os.Stderr, "watch %s: %v\n", src, e)
}

// ---- run -------------------------------------------------------------

var allSources = []struct {
	name string
	fn   func(*watcher)
}{
	{"cc", (*watcher).srcCC},
	{"labor", (*watcher).srcLabor},
	{"ppa", (*watcher).srcPPA},
	{"legalagenda", (*watcher).srcLegalAgenda},
	{"lbci", func(w *watcher) { w.srcSitemap("lbci", "https://www.lbcgroup.tv/newssitemap.xml") }},
	{"lebanon24", func(w *watcher) { w.srcSitemap("lebanon24", "https://www.lebanon24.com/newssitemap.xml") }},
	{"aljadeed", func(w *watcher) { w.srcSitemap("aljadeed", "https://www.aljadeed.tv/newssitemap.xml") }},
	{"gnews", (*watcher).srcGNews},
	{"pcm", (*watcher).srcPCM},
}

func watchRun(data, ua string, gap time.Duration, only string, judge, notify string, maxFetch int, first bool) error {
	if gap < 2*time.Second {
		gap = 2 * time.Second
	}
	w := newWatcher(data, ua, gap, judge, maxFetch)
	firstRun := !fileExists(w.itemsPath())
	want := map[string]bool{}
	for _, s := range strings.Split(only, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	perSource := map[string]int{}
	for _, s := range allSources {
		if len(want) > 0 && !want[s.name] {
			continue
		}
		before := len(w.fresh)
		s.fn(w)
		perSource[s.name] = len(w.fresh) - before
		w.save() // keep progress if a later source dies
	}
	if w.judge != "none" {
		w.judgeAll()
	}
	if err := w.save(); err != nil {
		return err
	}
	// report
	var names []string
	for n := range perSource {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("source %-12s new=%d\n", n, perSource[n])
	}
	fmt.Printf("requests=%d new_items=%d errors=%d\n", w.requests, len(w.fresh), len(w.errs))
	msg := w.message()
	if msg == "" {
		return nil
	}
	if firstRun && !first {
		fmt.Println("first run: baseline only, no notification")
		return nil
	}
	out := w.p("outbox", time.Now().UTC().Format("20060102T150405Z")+".txt")
	os.WriteFile(out, []byte(msg), 0o644)
	if notify != "" {
		cmd := exec.Command("sh", "-c", notify)
		cmd.Stdin = strings.NewReader(msg)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "notify failed: %v\n", err)
		}
	}
	return nil
}

// message builds the short DM text: a summary line (70 chars max), then bullets.
func (w *watcher) message() string {
	var off, alerts []Item
	for _, it := range w.fresh {
		switch {
		case it.Kind == "pdf" && (it.Status == "official" || it.Status == "unjudged"):
			off = append(off, it)
		case it.Kind == "alert":
			alerts = append(alerts, it)
		}
	}
	if len(off) == 0 && len(alerts) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Gazette watch: %d new official PDFs, %d alerts\n", len(off), len(alerts))
	for _, it := range off {
		fmt.Fprintf(&sb, "- [%s, %s] %s %s\n", it.Source, it.Status, it.Title, it.PDFURL)
	}
	for i, it := range alerts {
		if i == 8 {
			fmt.Fprintf(&sb, "- and %d more alerts in items.tsv\n", len(alerts)-8)
			break
		}
		fmt.Fprintf(&sb, "- alert [%s] %s %s\n", it.Source, it.Title, it.ArticleURL)
	}
	return sb.String()
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
