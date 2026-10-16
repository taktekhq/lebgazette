package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mizan (mizanlebanon.com) shows each new Gazette issue within a day, but the text is
// its own OCR and has errors (columns bleed into each other). So everything here is
// stored as source=mizan, status=unverified: an index of what an issue contains, and
// a third reader to compare with once we hold official page images. Public pages
// only (robots.txt allows /latest and /law/), 5 s gap, a daily cap, stop on a block.

const mizanBase = "https://mizanlebanon.com"

type mizanItem struct {
	LawID   string `json:"law_id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Number  string `json:"number"`
	Date    string `json:"date"`
	Journal string `json:"official_journal_number"`
	JDate   string `json:"official_journal_date"`
	Body    string `json:"body"`
}

var reRSC = regexp.MustCompile(`self\.__next_f\.push\(\[1,("(?:[^"\\]|\\.)*")\]\)`)

func rscText(html []byte) string {
	var sb strings.Builder
	for _, m := range reRSC.FindAllSubmatch(html, -1) {
		var s string
		if json.Unmarshal(m[1], &s) == nil {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func mizanLatest(html []byte) ([]mizanItem, error) {
	t := rscText(html)
	i := strings.Index(t, `"initialLatest":`)
	if i < 0 {
		return nil, fmt.Errorf("no initialLatest payload")
	}
	var v struct {
		Laws []mizanItem `json:"laws"`
	}
	if err := json.NewDecoder(strings.NewReader(t[i+len(`"initialLatest":`):])).Decode(&v); err != nil {
		return nil, err
	}
	return v.Laws, nil
}

// mizanDoc returns the item payload of a /law/<id> page, or ok=false for a page without one.
func mizanDoc(html []byte) (mizanItem, bool) {
	t := rscText(html)
	p := strings.Index(t, `"preamble"`)
	if p < 0 {
		return mizanItem{}, false
	}
	s := strings.LastIndex(t[:p], `{"law_id"`)
	if s < 0 {
		return mizanItem{}, false
	}
	var it mizanItem
	if err := json.NewDecoder(strings.NewReader(t[s:])).Decode(&it); err != nil {
		return mizanItem{}, false
	}
	return it, it.LawID != ""
}

var reMizanID = regexp.MustCompile(`^gzt_(\d{4})_(\d+)_doc_(\d+)$`)

func (a *app) mizanBudget(cap int) (spend func() bool) {
	file := filepath.Join(a.data, "mizan", "budget.txt")
	today, used := time.Now().Format("2006-01-02"), 0
	if b, err := os.ReadFile(file); err == nil {
		var d string
		var n int
		if fmt.Sscan(string(b), &d, &n); d == today {
			used = n
		}
	}
	return func() bool {
		if used >= cap {
			return false
		}
		used++
		os.MkdirAll(filepath.Dir(file), 0o755)
		os.WriteFile(file, []byte(fmt.Sprintf("%s %d\n", today, used)), 0o644)
		return true
	}
}

// mizan lists /latest, then walks item pages downward by document id until the items
// are older than minDate (the first issue we do not hold from another source).
func (a *app) mizan(ua string, gap time.Duration, minDate string, dailyCap int) error {
	f := NewFetcher(ua, max(gap, 5*time.Second), a.data)
	spend := a.mizanBudget(dailyCap)
	if !spend() {
		return fmt.Errorf("daily cap of %d requests used", dailyCap)
	}
	// /latest is never cached: it changes
	os.Remove(f.path("mizan/latest.html"))
	lb, err := f.Get(mizanBase+"/latest", "mizan/latest.html")
	if err != nil {
		return err
	}
	list, err := mizanLatest(lb)
	if err != nil {
		return err
	}
	rows := a.readMizan()
	have := map[string]bool{}
	for _, r := range rows {
		have[r.LawID] = true
	}
	minDoc, token, year := 1<<31-1, "", ""
	record := func(it mizanItem, html []byte) {
		if have[it.LawID] {
			return
		}
		have[it.LawID] = true
		rows = append(rows, it)
		os.MkdirAll(filepath.Join(a.data, "mizan", "text"), 0o755)
		os.WriteFile(filepath.Join(a.data, "mizan", "text", it.LawID+".txt"), []byte(it.Body), 0o644)
	}
	for _, it := range list {
		if m := reMizanID.FindStringSubmatch(it.LawID); m != nil {
			d, _ := strconv.Atoi(m[3])
			if d < minDoc {
				minDoc, token, year = d, m[2], m[1]
			}
		}
	}
	fetchItem := func(tok string, id int) (mizanItem, bool, error) {
		lawID := fmt.Sprintf("gzt_%s_%s_doc_%d", year, tok, id)
		if !f.Have("mizan/item/"+lawID+".html") && !spend() {
			return mizanItem{}, false, fmt.Errorf("daily cap of %d requests used", dailyCap)
		}
		b, err := f.Get(mizanBase+"/law/"+lawID, "mizan/item/"+lawID+".html")
		if err != nil {
			return mizanItem{}, false, err
		}
		it, ok := mizanDoc(b)
		return it, ok, nil
	}
	// the 30 newest: fetch each page for its text
	n := 0
	for _, li := range list {
		m := reMizanID.FindStringSubmatch(li.LawID)
		if m == nil || have[li.LawID] {
			continue
		}
		id, _ := strconv.Atoi(m[3])
		it, ok, err := fetchItem(m[2], id)
		if err != nil {
			a.writeMizan(rows)
			return err
		}
		if ok {
			record(it, nil)
			n++
		}
	}
	// then walk down
	misses := 0
	for id := minDoc - 1; misses < 4; id-- {
		it, ok, err := fetchItem(token, id)
		if err == nil && !ok {
			// maybe the previous issue's token
			if tn, e := strconv.Atoi(token); e == nil {
				for k := 1; k <= 2 && !ok && err == nil; k++ {
					it, ok, err = fetchItem(strconv.Itoa(tn-k), id)
					if ok {
						token = strconv.Itoa(tn - k)
					}
				}
			}
		}
		if err != nil {
			a.writeMizan(rows)
			return err
		}
		if !ok {
			misses++
			continue
		}
		misses = 0
		if it.JDate != "" && it.JDate < minDate {
			break
		}
		record(it, nil)
		n++
	}
	fmt.Fprintf(os.Stderr, "mizan: %d items stored (this run), %d total\n", n, len(rows))
	return a.writeMizan(rows)
}

func (a *app) readMizan() []mizanItem {
	b, err := os.ReadFile(filepath.Join(a.data, "index", "mizan.tsv"))
	if err != nil {
		return nil
	}
	var out []mizanItem
	for _, ln := range strings.Split(string(b), "\n")[1:] {
		c := strings.Split(ln, "\t")
		if len(c) < 9 {
			continue
		}
		body, _ := os.ReadFile(filepath.Join(a.data, "mizan", "text", c[0]+".txt"))
		out = append(out, mizanItem{c[0], c[6], c[4], c[5], c[3], c[1], c[2], string(body)})
	}
	return out
}

func (a *app) writeMizan(rows []mizanItem) error {
	p := filepath.Join(a.data, "index", "mizan.tsv")
	os.MkdirAll(filepath.Dir(p), 0o755)
	var sb strings.Builder
	sb.WriteString("law_id\tjournal_ref\tjournal_date\tdate\ttype\tnumber\ttitle\tstatus\tsource\tbody_chars\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\t%s\t%s\t%s\tunverified\tmizan\t%d\n", r.LawID, r.Journal, r.JDate, r.Date, r.Type, r.Number, tsvEsc(r.Title), len([]rune(r.Body)))
	}
	return os.WriteFile(p, []byte(sb.String()), 0o644)
}
