package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// saderOCR reads SaderLex page scans: render at 300 dpi, split into columns,
// reader 1 = Gemini (agy) on the column crops, reader 2 = Tesseract (ara) on the
// same crops. Agreement is word by word; disagreements are for the page image to settle.
func (a *app) saderOCR(ys []int, types map[string]bool, workers, limit int, model, model2 string) error {
	a.model2 = model2
	rows := a.readSader()
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Year != rows[j].Year {
			return rows[i].Year > rows[j].Year
		}
		return saderRank(rows[i].Type) < saderRank(rows[j].Type)
	})
	var jobs []SaderRow
	for _, r := range rows {
		yi, _ := strconv.Atoi(r.Year)
		if !want(ys, yi) || (len(types) > 0 && !types[r.Type]) {
			continue
		}
		if !fileExists(filepath.Join(a.data, "raw", "sader", "pdf", fmt.Sprintf("%d.pdf", r.ID))) {
			continue
		}
		if fileExists(filepath.Join(a.data, "ocr", "sader", fmt.Sprint(r.ID), "done")) {
			continue
		}
		jobs = append(jobs, r)
		if limit > 0 && len(jobs) >= limit {
			break
		}
	}
	fmt.Fprintf(os.Stderr, "sader-ocr: %d texts to read, %d workers\n", len(jobs), workers)
	var mu sync.Mutex
	var sumA float64
	var n, failed int
	logf, _ := os.OpenFile(filepath.Join(a.data, "ocr", "sader-agree.tsv"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	defer logf.Close()
	ch := make(chan SaderRow)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range ch {
				res, err := a.saderOCRText(r, model)
				mu.Lock()
				if err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "sader-ocr %d: %v\n", r.ID, err)
				}
				for _, x := range res {
					fmt.Fprintf(logf, "%d\t%d\t%s\t%d\t%.3f\t%d\n", r.ID, x.page, x.crop, x.words, x.agree, x.diff)
					sumA += x.agree
					n++
				}
				mu.Unlock()
			}
		}()
	}
	for _, r := range jobs {
		ch <- r
	}
	close(ch)
	wg.Wait()
	if n > 0 {
		fmt.Printf("sader-ocr: crops=%d mean_agree=%.3f failed_texts=%d\n", n, sumA/float64(n), failed)
	}
	return nil
}

type cropRes struct {
	page  int
	crop  string
	words int
	agree float64
	diff  int
}

func (a *app) saderOCRText(r SaderRow, model string) ([]cropRes, error) {
	out := filepath.Join(a.data, "ocr", "sader", fmt.Sprint(r.ID))
	os.MkdirAll(out, 0o755)
	pdf := filepath.Join(a.data, "raw", "sader", "pdf", fmt.Sprintf("%d.pdf", r.ID))
	if b, err := exec.Command("pdftoppm", "-r", "300", "-gray", "-png", pdf, filepath.Join(out, "pg")).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %v %s", err, b)
	}
	pages, _ := filepath.Glob(filepath.Join(out, "pg-*.png"))
	sort.Strings(pages)
	var res []cropRes
	for i, pg := range pages {
		tag := fmt.Sprintf("p%02d", i+1)
		cs, err := cropsScale(pg, out, tag, 1)
		if err != nil {
			return res, err
		}
		gem, _, err := geminiRead(cs, model)
		if err != nil {
			return res, fmt.Errorf("page %d: %v", i+1, err)
		}
		var gem2 map[string]string
		if a.model2 != "" {
			if gem2, _, err = geminiRead(cs, a.model2); err != nil {
				fmt.Fprintf(os.Stderr, "second reader %d page %d: %v\n", r.ID, i+1, err)
				gem2 = nil
			}
		}
		for ci, c := range cs {
			base := strings.TrimSuffix(c, ".png")
			ts, terr := tesseractRead(c)
			if terr != nil {
				ts = ""
			}
			g := gem[filepath.Base(c)]
			os.WriteFile(base+".gemini.txt", []byte(g), 0o644)
			os.WriteFile(base+".tess.txt", []byte(ts), 0o644)
			if g2, ok := gem2[filepath.Base(c)]; ok {
				os.WriteFile(base+".gemini2.txt", []byte(g2), 0o644)
				ag2, d2 := compare(normTokens(g), normTokens(g2))
				res = append(res, cropRes{i + 1, fmt.Sprintf("c%d-g2", ci+1), len(normTokens(g)), ag2, d2})
			}
			ag, d := compare(normTokens(g), normTokens(ts))
			res = append(res, cropRes{i + 1, fmt.Sprintf("c%d", ci+1), len(normTokens(g)), ag, d})
		}
		for _, c := range cs {
			os.Remove(c)
		}
		os.Remove(pg)
	}
	os.WriteFile(filepath.Join(out, "done"), []byte(model+"\n"), 0o644)
	return res, nil
}
