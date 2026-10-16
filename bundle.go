package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Reading markers inside a merged column: ‹reader 1|reader 2› where the readers
// disagree. U+E000 starts a span, U+E001 separates the readings, U+E002 ends it.
const (
	mkStart = ""
	mkSep   = ""
	mkEnd   = ""
)

func wordKey(w string) string { return strings.Join(normTokens(w), "") }

// mergeReaders aligns two readings word by word. Where they agree the word is
// published as printed by reader 1; where they differ both readings are kept in a
// marked span for a person (or the page image) to settle. Nothing is rewritten.
func mergeReaders(g1, g2 string) (marked string, flagged int, words int, matched int) {
	type w struct {
		s  string
		nl bool // reader 1 ended a line here
	}
	var a []w
	for _, ln := range strings.Split(g1, "\n") {
		f := strings.Fields(ln)
		for i, x := range f {
			a = append(a, w{x, i == len(f)-1})
		}
	}
	b := strings.Fields(g2)
	n, m := len(a), len(b)
	ka := make([]string, n)
	kb := make([]string, m)
	for i := range a {
		ka[i] = wordKey(a[i].s)
	}
	for j := range b {
		kb[j] = wordKey(b[j])
	}
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if ka[i] == kb[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var sb strings.Builder
	emit := func(s string, nl bool) {
		sb.WriteString(s)
		if nl {
			sb.WriteString("\n")
		} else {
			sb.WriteString(" ")
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		if i < n && j < m && ka[i] == kb[j] {
			emit(a[i].s, a[i].nl)
			matched++
			i++
			j++
			continue
		}
		// a disagreement run: advance until the next common word
		var ra, rb []string
		nl := false
		for (i < n || j < m) && !(i < n && j < m && ka[i] == kb[j]) {
			switch {
			case j >= m || (i < n && dp[i+1][j] >= dp[i][j+1]):
				ra = append(ra, a[i].s)
				nl = a[i].nl
				i++
			default:
				rb = append(rb, b[j])
				j++
			}
		}
		flagged++
		sb.WriteString(mkStart + strings.Join(ra, " ") + mkSep + strings.Join(rb, " ") + mkEnd)
		if nl {
			sb.WriteString("\n")
		} else {
			sb.WriteString(" ")
		}
	}
	return strings.TrimSpace(sb.String()), flagged, n, matched
}

var (
	reHdrIssue = regexp.MustCompile(`العدد\s*([0-9٠-٩]+)\s*(م\s*[0-9٠-٩]*|مكرر)?`)
	reHdrDate  = regexp.MustCompile(`([0-9٠-٩]{4})\s*/\s*([0-9٠-٩]{1,2})\s*/\s*([0-9٠-٩]{1,2})|([0-9٠-٩]{1,2})\s*/\s*([0-9٠-٩]{1,2})\s*/\s*([0-9٠-٩]{4})`)
	reDigits   = regexp.MustCompile(`[0-9٠-٩]{2,5}`)
)

func westernDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x660 && r <= 0x669 {
			return '0' + (r - 0x660)
		}
		return r
	}, s)
}

// Header is what a page's running header says.
type Header struct {
	Issue string `json:"issue"`
	Date  string `json:"date"`
	Page  int    `json:"page"`
}

func parseHeader(txt string) Header {
	var h Header
	t := westernDigits(txt)
	if m := reHdrIssue.FindStringSubmatch(t); m != nil {
		h.Issue = strings.ReplaceAll(m[1]+m[2], " ", "")
	}
	if m := reHdrDate.FindStringSubmatch(t); m != nil {
		if m[1] != "" {
			y, _ := strconv.Atoi(m[1])
			mo, _ := strconv.Atoi(m[2])
			d, _ := strconv.Atoi(m[3])
			h.Date = pad(y, 4) + "-" + pad(mo, 2) + "-" + pad(d, 2)
		} else {
			y, _ := strconv.Atoi(m[6])
			mo, _ := strconv.Atoi(m[5])
			d, _ := strconv.Atoi(m[4])
			h.Date = pad(y, 4) + "-" + pad(mo, 2) + "-" + pad(d, 2)
		}
	}
	// the page number is the digit run that is not part of the issue or the date
	rest := reHdrDate.ReplaceAllString(reHdrIssue.ReplaceAllString(t, " "), " ")
	if m := reDigits.FindAllString(rest, -1); len(m) > 0 {
		h.Page, _ = strconv.Atoi(m[len(m)-1])
	}
	return h
}

