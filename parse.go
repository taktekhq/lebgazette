package main

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Issue is one Gazette issue (or supplement) as listed on the PCM site.
type Issue struct {
	PageID int    // PCM subpg pageid
	Number string // as printed: "39", "35م", "10 م1"
	Date   string // ISO, from the label
	Year   int
	Label  string
}

var (
	reMenu    = regexp.MustCompile(`<div class="rightMenuLevel1"><a href="/arabic/subpg\.aspx\?pageid=(\d+)">([^<]*)</a>`)
	reListing = regexp.MustCompile(`<h3><a href="/arabic/subpg\.aspx\?pageid=(\d+)"[^>]*>([^<]*)</a></h3>`)
	reLabel   = regexp.MustCompile(`عدد\s*(.+?)\s*تاريخ\s*(\d{1,2})-(\d{1,2})-(\d{4})`)
	reYearNav = regexp.MustCompile(`href="(/arabic/listingbydate\.aspx\?pageid=(\d+))"[^>]*>\s*([^<]*(?:سنة|السنة)[^<]*)<`)
)

func parseLabel(pageid, label string) (Issue, bool) {
	label = strings.TrimSpace(html.UnescapeString(label))
	m := reLabel.FindStringSubmatch(label)
	if m == nil {
		return Issue{}, false
	}
	id, _ := strconv.Atoi(pageid)
	d, _ := strconv.Atoi(m[2])
	mo, _ := strconv.Atoi(m[3])
	y, _ := strconv.Atoi(m[4])
	return Issue{PageID: id, Number: strings.Join(strings.Fields(m[1]), " "), Year: y,
		Date: pad(y, 4) + "-" + pad(mo, 2) + "-" + pad(d, 2), Label: strings.Join(strings.Fields(label), " ")}, true
}

func pad(n, w int) string {
	s := strconv.Itoa(n)
	for len(s) < w {
		s = "0" + s
	}
	return s
}

func parseMenu(b []byte) []Issue {
	var out []Issue
	for _, m := range reMenu.FindAllStringSubmatch(string(b), -1) {
		if is, ok := parseLabel(m[1], m[2]); ok {
			out = append(out, is)
		}
	}
	return out
}

func parseListing(b []byte) []Issue {
	var out []Issue
	for _, m := range reListing.FindAllStringSubmatch(string(b), -1) {
		if is, ok := parseLabel(m[1], m[2]); ok {
			out = append(out, is)
		}
	}
	return out
}

// YearListings returns the listing pageids (and link text) found in the
// Gazette landing page's menu.
func parseYearNav(b []byte) map[int]string {
	out := map[int]string{}
	for _, m := range reYearNav.FindAllStringSubmatch(string(b), -1) {
		id, _ := strconv.Atoi(m[2])
		out[id] = strings.TrimSpace(html.UnescapeString(m[3]))
	}
	return out
}

// Text is one entry of an issue's table of contents.
type Text struct {
	Section string
	Title   string
	URL     string
}

var (
	reAnchor = regexp.MustCompile(`(?s)<a\s[^>]*href="([^"]*archive/[^"]*)"[^>]*>(.*?)</a>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]+>`)
	reP      = regexp.MustCompile(`(?s)<p[ >].*?</p>`)
)

func clean(s string) string {
	s = reTag.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// parseContents reads an issue page's table of contents: section headings are
// paragraphs without a link; every archive link under one is a text.
func parseContents(b []byte) []Text {
	s := string(b)
	i := strings.Index(s, `class="contentContainer"`)
	if i < 0 {
		return nil
	}
	s = s[i:]
	if j := strings.Index(s, "Page Content End"); j > 0 {
		s = s[:j]
	}
	section := ""
	var out []Text
	for _, p := range reP.FindAllString(s, -1) {
		as := reAnchor.FindAllStringSubmatch(p, -1)
		if len(as) == 0 {
			if t := clean(p); t != "" && !strings.Contains(t, "فهرس العدد") {
				section = t
			}
			continue
		}
		for _, a := range as {
			out = append(out, Text{Section: section, Title: clean(a[2]), URL: html.UnescapeString(a[1])})
		}
	}
	return out
}

// Viewer is a text's small image viewer page.
type Viewer struct {
	Start, End int
	ImgDir     string // folder holding the page images, relative to the viewer
	Ext        string
}

var (
	reStart = regexp.MustCompile(`var pagestart=(\d+);`)
	reEnd   = regexp.MustCompile(`var pageend=(\d+);`)
	reImg   = regexp.MustCompile(`<img[^>]*src="([^"]*?)(\d+)\.(gif|jpg|png)"`)
)

func parseViewer(b []byte) (Viewer, bool) {
	s := string(b)
	a, c, im := reStart.FindStringSubmatch(s), reEnd.FindStringSubmatch(s), reImg.FindStringSubmatch(s)
	if a == nil || c == nil || im == nil {
		return Viewer{}, false
	}
	st, _ := strconv.Atoi(a[1])
	en, _ := strconv.Atoi(c[1])
	return Viewer{Start: st, End: en, ImgDir: im[1], Ext: im[3]}, true
}

// textKey turns .../archive/j2026/j39/wfn/n70.htm into "j2026/j39/wfn/n70.htm".
func textKey(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	i := strings.Index(u.Path, "/archive/")
	if i < 0 {
		return "", false
	}
	return strings.TrimPrefix(u.Path[i:], "/archive/"), true
}
