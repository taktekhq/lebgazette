package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Anything legal is read and judged by Gemini through agy (Antigravity CLI):
// Nizar's rule of 16 Oct 2026. Models transcribe and judge; they never rewrite.

const defaultGemini = "gemini-3.8-flash-high"

// agyRun runs `agy -p` in dir and returns its output. One retry on an empty answer.
func agyRun(dir, prompt, model string, timeout time.Duration) (string, error) {
	var last error
	for try := 0; try < 2; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, "agy", "-p", prompt, "--model", model, "--print-timeout", "0", "--dangerously-skip-permissions")
		cmd.Dir = dir
		out, err := cmd.Output()
		cancel()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return string(out), nil
		}
		last = fmt.Errorf("agy: %v (%d bytes)", err, len(out))
	}
	return "", last
}

const columnPrompt = `View the image files %s once each with your file viewer. Each is one column of a page of the Lebanese Official Gazette, in Arabic. For each file, in the order given, output a line "=== <file name> ===" and then the transcription of its text, exactly as printed, top to bottom, one printed paragraph (or table row, cells separated by " | ") per line. Do not crop, enlarge, re-inspect, run code, or create files. Do not correct, normalize, modernize or reword anything. Keep digits exactly as printed. Give your best reading of every word. Output nothing else.`

var reMark = regexp.MustCompile(`(?m)^=== (.+?) ===\s*$`)

// geminiRead transcribes the given image files (same directory) in one call.
func geminiRead(files []string, model string) (map[string]string, float64, error) {
	t0 := time.Now()
	dir := filepath.Dir(files[0])
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = filepath.Base(f)
	}
	out, err := agyRun(dir, fmt.Sprintf(columnPrompt, strings.Join(names, ", ")), model, 15*time.Minute)
	if err != nil {
		return nil, 0, err
	}
	res := map[string]string{}
	idx := reMark.FindAllStringSubmatchIndex(out, -1)
	for i, m := range idx {
		end := len(out)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		res[strings.TrimSpace(out[m[2]:m[3]])] = strings.TrimSpace(out[m[1]:end])
	}
	if len(files) == 1 && len(idx) == 0 {
		res[names[0]] = strings.TrimSpace(out)
	}
	for _, n := range names {
		if refusal(res[n]) {
			return nil, 0, fmt.Errorf("no usable reading for %s", n)
		}
	}
	return res, time.Since(t0).Seconds(), nil
}

// visionRead keeps the old single-file signature for the pilot code paths.
func visionRead(path, model string) (visionResult, error) {
	r, secs, err := geminiRead([]string{path}, model)
	if err != nil {
		return visionResult{}, err
	}
	return visionResult{Text: r[filepath.Base(path)], Secs: secs}, nil
}

// judgeGemini asks Gemini about page 1 of a PDF (a rendered PNG named page1.png in dir).
func judgeGemini(dir, source, title, model string) (bool, string, error) {
	out, err := agyRun(dir, fmt.Sprintf(`View the image file page1.png once with your file viewer. It is page 1 of a PDF collected from "%s" (title: %q). Decide whether it is the OFFICIAL text of a Lebanese law, decree, ministerial decision, circular or court or Constitutional Council ruling: a scan or export of the signed document (it shows its number and date, and a signature, stamp, official letterhead or the issuing body's own final file). It is NOT official if it is a draft, committee version, proposal, report, study, memo, or a news or NGO write-up. Do not transcribe or rewrite anything, do not crop or run code. Answer with one line of JSON only: {"official":true or false,"reasons":"at most 30 words"}`, source, title), model, 10*time.Minute)
	if err != nil {
		return false, "", err
	}
	m := reJSON.FindString(out)
	var v struct {
		Official bool   `json:"official"`
		Reasons  string `json:"reasons"`
	}
	if m == "" || jsonUnmarshal(m, &v) != nil {
		return false, "", fmt.Errorf("unparseable answer: %.120s", out)
	}
	return v.Official, v.Reasons, nil
}

var _ = os.Stderr

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
