// lebgazette crawls the free copy of Lebanon's Official Gazette on the
// Presidency of the Council of Ministers site (pcm.gov.lb): year listings,
// issue contents, per-text viewers and the typeset page images.
//
// One request at a time, a gap between requests, stored once, never refetched.
// A 403/429/503 or a challenge page stops the crawl and writes DATA/BLOCKED.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const _ = 0

const defaultUA = "Mozilla/5.0 (X11; Linux x86_64) lebgazette/0.1 (+https://github.com/taktekhq/lebgazette)"

type app struct {
	f             *Fetcher
	base          string
	data          string
	part2         bool
	maxSpan       int
	model2        string
	heldElsewhere map[string]bool // kind|number|year of texts already held elsewhere (UL)
	kinds         []string
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	data := fs.String("data", "data", "directory for raw pages and index")
	base := fs.String("base", "https://pcm.gov.lb", "site root")
	ua := fs.String("ua", defaultUA, "User-Agent")
	gap := fs.Duration("gap", 1500*time.Millisecond, "minimum gap between requests")
	years := fs.String("years", "", "comma list of years (default: all found)")
	part2 := fs.Bool("part2", false, "also fetch Part Two (court notices, trademarks...) texts")
	maxSpan := fs.Int("maxspan", 120, "skip images of a text whose viewer page range is longer than this (the site has typos like 4219-4920); they are listed in index as suspect")
	kinds := fs.String("kinds", "", "only texts whose title starts with one of these words, e.g. قانون,مرسوم (default: all)")
	model := fs.String("model", defaultGemini, "Gemini model (via agy) for page reading and judging")
	sources := fs.String("sources", "", "watch: comma list of cc,labor,ppa,legalagenda,lbci,lebanon24,aljadeed,gnews,pcm (default all)")
	judge := fs.String("judge", "gemini", "watch: gemini (judge PDFs with agy) or none")
	notify := fs.String("notify", "", "watch: shell command that receives the message on stdin (e.g. a Slack DM)")
	notifyFirst := fs.Bool("notify-first", false, "watch: notify even on the first (baseline) run")
	maxFetch := fs.Int("max-new", 60, "watch: most new PDFs or articles taken per source per run")
	types := fs.String("types", "", "sader-ocr: only these types, e.g. قوانين,مراسيم")
	workers := fs.Int("workers", 6, "sader-ocr: texts read in parallel (agy image jobs: keep at most 6-8)")
	model2 := fs.String("model2", "", "sader-ocr: second Gemini model as another reader (e.g. gemini-4-argon-medium)")
	minDate := fs.String("min-date", "2026-10-01", "mizan: stop at items of issues older than this (YYYY-MM-DD)")
	dailyCap := fs.Int("daily-cap", 300, "mizan: most requests per day")
	out := fs.String("out", "bundle", "sader-bundle: output directory")
	dpi := fs.Int("dpi", 120, "sader-bundle: resolution of the page images")
	ids := fs.String("ids", "", "sader-ocr: read these SaderLex ids first, in this order; the rest follow from shortest to longest")
	skipF := fs.String("skip", "", "sader-ocr: TSV (kind, number, year) of texts to leave out, e.g. what the Lebanese University already has")
	limit := fs.Int("limit", 0, "stop after this many network fetches (0 = no limit)")
	fs.Parse(os.Args[2:])
	a := &app{f: NewFetcher(*ua, *gap, *data), base: strings.TrimRight(*base, "/"), data: *data, part2: *part2, maxSpan: *maxSpan}
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.TrimSpace(k); k != "" {
			a.kinds = append(a.kinds, k)
		}
	}
	ys := parseYears(*years)
	var err error
	switch cmd {
	case "discover":
		err = a.discover(ys)
	case "texts":
		err = a.texts(ys, *limit)
	case "images":
		err = a.images(ys, *limit)
	case "index":
		err = a.index()
	case "sader-tree":
		err = a.saderTree(newSaderFetcher(*ua, max(*gap, 3*time.Second), *data), ys)
	case "sader-pdfs":
		sf := newSaderFetcher(*ua, max(*gap, 3*time.Second), *data)
		err = a.saderPDFs(sf, ys, *limit)
		a.f = sf
	case "sader-ocr":
		ts := map[string]bool{}
		for _, t := range strings.Split(*types, ",") {
			if t = strings.TrimSpace(t); t != "" {
				ts[t] = true
			}
		}
		a.heldElsewhere = loadSkip(*skipF)
		err = a.saderOCR(ys, ts, *workers, *limit, *model, *model2, *ids)
	case "mizan":
		err = a.mizan(*ua, *gap, *minDate, *dailyCap)
	case "sader-headers":
		err = a.saderHeaders(ys, setOf(*types), *workers, *model)
	case "sader-bundle":
		err = a.saderBundle(*out, ys, setOf(*types), *dpi)
	case "watch":
		err = watchRun(*data, *ua, *gap, *sources, *judge, *notify, *maxFetch, *notifyFirst)
	case "pdf-read":
		if fs.NArg() < 1 {
			usage()
		}
		err = pdfRead(*data, fs.Arg(0), *model, *limit)
	case "ocr-pilot":
		err = ocrPilot(fs.Args(), filepath.Join(*data, "ocr-pilot"), *model)
	case "status":
		err = a.status(ys)
	default:
		usage()
	}
	fmt.Fprintf(os.Stderr, "network fetches: %d\n", a.f.Fetched)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: lebgazette discover|texts|images|index|status [-data DIR] [-years 2026,2025] [-part2] [-limit N] [-gap 1.5s]")
	os.Exit(2)
}