// saderHeaders reads the running header of the first and last page of every text
// that has a PDF, one Gemini call per text (header strips only).
func (a *app) saderHeaders(ys []int, types map[string]bool, workers int, model string) error {
	rows := a.readSader()
	var jobs []SaderRow
	for _, r := range rows {
		yi, _ := strconv.Atoi(r.Year)
		if !want(ys, yi) || (len(types) > 0 && !types[r.Type]) {
			continue
		}
		if !fileExists(filepath.Join(a.data, "raw", "sader", "pdf", fmt.Sprintf("%d.pdf", r.ID))) || fileExists(filepath.Join(a.data, "ocr", "sader", fmt.Sprint(r.ID), "hdr.txt")) {
			continue
		}
		jobs = append(jobs, r)
	}
	fmt.Fprintf(os.Stderr, "sader-headers: %d texts\n", len(jobs))
	ch := make(chan SaderRow)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range ch {
				if err := a.saderHeader(r, model); err != nil {
					fmt.Fprintf(os.Stderr, "sader-headers %d: %v\n", r.ID, err)
				}
			}
		}()
	}
	for _, r := range jobs {
		ch <- r
	}
	close(ch)
	wg.Wait()
	return nil
}

func (a *app) saderHeader(r SaderRow, model string) error {
	out := filepath.Join(a.data, "ocr", "sader", fmt.Sprint(r.ID))
	tmp, err := os.MkdirTemp("", "gzhdr")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	pdf := filepath.Join(a.data, "raw", "sader", "pdf", fmt.Sprintf("%d.pdf", r.ID))
	info, _ := exec.Command("pdfinfo", pdf).Output()
	npages := 1
	for _, ln := range strings.Split(string(info), "\n") {
		if strings.HasPrefix(ln, "Pages:") {
			npages, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "Pages:")))
		}
	}
	var files []string
	for k, pg := range []int{1, npages} {
		if k == 1 && npages == 1 {
			break
		}
		base := filepath.Join(tmp, fmt.Sprintf("pg%d", k))
		if b, err := exec.Command("pdftoppm", "-r", "200", "-gray", "-png", "-singlefile", "-f", fmt.Sprint(pg), "-l", fmt.Sprint(pg), pdf, base).CombinedOutput(); err != nil {
			return fmt.Errorf("pdftoppm: %v %s", err, b)
		}
		h, err := headerStrip(base+".png", filepath.Join(tmp, fmt.Sprintf("hdr%d.png", k+1)))
		if err != nil {
			return err
		}
		files = append(files, h)
	}
	res, _, err := geminiRead(files, model)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, f := range files {
		fmt.Fprintf(&sb, "=== %s ===\n%s\n", filepath.Base(f), res[filepath.Base(f)])
	}
	os.MkdirAll(out, 0o755)
	return os.WriteFile(filepath.Join(out, "hdr.txt"), []byte(sb.String()), 0o644)
}

// bundle writes everything the site needs for each fully read text.
type bundleMeta struct {
	ID         int     `json:"id"`
	Source     string  `json:"source"`
	SourceName string  `json:"source_name"`
	SourceURL  string  `json:"source_url"`
	Kind       string  `json:"kind"`
	Number     string  `json:"number"`
	Date       string  `json:"date"`
	Year       int     `json:"year"`
	Title      string  `json:"title"`
	Group      string  `json:"group"`
	Pages      int     `json:"pages"`
	Issue      string  `json:"gazette_issue"`
	IssueDate  string  `json:"gazette_issue_date"`
	PageFrom   int     `json:"gazette_page_from"`
	PageTo     int     `json:"gazette_page_to"`
	PCMURL     string  `json:"pcm_url"`
	Words      int     `json:"words"`
	Flagged    int     `json:"flagged_spans"`
	Agreement  float64 `json:"agreement"`
	Status     string  `json:"status"`
}

type bundleText struct {
	Pages []struct {
		N    int      `json:"n"`
		Cols []string `json:"cols"`
	} `json:"pages"`
}

var rePCMKey = regexp.MustCompile(`^(?:إعادة نشر\s+)?(قانون|مرسوم|قرار)[^0-9]*رقم\s*([0-9]+)`)

func (a *app) pcmIndex() map[string][3]string {
	out := map[string][3]string{}
	b, err := os.ReadFile(filepath.Join(a.data, "index", "texts.tsv"))
	if err != nil {
		return out
	}
	for _, ln := range strings.Split(string(b), "\n")[1:] {
		c := strings.Split(ln, "\t")
		if len(c) < 8 {
			continue
		}
		if m := rePCMKey.FindStringSubmatch(c[4]); m != nil {
			out[m[1]+"|"+m[2]+"|"+c[0]] = [3]string{c[1], c[6], c[7]} // issue, pcm url, issue url
		}
	}
	return out
}

