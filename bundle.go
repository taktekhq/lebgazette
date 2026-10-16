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

var reHdrLine = regexp.MustCompile(`^[0-9٠-٩\s/\-ـ–.]{1,14}$`)

// stripHeaderLines drops running-header lines that leaked into the top of a column
// (the Gazette's name and issue line, the page number): only among its first lines.
func stripHeaderLines(col string) string {
	lines := strings.Split(col, "\n")
	i := 0
	for i < len(lines) && i < 3 {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || strings.Contains(ln, "الجريدة الرسمية") || (strings.Contains(ln, "العدد") && reHdrDate.MatchString(westernDigits(ln))) || reHdrLine.MatchString(ln) {
			i++
			continue
		}
		break
	}
	return strings.Join(lines[i:], "\n")
}

func wordKey(w string) string { return strings.Join(normTokens(w), "") }

var voteFold = strings.NewReplacer("ى", "ي", "ة", "ه", "ؤ", "و", "ئ", "ي")

// voteKey folds a word further than wordKey, to forgive Tesseract's small slips.
func voteKey(w string) string { return voteFold.Replace(wordKey(w)) }

// voteSide returns 1 or 2 when Tesseract backs reading one or two of a disputed span
// (a strictly larger share of its words appear in Tesseract's text, and at least 60%),
// 0 when it does not decide. Words are never written here: the winner is one of the
// two readings, as printed by that reader.
func voteSide(ra, rb []string, tess map[string]bool) int {
	score := func(r []string) float64 {
		if len(r) == 0 {
			return -1
		}
		in := 0
		for _, w := range r {
			if tess[voteKey(w)] {
				in++
			}
		}
		return float64(in) / float64(len(r))
	}
	sa, sb := score(ra), score(rb)
	switch {
	case sa >= 0.6 && sa > sb:
		return 1
	case sb >= 0.6 && sb > sa:
		return 2
	}
	return 0
}

// mergeReaders aligns two readings word by word. Where they agree the word is
// published as printed by reader 1; where they differ both readings are kept in a
// marked span for a person (or the page image) to settle. Nothing is rewritten.
func mergeReaders(g1, g2, tess string) (marked string, flagged int, words int, matched int, settled int) {
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
	tessSet := map[string]bool{}
	for _, x := range strings.Fields(tess) {
		tessSet[voteKey(x)] = true
	}
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
		// settle by majority: Tesseract backs one Gemini reading (folded comparison)
		switch voteSide(ra, rb, tessSet) {
		case 1:
			settled++
			if len(ra) > 0 {
				sb.WriteString(strings.Join(ra, " "))
			}
		case 2:
			settled++
			if len(rb) > 0 {
				sb.WriteString(strings.Join(rb, " "))
			}
		default:
			flagged++
			sb.WriteString(mkStart + strings.Join(ra, " ") + mkSep + strings.Join(rb, " ") + mkEnd)
		}
		if nl {
			sb.WriteString("\n")
		} else {
			sb.WriteString(" ")
		}
	}
	return strings.TrimSpace(sb.String()), flagged, n, matched, settled
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
	Settled    int     `json:"settled_spans"`
	Agreement  float64 `json:"agreement"`
	Status     string  `json:"status"`
}

type bundleText struct {
	Pages []struct {
		N    int      `json:"n"`
		Cols []string `json:"cols"`
	} `json:"pages"`
}

var rePageName = regexp.MustCompile(`^page-(\d+)\.png$`)

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

// bundleVersion changes whenever the merge or the metadata rules change, so every text is rewritten.
const bundleVersion = "2026-10-16c majority-vote, pcm issue dates, header lines stripped\n"

func (a *app) saderBundle(outDir string, ys []int, types map[string]bool, dpi int) error {
	force := false
	pcm := a.pcmIndex()
	rows := a.readSader()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	os.MkdirAll(outDir, 0o755)
	if err := copyFile(filepath.Join(a.data, "index", "issues.tsv"), filepath.Join(outDir, "issues.tsv")); err != nil {
		fmt.Fprintf(os.Stderr, "bundle: issues.tsv not copied: %v\n", err)
	}
	if v, _ := os.ReadFile(filepath.Join(outDir, ".version")); string(v) != bundleVersion {
		os.WriteFile(filepath.Join(outDir, ".version"), []byte(bundleVersion), 0o644) // a new merge rule: rebuild every text
		force = true
	}
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
		if !force && bundleFresh(src, filepath.Join(outDir, fmt.Sprint(r.ID))) {
			n++
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
	var words, matched, flagged, settled, npages int
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
			g1s := stripHeaderLines(string(g1))
			marked, words = mkStart+strings.TrimSpace(g1s)+mkSep+mkEnd, words+len(strings.Fields(g1s))
			flagged++
		} else {
			ts, _ := os.ReadFile(base + ".tess.txt")
			m, f, w, mt, st := mergeReaders(stripHeaderLines(string(g1)), stripHeaderLines(string(g2)), stripHeaderLines(string(ts)))
			marked, flagged, words, matched, settled = m, flagged+f, words+w, matched+mt, settled+st
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
		Issue: hdr.Issue, IssueDate: hdr.Date, PageFrom: hdr.Page, PageTo: hdrLast.Page, Words: words, Flagged: flagged, Settled: settled, Status: "official-scan"}
	if words > 0 {
		meta.Agreement = float64(matched) / float64(words)
	}
	// the issue number is read from the header; its date comes from PCM's list of issues, never from OCR
	meta.IssueDate = ""
	if d := a.pcmIssueDate(meta.Issue, yi, r.Date); d != "" {
		meta.IssueDate = d
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
	// pdftoppm pads page numbers (page-001.png); the site asks for page-1.png
	ents, _ := os.ReadDir(out)
	for _, e := range ents {
		if m := rePageName.FindStringSubmatch(e.Name()); m != nil {
			k, _ := strconv.Atoi(m[1])
			want := fmt.Sprintf("page-%d.png", k)
			if want != e.Name() {
				os.Rename(filepath.Join(out, e.Name()), filepath.Join(out, want))
			}
		}
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

// bundleFresh: the bundle is newer than everything it was made from.
func bundleFresh(src, out string) bool {
	mi, err := os.Stat(filepath.Join(out, "meta.json"))
	if err != nil {
		return false
	}
	for _, f := range []string{"done", "hdr.txt"} {
		if st, err := os.Stat(filepath.Join(src, f)); err == nil && st.ModTime().After(mi.ModTime()) {
			return false
		}
	}
	if pngs, _ := filepath.Glob(filepath.Join(out, "page-*.png")); len(pngs) == 0 {
		return false
	}
	return true
}

// pcmIssueDate finds the date of issue `number` in index/issues.tsv: the issue of the
// text's own year (or the next) dated on or after the text itself.
func (a *app) pcmIssueDate(number string, year int, textDate string) string {
	if number == "" {
		return ""
	}
	best := ""
	for _, is := range a.readIssues() {
		if is.Number != number || (is.Year != year && is.Year != year+1) || is.Date < textDate {
			continue
		}
		if best == "" || is.Date < best {
			best = is.Date
		}
	}
	return best
}