func parseYears(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

func want(ys []int, y int) bool {
	if len(ys) == 0 {
		return true
	}
	for _, v := range ys {
		if v == y {
			return true
		}
	}
	return false
}

func (a *app) page(id int, kind string) ([]byte, error) {
	rel := fmt.Sprintf("%s/%d.html", kind, id)
	u := fmt.Sprintf("%s/arabic/%s.aspx?pageid=%d", a.base, map[string]string{"year": "listingbydate", "issue": "subpg"}[kind], id)
	return a.f.Get(u, rel)
}

// discover reads the landing page, every year listing, and one issue page per
// year (its side menu lists every issue of the year, supplements included).
func (a *app) discover(ys []int) error {
	b, err := a.f.Get(a.base+"/arabic/landing.aspx?pageid=9", "landing.html")
	if err != nil {
		return err
	}
	nav := parseYearNav(b)
	ids := make([]int, 0, len(nav))
	for id := range nav {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	all := map[int]Issue{}
	for _, id := range ids {
		lb, err := a.page(id, "year")
		if err != nil {
			return err
		}
		li := parseListing(lb)
		if len(li) == 0 {
			continue
		}
		y := li[0].Year
		if !want(ys, y) {
			continue
		}
		ib, err := a.page(li[0].PageID, "issue")
		if err != nil {
			return err
		}
		m := parseMenu(ib)
		fmt.Fprintf(os.Stderr, "year %d: listing %d (%q) has %d issues in first page, menu has %d\n", y, id, nav[id], len(li), len(m))
		for _, is := range append(li, m...) {
			all[is.PageID] = is
		}
	}
	return a.writeIssues(all)
}

func (a *app) writeIssues(all map[int]Issue) error {
	// keep issues of years not rediscovered this run
	for _, is := range a.readIssues() {
		if _, ok := all[is.PageID]; !ok {
			all[is.PageID] = is
		}
	}
	var l []Issue
	for _, is := range all {
		l = append(l, is)
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].Date != l[j].Date {
			return l[i].Date > l[j].Date
		}
		return l[i].PageID > l[j].PageID
	})
	p := filepath.Join(a.data, "index", "issues.tsv")
	os.MkdirAll(filepath.Dir(p), 0o755)
	w, err := os.Create(p)
	if err != nil {
		return err
	}
	defer w.Close()
	fmt.Fprintln(w, "year\tnumber\tdate\tpageid\turl\tlabel")
	for _, is := range l {
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s/arabic/subpg.aspx?pageid=%d\t%s\n", is.Year, is.Number, is.Date, is.PageID, a.base, is.PageID, is.Label)
	}
	fmt.Fprintf(os.Stderr, "issues.tsv: %d issues\n", len(l))
	return nil
}

