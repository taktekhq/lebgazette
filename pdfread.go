package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// pdfRead reads a watched PDF twice, page by page: a vision model on the
// rendered page, and the PDF's own text layer (NFKC-normalized: Arabic PDFs
// store presentation forms). Where they agree the text is publishable; the
// disagreements are listed for the page image to settle. Nothing is rewritten.
func pdfRead(data, sha, model string, maxPages int) error {
	pdf := filepath.Join(data, "watch", "pdf", sha+".pdf")
	out := filepath.Join(data, "watch", "text", sha)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if b, err := exec.Command("pdftoppm", "-r", "150", "-png", pdf, filepath.Join(out, "page")).CombinedOutput(); err != nil {
		return fmt.Errorf("pdftoppm: %v %s", err, b)
	}
	pages, _ := filepath.Glob(filepath.Join(out, "page-*.png"))
	sort.Strings(pages)
	var sumA, cost float64
	n := 0
	for i, p := range pages {
		if maxPages > 0 && i >= maxPages {
			break
		}
		lt, err := exec.Command("pdftotext", "-f", fmt.Sprint(i+1), "-l", fmt.Sprint(i+1), pdf, "-").Output()
		if err != nil {
			return err
		}
		layer := norm.NFKC.String(string(lt))
		var v visionResult
		for try := 0; try < 2; try++ {
			v, err = visionRead(p, model)
			if err == nil && !refusal(v.Text) {
				break
			}
			err = fmt.Errorf("refusal or error")
		}
		base := strings.TrimSuffix(p, ".png")
		os.WriteFile(base+".layer.txt", []byte(layer), 0o644)
		if err != nil {
			fmt.Printf("page %d\tVISION FAILED\n", i+1)
			continue
		}
		os.WriteFile(base+".vision.txt", []byte(v.Text), 0o644)
		a, d := compare(normTokens(v.Text), normTokens(layer))
		fmt.Printf("page %d\twords=%d\tagree=%.3f\tdisagree=%d\t$%.4f\n", i+1, len(normTokens(v.Text)), a, d, v.Cost)
		sumA += a
		cost += v.Cost
		n++
	}
	if n > 0 {
		fmt.Printf("TOTAL pages=%d mean_agree=%.3f cost=$%.3f\n", n, sumA/float64(n), cost)
	}
	return nil
}