func (a *app) saderBundle(outDir string, ys []int, types map[string]bool, dpi int) error {
	pcm := a.pcmIndex()
	rows := a.readSader()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	n := 0
	for _, r := range rows {
		yi, _ := strconv.Atoi(r.Year)
		if !want(ys, yi) || (len(types) > 0 && !types[r.Type]) {
			continue
		}
		src := filepath.Join(a.data, "ocr", "sader", fmt.Sprint(r.ID))
		if !fileExists(filepath.Join(src, "done")) {
			continue
		}
		if err := a.bundleOne(r, src, filepath.Join(outDir, fmt.Sprint(r.ID)), pcm, dpi); err != nil {
			fmt.Fprintf(os.Stderr, "bundle %d: %v\n", r.ID, err)
			continue
		}
		n++
	}
	fmt.Fprintf(os.Stderr, "bundle: %d texts written to %s\n", n, outDir)
	return nil
}

func (a *app) bundleOne(r SaderRow, src, out string, pcm map[string][3]string, dpi int) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	pdf := filepath.Join(a.data, "raw", "sader", "pdf", fmt.Sprintf("%d.pdf", r.ID))
	var bt bundleText
	var words, matched, flagged, npages int
	g1s, _ := filepath.Glob(filepath.Join(src, "p*.c*.gemini.txt"))
	sort.Strings(g1s)
	pageCols := map[int][]string{}
	for _, g1p := range g1s {
		base := strings.TrimSuffix(g1p, ".gemini.txt")
		pn, _ := strconv.Atoi(filepath.Base(base)[1:3])
		g1, _ := os.ReadFile(g1p)
		g2, err := os.ReadFile(base + ".gemini2.txt")
		var marked string
		if err != nil {
			// no second reader for this column: nothing is confirmed, show it whole as one flagged span
			marked, words = mkStart+strings.TrimSpace(string(g1))+mkSep+mkEnd, words+len(strings.Fields(string(g1)))
			flagged++
		} else {
			m, f, w, mt := mergeReaders(string(g1), string(g2))
			marked, flagged, words, matched = m, flagged+f, words+w, matched+mt
		}
		pageCols[pn] = append(pageCols[pn], marked)
		if pn > npages {
			npages = pn
		}
	}
	for pn := 1; pn <= npages; pn++ {
		bt.Pages = append(bt.Pages, struct {
			N    int      `json:"n"`
			Cols []string `json:"cols"`
		}{pn, pageCols[pn]})
	}
	hdr := Header{}
	var hdrLast Header
	if b, err := os.ReadFile(filepath.Join(src, "hdr.txt")); err == nil {
		parts := reMark.Split(string(b), -1) // text before, then one block per header file
		blocks := parts[1:]
		if len(blocks) > 0 {
			hdr = parseHeader(blocks[0])
			hdrLast = hdr
		}
		if len(blocks) > 1 {
			hdrLast = parseHeader(blocks[1])
		}
	}
	yi, _ := strconv.Atoi(r.Year)
	meta := bundleMeta{ID: r.ID, Source: "saderlex", SourceName: "SaderLex", SourceURL: fmt.Sprintf("%s/gazette/pdf/%d", saderBase, r.ID),
		Kind: r.Kind, Number: r.Number, Date: r.Date, Year: yi, Title: r.Title, Group: r.Group, Pages: npages,
		Issue: hdr.Issue, IssueDate: hdr.Date, PageFrom: hdr.Page, PageTo: hdrLast.Page, Words: words, Flagged: flagged, Status: "official-scan"}
	if words > 0 {
		meta.Agreement = float64(matched) / float64(words)
	}
	kind1 := strings.Fields(r.Kind + " ")[0]
	if v, ok := pcm[kind1+"|"+r.Number+"|"+r.Year]; ok {
		meta.PCMURL = v[2]
		if meta.Issue == "" {
			meta.Issue = v[0]
		}
	}
	mb, _ := json.MarshalIndent(meta, "", " ")
	tb, _ := json.Marshal(bt)
	os.WriteFile(filepath.Join(out, "meta.json"), mb, 0o644)
	os.WriteFile(filepath.Join(out, "text.json"), tb, 0o644)
	if err := copyFile(pdf, filepath.Join(out, "source.pdf")); err != nil {
		return err
	}
	if b, err := exec.Command("pdftoppm", "-r", strconv.Itoa(dpi), "-gray", "-png", pdf, filepath.Join(out, "page")).CombinedOutput(); err != nil {
		return fmt.Errorf("pdftoppm: %v %s", err, b)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	o, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer o.Close()
	_, err = io.Copy(o, in)
	return err
}