func (a *app) readIssues() []Issue {
	f, err := os.Open(filepath.Join(a.data, "index", "issues.tsv"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Issue
	sc := bufio.NewScanner(f)
	sc.Scan()
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if len(c) < 6 {
			continue
		}
		y, _ := strconv.Atoi(c[0])
		id, _ := strconv.Atoi(c[3])
		out = append(out, Issue{Year: y, Number: c[1], Date: c[2], PageID: id, Label: c[5]})
	}
	return out
}

func (a *app) eachIssue(ys []int, fn func(Issue) error) error {
	for _, is := range a.readIssues() {
		if want(ys, is.Year) {
			if err := fn(is); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *app) skip(key string) bool {
	return !a.part2 && strings.Contains("/"+key, "/part2/")
}

// keep applies -kinds to a title ("إعادة نشر" re-publications count as their kind).
func (a *app) keep(title string) bool {
	if len(a.kinds) == 0 {
		return true
	}
	t := strings.TrimSpace(strings.TrimPrefix(title, "إعادة نشر"))
	for _, k := range a.kinds {
		if strings.HasPrefix(t, k) {
			return true
		}
	}
	return false
}

// texts fetches each issue page, then each text's viewer page.
func (a *app) texts(ys []int, limit int) error {
	return a.eachIssue(ys, func(is Issue) error {
		ib, err := a.page(is.PageID, "issue")
		if err != nil {
			return err
		}
		for _, t := range parseContents(ib) {
			key, ok := textKey(t.URL)
			if !ok || a.skip(key) || !a.keep(t.Title) {
				continue
			}
			if _, err := a.f.Get(t.URL, "text/"+key); err != nil && err != errNotFound {
				return err
			}
			if limit > 0 && a.f.Fetched >= limit {
				return fmt.Errorf("limit reached")
			}
		}
		return nil
	})
}

func (a *app) suspect(v Viewer) bool { return v.End < v.Start || v.End-v.Start+1 > a.maxSpan }

// imgs lists the (url, rel) of every page image of a viewer.
func imgs(textURL, key string, v Viewer) (urls, rels []string) {
	bu, _ := url.Parse(textURL)
	ref, _ := url.Parse(v.ImgDir)
	dir := bu.ResolveReference(ref).String()
	rdir := path.Clean(path.Join(path.Dir(key), v.ImgDir))
	for n := v.Start; n <= v.End; n++ {
		urls = append(urls, fmt.Sprintf("%s%d.%s", dir, n, v.Ext))
		rels = append(rels, fmt.Sprintf("img/%s/%d.%s", rdir, n, v.Ext))
	}
	return
}

func (a *app) images(ys []int, limit int) error {
	return a.eachIssue(ys, func(is Issue) error {
		ib, err := a.page(is.PageID, "issue")
		if err != nil {
			return err
		}
		for _, t := range parseContents(ib) {
			key, ok := textKey(t.URL)
			if !ok || a.skip(key) || !a.keep(t.Title) {
				continue
			}
			vb, err := a.f.Get(t.URL, "text/"+key)
			if err != nil {
				if err == errNotFound {
					continue
				}
				return err
			}
			v, ok := parseViewer(vb)
			if !ok {
				continue
			}
			if a.suspect(v) {
				fmt.Fprintf(os.Stderr, "suspect page range %d-%d, skipped: %s\n", v.Start, v.End, t.URL)
				continue
			}
			urls, rels := imgs(t.URL, key, v)
			for i := range urls {
				if _, err := a.f.Get(urls[i], rels[i]); err != nil && err != errNotFound {
					return err
				}
				if limit > 0 && a.f.Fetched >= limit {
					return fmt.Errorf("limit reached")
				}
			}
		}
		return nil
	})
}

// index writes index/texts.tsv from what is on disk.
func (a *app) index() error {
	p := filepath.Join(a.data, "index", "texts.tsv")
	w, err := os.Create(p)
	if err != nil {
		return err
	}
	defer w.Close()
	fmt.Fprintln(w, "year\tissue\tdate\tsection\ttitle\tkey\tpcm_url\tissue_url\tpage_start\tpage_end\timg_dir\tpages_on_disk")
	n := 0
	for _, is := range a.readIssues() {
		ib, err := os.ReadFile(a.f.path(fmt.Sprintf("issue/%d.html", is.PageID)))
		if err != nil {
			continue
		}
		for _, t := range parseContents(ib) {
			key, ok := textKey(t.URL)
			if !ok {
				continue
			}
			ps, pe, dir, have := "", "", "", 0
			if vb, err := os.ReadFile(a.f.path("text/" + key)); err == nil {
				if v, ok := parseViewer(vb); ok {
					ps, pe = strconv.Itoa(v.Start), strconv.Itoa(v.End)
					if a.suspect(v) {
						dir = "SUSPECT"
					} else {
						_, rels := imgs(t.URL, key, v)
						dir = path.Dir(rels[0])
						for _, r := range rels {
							if a.f.Have(r) {
								have++
							}
						}
					}
				}
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s/arabic/subpg.aspx?pageid=%d\t%s\t%s\t%s\t%d\n",
				is.Year, is.Number, is.Date, t.Section, t.Title, key, t.URL, a.base, is.PageID, ps, pe, dir, have)
			n++
		}
	}
	fmt.Fprintf(os.Stderr, "texts.tsv: %d texts\n", n)
	return nil
}

func (a *app) status(ys []int) error {
	type c struct{ issues, fetchedIssues, texts, viewers, pages, pagesHave int }
	by := map[int]*c{}
	for _, is := range a.readIssues() {
		if !want(ys, is.Year) {
			continue
		}
		s := by[is.Year]
		if s == nil {
			s = &c{}
			by[is.Year] = s
		}
		s.issues++
		ib, err := os.ReadFile(a.f.path(fmt.Sprintf("issue/%d.html", is.PageID)))
		if err != nil {
			continue
		}
		s.fetchedIssues++
		for _, t := range parseContents(ib) {
			key, ok := textKey(t.URL)
			if !ok || a.skip(key) || !a.keep(t.Title) {
				continue
			}
			s.texts++
			if vb, err := os.ReadFile(a.f.path("text/" + key)); err == nil {
				if v, ok := parseViewer(vb); ok {
					s.viewers++
					if a.suspect(v) {
						continue
					}
					_, rels := imgs(t.URL, key, v)
					s.pages += len(rels)
					for _, r := range rels {
						if a.f.Have(r) {
							s.pagesHave++
						}
					}
				}
			}
		}
	}
	var yl []int
	for y := range by {
		yl = append(yl, y)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(yl)))
	fmt.Println("year issues issues_fetched texts viewers pages pages_on_disk")
	for _, y := range yl {
		s := by[y]
		fmt.Printf("%d %d %d %d %d %d %d\n", y, s.issues, s.fetchedIssues, s.texts, s.viewers, s.pages, s.pagesHave)
	}
	return nil
}

func setOf(csv string) map[string]bool {
	m := map[string]bool{}
	for _, t := range strings.Split(csv, ",") {
		if t = strings.TrimSpace(t); t != "" {
			m[t] = true
		}
	}
	return m
}
