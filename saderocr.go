package main

import (
	"bytes"
	"fmt"
	"golang.org/x/image/draw"
	"image"
	"image/color"
	"image/png"
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
func (a *app) saderOCR(ys []int, types map[string]bool, workers, limit int, model, model2, idsCSV string) error {
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
	}
	// priority ids first (in the order given), then the rest from the shortest text to the longest
	prio := map[int]int{}
	for i, x := range strings.Split(idsCSV, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			prio[v] = i + 1
		}
	}
	npages := map[int]int{}
	for _, r := range jobs {
		out, _ := exec.Command("pdfinfo", filepath.Join(a.data, "raw", "sader", "pdf", fmt.Sprintf("%d.pdf", r.ID))).Output()
		for _, ln := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(ln, "Pages:") {
				npages[r.ID], _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "Pages:")))
			}
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		pi, pj := prio[jobs[i].ID], prio[jobs[j].ID]
		if (pi > 0) != (pj > 0) {
			return pi > 0
		}
		if pi > 0 {
			return pi < pj
		}
		return npages[jobs[i].ID] < npages[jobs[j].ID]
	})
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
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
		var gem, gem2 map[string]string
		var err2 error
		var wg2 sync.WaitGroup
		if a.model2 != "" {
			wg2.Add(1)
			go func() { // the two readers run side by side
				defer wg2.Done()
				gem2, err2 = readColumns(cs, a.model2)
			}()
		}
		gem, err = readColumns(cs, model)
		wg2.Wait()
		if err != nil {
			return res, fmt.Errorf("page %d: %v", i+1, err)
		}
		if err2 != nil {
			fmt.Fprintf(os.Stderr, "second reader %d page %d: %v\n", r.ID, i+1, err2)
			gem2 = nil
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

// headerStrip crops the running header band of a page image (full width) and
// upscales it 2x.
func headerStrip(pagePath, outPath string) (string, error) {
	f, err := os.Open(pagePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return "", err
	}
	b := src.Bounds()
	y0, y1 := headerBand(src)
	r := image.Rect(b.Min.X, b.Min.Y+y0, b.Max.X, b.Min.Y+y1)
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx()*2, r.Dy()*2))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, r, draw.Over, nil)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return "", err
	}
	return outPath, os.WriteFile(outPath, buf.Bytes(), 0o644)
}

// readColumns reads the crops in one call; if the model's output filter cuts it
// off (long, well-known texts: conventions, loan agreements), it reads each crop in
// short slices instead, one call per slice, side by side.
func readColumns(all []string, model string) (map[string]string, error) {
	res := map[string]string{}
	var cs []string
	for _, c := range all {
		if blankImage(c) { // an empty column (end of a text): nothing to read
			res[filepath.Base(c)] = ""
		} else {
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 {
		return res, nil
	}
	if r, _, err := geminiRead(cs, model); err == nil {
		for k, v := range r {
			res[k] = v
		}
		return res, nil
	}
	for _, c := range cs {
		txt, err := readAdaptive(c, model, 0)
		if err != nil {
			return nil, err
		}
		res[filepath.Base(c)] = txt
	}
	return res, nil
}

// readAdaptive reads an image; if the model's output filter cuts it off (long,
// well-known texts: conventions, loan agreements), the image is halved at a blank
// row and each half read on its own, down to a few lines. A piece that still
// cannot be read becomes a placeholder word that differs per model, so the two
// readers disagree there and the page image settles it.
func readAdaptive(path, model string, depth int) (string, error) {
	if blankImage(path) {
		return "", nil
	}
	if r, _, err := geminiRead([]string{path}, model); err == nil {
		return r[filepath.Base(path)], nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return "", err
	}
	if src.Bounds().Dy() < 300 || depth >= 5 {
		return "⟪؟ " + model + "⟫", nil
	}
	parts, err := sliceAt(path, src.Bounds().Dy()/2, false)
	if err != nil {
		return "", err
	}
	out := make([]string, len(parts))
	errs := make([]error, len(parts))
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			out[i], errs[i] = readAdaptive(p, model, depth+1)
			os.Remove(p)
		}(i, p)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return "", e
		}
	}
	return strings.Join(out, "\n"), nil
}

// sliceCrop cuts a column image into slices of about maxW-wide-proportional height,
// always at a blank row between lines, so no line is split.
func sliceCrop(path string, targetH int) ([]string, error) {
	return sliceAt(path, targetH, true)
}

// sliceAt is sliceCrop with the target in pixels when scaled is false.
func sliceAt(path string, targetH int, scaled bool) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	W, H := b.Dx(), b.Dy()
	if scaled {
		targetH = targetH * W / 600 // keep the same line count per slice for any resolution
	}
	ink := make([]int, H)
	for y := 0; y < H; y++ {
		for x := 0; x < W; x += 2 {
			r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			if (r+g+bl)/3 < 0x8000 {
				ink[y]++
			}
		}
	}
	var cuts []int
	y := 0
	for y+targetH < H {
		want := y + targetH
		best, bestInk := want, 1<<30
		for k := want - targetH/4; k <= want+targetH/4 && k < H; k++ {
			if ink[k] < bestInk {
				best, bestInk = k, ink[k]
			}
		}
		cuts = append(cuts, best)
		y = best
	}
	cuts = append(cuts, H)
	var out []string
	prev := 0
	for i, c := range cuts {
		r := image.Rect(b.Min.X, b.Min.Y+prev, b.Max.X, b.Min.Y+c)
		dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
		draw.Draw(dst, dst.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
		draw.Draw(dst, dst.Bounds(), src, r.Min, draw.Over)
		var buf bytes.Buffer
		if err := png.Encode(&buf, dst); err != nil {
			return nil, err
		}
		p := strings.TrimSuffix(path, ".png") + fmt.Sprintf(".s%02d.png", i+1)
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			return nil, err
		}
		out = append(out, p)
		prev = c
	}
	return out, nil
}

// blankImage reports an image with almost no ink (under 0.3% dark pixels).
func blankImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return false
	}
	b := src.Bounds()
	n, tot := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y += 2 {
		for x := b.Min.X; x < b.Max.X; x += 2 {
			r, g, bl, _ := src.At(x, y).RGBA()
			tot++
			if (r+g+bl)/3 < 0x8000 {
				n++
			}
		}
	}
	return tot > 0 && n*1000 < tot*3
}
