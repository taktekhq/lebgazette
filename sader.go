package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SaderLex publishes page scans of the Official Gazette as PDFs, organised as a
// tree (year > type > ministry > text). Public pages only: no login, no search
// pages (robots.txt disallows them), a 3 s gap, stop on any 403/429/challenge.

const saderBase = "https://saderlex.com"

type saderNode struct {
	ID    int    `json:"id"`
	Title string `json:"t"`
	Leaf  bool   `json:"f"`
}

// SaderRow is one text (a leaf of the tree).
type SaderRow struct {
	ID     int
	Year   string
	Type   string // قوانين, مراسيم, قرارات, دستور
	Group  string // ministry or sub-type
	Kind   string // قانون, مرسوم, قرار ...
	Number string
	Date   string // ISO
	Title  string
}

var reSaderTitle = regexp.MustCompile(`^(.*?)\s*رقم\s*(\S+)\s+صادر\s+بتاريخ\s+(\d{1,2})/(\d{1,2})/(\d{4})\s*(.*)$`)

func parseSaderTitle(t string) (kind, number, date, rest string) {
	m := reSaderTitle.FindStringSubmatch(strings.TrimSpace(t))
	if m == nil {
		return "", "", "", t
	}
	d, _ := strconv.Atoi(m[3])
	mo, _ := strconv.Atoi(m[4])
	y, _ := strconv.Atoi(m[5])
	return strings.TrimSpace(m[1]), m[2], pad(y, 4) + "-" + pad(mo, 2) + "-" + pad(d, 2), strings.TrimSpace(m[6])
}

func newSaderFetcher(ua string, gap time.Duration, data string) *Fetcher {
	f := NewFetcher(ua, gap, data)
	f.Referer = saderBase + "/gazette?country=lb"
	return f
}

func (f *Fetcher) saderChildren(id int) ([]saderNode, error) {
	b, err := f.Get(fmt.Sprintf("%s/api/gazette/children?id=%d", saderBase, id), fmt.Sprintf("sader/tree/%d.json", id))
	if err != nil {
		return nil, err
	}
	var r struct {
		OK    bool        `json:"ok"`
		Items []saderNode `json:"items"`
	}
	if err := json.Unmarshal(b, &r); err != nil || !r.OK {
		return nil, fmt.Errorf("tree node %d: bad answer", id)
	}
	return r.Items, nil
}

var saderTypeOrder = []string{"قوانين", "مراسيم", "قرارات", "دستور"}

func saderRank(t string) int {
	for i, x := range saderTypeOrder {
		if x == t {
			return i
		}
	}
	return len(saderTypeOrder)
}

// saderTree walks the tree for the given years (all if empty) and rewrites index/sader.tsv.
func (a *app) saderTree(f *Fetcher, ys []int) error {
	top, err := f.saderChildren(0)
	if err != nil {
		return err
	}
	var rows []SaderRow
	for _, y := range top {
		yi, _ := strconv.Atoi(y.Title)
		if y.Leaf || !want(ys, yi) {
			continue
		}
		types, err := f.saderChildren(y.ID)
		if err != nil {
			return err
		}
		sort.SliceStable(types, func(i, j int) bool { return saderRank(types[i].Title) < saderRank(types[j].Title) })
		var walk func(id int, typ, group string) error
		walk = func(id int, typ, group string) error {
			kids, err := f.saderChildren(id)
			if err != nil {
				return err
			}
			for _, k := range kids {
				if strings.HasPrefix(k.Title, "الرجاء الاختيار") {
					continue
				}
				if k.Leaf {
					kind, num, date, _ := parseSaderTitle(k.Title)
					rows = append(rows, SaderRow{k.ID, y.Title, typ, group, kind, num, date, k.Title})
					continue
				}
				g := group
				if g == "" {
					g = k.Title
				} else {
					g += " / " + k.Title
				}
				if err := walk(k.ID, typ, g); err != nil {
					return err
				}
			}
			return nil
		}
		for _, t := range types {
			if t.Leaf {
				kind, num, date, _ := parseSaderTitle(t.Title)
				rows = append(rows, SaderRow{t.ID, y.Title, "", "", kind, num, date, t.Title})
				continue
			}
			if err := walk(t.ID, t.Title, ""); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "sader %s: %d texts so far\n", y.Title, len(rows))
	}
	return a.writeSader(rows, ys)
}

func (a *app) readSader() []SaderRow {
	fh, err := os.Open(filepath.Join(a.data, "index", "sader.tsv"))
	if err != nil {
		return nil
	}
	defer fh.Close()
	var out []SaderRow
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	sc.Scan()
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if len(c) < 8 {
			continue
		}
		id, _ := strconv.Atoi(c[0])
		out = append(out, SaderRow{id, c[1], c[2], c[3], c[4], c[5], c[6], c[7]})
	}
	return out
}

func (a *app) writeSader(rows []SaderRow, ys []int) error {
	fresh := map[string]bool{}
	for _, r := range rows {
		fresh[r.Year] = true
	}
	for _, r := range a.readSader() { // keep years not walked this time
		if !fresh[r.Year] {
			rows = append(rows, r)
		}
	}
	p := filepath.Join(a.data, "index", "sader.tsv")
	os.MkdirAll(filepath.Dir(p), 0o755)
	w, err := os.Create(p)
	if err != nil {
		return err
	}
	defer w.Close()
	fmt.Fprintln(w, "id\tyear\ttype\tgroup\tkind\tnumber\tdate\ttitle\tpdf_url")
	for _, r := range rows {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s/gazette/pdf/%d\n", r.ID, r.Year, r.Type, tsvEsc(r.Group), r.Kind, r.Number, r.Date, tsvEsc(r.Title), saderBase, r.ID)
	}
	fmt.Fprintf(os.Stderr, "sader.tsv: %d texts\n", len(rows))
	return nil
}

// saderPDFs fetches each text's PDF in order: laws, then decrees, then decisions.
func (a *app) saderPDFs(f *Fetcher, ys []int, limit int) error {
	rows := a.readSader()
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Year != rows[j].Year {
			return rows[i].Year > rows[j].Year
		}
		return saderRank(rows[i].Type) < saderRank(rows[j].Type)
	})
	for _, r := range rows {
		yi, _ := strconv.Atoi(r.Year)
		if !want(ys, yi) {
			continue
		}
		rel := fmt.Sprintf("sader/pdf/%d.pdf", r.ID)
		if f.Have(rel) {
			continue
		}
		if _, err := f.Get(fmt.Sprintf("%s/gazette/pdf/%d", saderBase, r.ID), rel); err != nil && err != errNotFound {
			return err
		}
		if b, err := os.ReadFile(f.path(rel)); err == nil && !strings.HasPrefix(string(b), "%PDF-") {
			os.Remove(f.path(rel))
			fmt.Fprintf(os.Stderr, "not a PDF, dropped: %d\n", r.ID)
		}
		if limit > 0 && f.Fetched >= limit {
			return fmt.Errorf("limit reached")
		}
	}
	return nil
}
