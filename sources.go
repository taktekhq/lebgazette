package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"html"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

func sha256hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

var reAnchorText = regexp.MustCompile(`(?s)<a\s[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)

// 1. Constitutional Council: every decision has its full PDF.
func (w *watcher) srcCC() {
	const src = "cc"
	b, _, err := w.fetch("https://www.cc.gov.lb/ar/القرارات/قرارات-دستورية-القوانين/")
	if err != nil {
		w.err(src, err)
		return
	}
	n := 0
	for _, u := range pdfLinks("https://www.cc.gov.lb/", b) {
		if !w.known[u] && n < w.maxFetch {
			n++
			w.addPDF(src, "المجلس الدستوري: "+nameTitle(u), "https://www.cc.gov.lb/ar/القرارات/قرارات-دستورية-القوانين/", "", u, true)
		}
	}
}

// 2a. Ministry of Labour: ministerial decisions and circulars.
func (w *watcher) srcLabor() {
	const src, page = "labor", "https://www.labor.gov.lb/AllLegalText.aspx?type=11"
	b, _, err := w.fetch(page)
	if err != nil {
		w.err(src, err)
		return
	}
	base, _ := url.Parse(page)
	n := 0
	for _, m := range reAnchorText.FindAllStringSubmatch(string(b), -1) {
		if !strings.Contains(strings.ToLower(m[1]), ".pdf") {
			continue
		}
		r, err := url.Parse(html.UnescapeString(m[1]))
		if err != nil {
			continue
		}
		u := base.ResolveReference(r).String()
		if w.known[u] || n >= w.maxFetch {
			continue
		}
		n++
		w.addPDF(src, "وزارة العمل: "+clean(m[2]), page, "", u, true)
	}
}

// 2b. Public Procurement Authority circulars: a listing of detail pages, each with its PDF.
func (w *watcher) srcPPA() {
	const src, page = "ppa", "https://www.ppa.gov.lb/ar/listing-pages/circulars"
	b, _, err := w.fetch(page)
	if err != nil {
		w.err(src, err)
		return
	}
	n := 0
	for _, m := range reAnchorText.FindAllStringSubmatch(string(b), -1) {
		if !strings.Contains(m[1], "/ar/pages/details/") || w.known[m[1]] || n >= w.maxFetch {
			continue
		}
		n++
		d, _, err := w.fetch(m[1])
		if err != nil {
			w.err(src, err)
			return
		}
		w.markSeen(m[1])
		for _, u := range pdfLinks(m[1], d) {
			w.addPDF(src, "هيئة الشراء العام: "+clean(m[2]), m[1], "", u, true)
		}
	}
}

type rssFeed struct {
	Items []struct {
		Title   string `xml:"title"`
		Link    string `xml:"link"`
		PubDate string `xml:"pubDate"`
		Content string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
		Desc    string `xml:"description"`
		Source  string `xml:"source"`
	} `xml:"channel>item"`
}

// 3. Legal Agenda: attaches official PDFs under /wp-content/uploads/.
func (w *watcher) srcLegalAgenda() {
	const src = "legalagenda"
	b, _, err := w.fetch("https://legal-agenda.com/feed/")
	if err != nil {
		w.err(src, err)
		return
	}
	var f rssFeed
	if err := xml.Unmarshal(b, &f); err != nil {
		w.err(src, err)
		return
	}
	n := 0
	for _, it := range f.Items {
		for _, u := range pdfLinks(it.Link, []byte(it.Content+it.Desc)) {
			if !strings.Contains(u, "/wp-content/uploads/") || w.known[u] || n >= w.maxFetch {
				continue
			}
			n++
			w.addPDF(src, clean(it.Title), it.Link, isoDate(it.PubDate), u, false)
		}
	}
}

type newsSitemap struct {
	URLs []struct {
		Loc   string `xml:"loc"`
		Date  string `xml:"http://www.google.com/schemas/sitemap-news/0.9 news>publication_date"`
		Title string `xml:"http://www.google.com/schemas/sitemap-news/0.9 news>title"`
	} `xml:"url"`
}

var reLawTitle = regexp.MustCompile(`قانون|مرسوم|قرار|المجلس الدستوري|الجريدة الرسمية|تعميم`)

// 4. News sites with a news sitemap: new articles whose title looks legal; collect attached PDFs.
func (w *watcher) srcSitemap(src, sitemap string) {
	b, _, err := w.fetch(sitemap)
	if err != nil {
		w.err(src, err)
		return
	}
	var sm newsSitemap
	if err := xml.Unmarshal(b, &sm); err != nil {
		w.err(src, err)
		return
	}
	n := 0
	for _, u := range sm.URLs {
		loc := strings.TrimSpace(u.Loc)
		if loc == "" || w.known[loc] || !reLawTitle.MatchString(u.Title) || n >= w.maxFetch {
			continue
		}
		n++
		a, _, err := w.fetch(loc)
		if err != nil {
			w.err(src, err)
			return
		}
		w.markSeen(loc)
		for _, p := range pdfLinks(loc, a) {
			w.addPDF(src, clean(u.Title), loc, isoDate(u.Date), p, false)
		}
	}
}

var gnewsQueries = []string{`"الجريدة الرسمية" لبنان`, `"صدر في الجريدة الرسمية"`, `"نص القانون"`, `"نص المرسوم"`, `"ينفرد بنشر"`}

// hands-only: outlets behind Cloudflare challenges. Alert text says so; never fetched from here.
var reForeign = regexp.MustCompile(`مصر|الجزائر|تونس|المغرب|الأردن|السعودية|الإمارات|العراق|سوريا|فلسطين|الكويت|ليبيا|قطر|عمان|Algeria|Egypt|Tunisia|Morocco`)

var byHandOutlets = []string{"ليبانون ديبايت", "النشرة", "ليبانون فايلز", "Lebanon Debate", "Elnashra", "Lebanon Files"}

// 5. Google News RSS: alerts only.
func (w *watcher) srcGNews() {
	const src = "gnews"
	for _, q := range gnewsQueries {
		u := "https://news.google.com/rss/search?q=" + url.QueryEscape(q+" when:2d") + "&hl=ar&gl=LB&ceid=LB:ar"
		b, _, err := w.fetch(u)
		if err != nil {
			w.err(src, err)
			return
		}
		var f rssFeed
		if err := xml.Unmarshal(b, &f); err != nil {
			w.err(src, err)
			continue
		}
		for _, it := range f.Items {
			t := clean(it.Title)
			if reForeign.MatchString(t) && !strings.Contains(t, "لبنان") {
				continue // a foreign law or gazette, not ours
			}
			for _, o := range byHandOutlets {
				if strings.Contains(it.Source, o) || strings.Contains(t, o) {
					t += " (open by hand: Cloudflare)"
					break
				}
			}
			w.addAlert(src, t, it.Link, isoDate(it.PubDate))
		}
	}
}

// 6. PCM: a tiny budget (3 per run, 30 per day). Skipped while the crawler's BLOCKED file exists.
func (w *watcher) srcPCM() {
	const src = "pcm"
	if fileExists(w.pcmBlock) {
		w.err(src, fmt.Errorf("skipped: %s exists (PCM challenged the crawler)", w.pcmBlock))
		return
	}
	budgetFile := w.p("pcm-budget.txt")
	today, used := time.Now().Format("2006-01-02"), 0
	if b, err := os.ReadFile(budgetFile); err == nil {
		var d string
		var n int
		if fmt.Sscan(string(b), &d, &n); d == today {
			used = n
		}
	}
	spend := func() bool {
		if used >= 30 {
			w.err(src, fmt.Errorf("daily PCM budget of 30 used"))
			return false
		}
		used++
		os.WriteFile(budgetFile, []byte(fmt.Sprintf("%s %d\n", today, used)), 0o644)
		return true
	}
	if !spend() {
		return
	}
	lb, _, err := w.fetch("https://pcm.gov.lb/arabic/listingbydate.aspx?pageid=26724")
	if err != nil {
		w.err(src, err)
		return
	}
	a := &app{data: w.data}
	have := map[int]bool{}
	for _, is := range a.readIssues() {
		have[is.PageID] = true
	}
	for _, is := range parseListing(lb) {
		if !have[is.PageID] {
			w.addAlert(src, "PCM: new Gazette issue on the free site: "+is.Label, fmt.Sprintf("https://pcm.gov.lb/arabic/subpg.aspx?pageid=%d", is.PageID), is.Date)
		}
	}
	if !spend() {
		return
	}
	cb, _, err := w.fetch("https://pcm.gov.lb/arabic/listingandcalendarnew.aspx?pageid=28")
	if err != nil {
		w.err(src, err)
		return
	}
	for _, m := range reAnchorText.FindAllStringSubmatch(string(cb), -1) {
		if strings.Contains(m[1], "subpg.aspx?pageid=") && !strings.Contains(m[1], "pageid=13090") {
			t := clean(m[2])
			if strings.Contains(t, "جلسة") || strings.Contains(t, "مقررات") {
				u := w.absURL("https://pcm.gov.lb/arabic/", m[1])
				w.addAlert(src, "PCM cabinet: "+t, u, "")
			}
		}
	}
}

func (w *watcher) absURL(base, ref string) string {
	b, _ := url.Parse(base)
	r, err := url.Parse(html.UnescapeString(ref))
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func isoDate(s string) string {
	s = strings.TrimSpace(s)
	for _, l := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "2006-01-02T15:04:05-07:00"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC().Format("2006-01-02")
		}
	}
	return s
}
