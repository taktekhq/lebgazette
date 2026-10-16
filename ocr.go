package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
)

// Two independent readings of a page image: Tesseract (ara) and a vision
// model through `claude -p`. Models transcribe; they never rewrite.

const visionPrompt = `Read the image file %s with the Read tool. It is one column of a page of the Lebanese Official Gazette, in Arabic, set in typeface at low resolution. Transcribe ALL the text exactly as printed, top to bottom, one printed paragraph (or table row, cells separated by " | ") per line. Do not correct, normalize, modernize or reword anything. Keep digits exactly as printed. Always give your best reading of every word, even if small. Output only the transcription, nothing else.`

// refusal reports text that is a model's excuse, not a transcription.
func refusal(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return true
	}
	for _, w := range []string{"i can't", "i cannot", "i haven't", "i'm unable", "i am unable", "i couldn't", "unable to transcribe", "as an ai"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// crops splits a page into its columns (right column first, as read), drops
// the running header, upscales 2x into RGB PNGs (a grayscale PNG made
// tesseract return nothing) and returns their paths.
func crops(gifPath, workDir, tag string) ([]string, error) {
	return cropsScale(gifPath, workDir, tag, 2)
}

// cropsScale is crops with a chosen upscale (1 for already high-resolution scans).
func cropsScale(imgPath, workDir, tag string, scale int) ([]string, error) {
	f, err := os.Open(imgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	top := b.Dy() * 34 / 934 // running header: "الجريدة الرسمية - العدد N - date" and the page number
	dark := func(x, y int) bool {
		r, g, bl, _ := src.At(x, y).RGBA()
		return (r+g+bl)/3 < 0x8000
	}
	prof := make([]int, b.Dx())
	maxv := 0
	for x := 0; x < b.Dx(); x++ {
		for y := top; y < b.Dy(); y++ {
			if dark(b.Min.X+x, b.Min.Y+y) {
				prof[x]++
			}
		}
		maxv = max(maxv, prof[x])
	}
	// widest run of near-empty columns in the middle band
	lo, hi := b.Dx()*35/100, b.Dx()*65/100
	bestS, bestL, runS := 0, 0, -1
	for x := lo; x <= hi; x++ {
		if prof[x] <= max(3, maxv/20) {
			if runS < 0 {
				runS = x
			}
			if l := x - runS + 1; l > bestL {
				bestS, bestL = runS, l
			}
		} else {
			runS = -1
		}
	}
	// a ruled table spans the page: its gutters are not column gutters
	rules := 0
	for y := top; y < b.Dy(); y++ {
		n := 0
		for x := 0; x < b.Dx(); x++ {
			if dark(b.Min.X+x, b.Min.Y+y) {
				n++
			}
		}
		if n*10 > b.Dx()*8 {
			rules++
		}
	}
	rects := []image.Rectangle{image.Rect(b.Min.X, b.Min.Y+top, b.Max.X, b.Max.Y)}
	if bestL >= 6 && rules < 4 {
		mid := b.Min.X + bestS + bestL/2
		rects = []image.Rectangle{
			image.Rect(mid, b.Min.Y+top, b.Max.X, b.Max.Y), // right column
			image.Rect(b.Min.X, b.Min.Y+top, mid, b.Max.Y), // left column
		}
	}
	var paths []string
	for i, r := range rects {
		dst := image.NewRGBA(image.Rect(0, 0, r.Dx()*scale, r.Dy()*scale))
		draw.Draw(dst, dst.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, r, draw.Over, nil)
		var buf bytes.Buffer
		if err := png.Encode(&buf, dst); err != nil {
			return nil, err
		}
		p := filepath.Join(workDir, fmt.Sprintf("%s.c%d.png", tag, i+1))
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func tesseractRead(png string) (string, error) {
	out, err := exec.Command("tesseract", png, "-", "-l", "ara", "--psm", "6").Output()
	return string(out), err
}

type visionResult struct {
	Text string
	Cost float64
	Secs float64
}

// normTokens splits into words for comparison only (the stored readings stay
// untouched): drops tatweel, diacritics, bidi marks and punctuation, maps
// Arabic-Indic digits to Western.
func normTokens(s string) []string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0x640 || (r >= 0x64B && r <= 0x652) || r == 0x200E || r == 0x200F || r == 0x202A || r == 0x202B || r == 0x202C || r == 0x2066 || r == 0x2067 || r == 0x2069:
		case r >= 0x660 && r <= 0x669:
			b.WriteRune('0' + (r - 0x660))
		case r >= 0x6F0 && r <= 0x6F9:
			b.WriteRune('0' + (r - 0x6F0))
		case r == 0x623 || r == 0x625 || r == 0x622:
			b.WriteRune(0x627) // alef variants only for matching
		case strings.ContainsRune(".,;:!?()[]«»\"'-–—ـ،؛؟/_*", r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Fields(b.String())
}

// compare returns matched/max(len) by LCS over words, and the disagreeing words of a.
func compare(a, b []string) (agree float64, diffA int) {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return 0, n
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	l := dp[0][0]
	return float64(l) / float64(max(n, m)), n - l
}

// ocrPilot reads the given page images, column by column, with both readers.
func ocrPilot(pages []string, outDir, model string) error {
	os.MkdirAll(outDir, 0o755)
	var totCost, totSecs, sumAgree float64
	var done, failed, singles int
	var sumWords, sumDis int
	for _, p := range pages {
		tag := filepath.Base(filepath.Dir(filepath.Dir(p))) + "_" + filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p)))) + "_" + strings.TrimSuffix(filepath.Base(p), ".gif")
		cs, err := crops(p, outDir, tag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crops %s: %v\n", p, err)
			continue
		}
		if len(cs) == 1 {
			singles++
		}
		for ci, c := range cs {
			tess, err := tesseractRead(c)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tesseract %s: %v\n", c, err)
				continue
			}
			var v visionResult
			for try := 0; try < 2; try++ {
				v, err = visionRead(c, model)
				if err == nil && !refusal(v.Text) {
					break
				}
				err = fmt.Errorf("refusal or error")
			}
			if err != nil {
				fmt.Printf("%s.c%d\tVISION FAILED\n", tag, ci+1)
				failed++
				continue
			}
			base := filepath.Join(outDir, fmt.Sprintf("%s.c%d", tag, ci+1))
			os.WriteFile(base+".tess.txt", []byte(tess), 0o644)
			os.WriteFile(base+".vision.txt", []byte(v.Text), 0o644)
			a, d := compare(normTokens(v.Text), normTokens(tess))
			nw := len(normTokens(v.Text))
			fmt.Printf("%s.c%d\twords=%d\tagree=%.3f\tdisagree_words=%d\tcost=$%.4f\t%.0fs\n", tag, ci+1, nw, a, d, v.Cost, v.Secs)
			totCost += v.Cost
			totSecs += v.Secs
			sumAgree += a
			sumWords += nw
			sumDis += d
			done++
		}
	}
	if done > 0 {
		fmt.Printf("TOTAL crops=%d single_column_pages=%d vision_failed=%d mean_agree=%.3f word_disagree=%.3f cost=$%.3f mean_secs=%.1f\n", done, singles, failed, sumAgree/float64(done), float64(sumDis)/float64(sumWords), totCost, totSecs/float64(done))
	}
	return nil
}
