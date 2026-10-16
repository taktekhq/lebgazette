package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const judgePrompt = `Read the image file page1.png with the Read tool. It is page 1 of a PDF collected from "%s" (title: %q). Decide whether it is the OFFICIAL text of a Lebanese law, decree, ministerial decision, circular or court or Constitutional Council ruling: a scan or export of the signed document (it shows its number and date, and a signature, stamp, official letterhead or the issuing body's own final file). It is NOT official if it is a draft, committee version, proposal, report, study, memo, or a news or NGO write-up. Do not transcribe or rewrite anything. Answer with one line of JSON only: {"official":true or false,"reasons":"at most 30 words"}`

var reJSON = regexp.MustCompile(`\{[^{}]*"official"[^{}]*\}`)

// judgeOne asks a model about page 1 only. It judges, it never rewrites text.
func (w *watcher) judgeOne(it Item) (official bool, reason string, err error) {
	dir, err := os.MkdirTemp("", "gzjudge")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(dir)
	if out, err := exec.Command("pdftoppm", "-r", "110", "-f", "1", "-l", "1", "-png", "-singlefile",
		w.p("pdf", it.SHA+".pdf"), filepath.Join(dir, "page1")).CombinedOutput(); err != nil {
		return false, "", fmt.Errorf("pdftoppm: %v %s", err, out)
	}
	cmd := exec.Command("claude", "-p", fmt.Sprintf(judgePrompt, it.Source, it.Title),
		"--model", "sonnet", "--no-session-persistence", "--tools", "Read",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--disable-slash-commands", "--setting-sources", "",
		"--system-prompt", "You classify documents. You never rewrite them.", "--output-format", "json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, "", fmt.Errorf("claude: %v", err)
	}
	var j struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(out, &j); err != nil || j.IsError {
		return false, "", fmt.Errorf("claude: %.120s", j.Result)
	}
	m := reJSON.FindString(j.Result)
	var v struct {
		Official bool   `json:"official"`
		Reasons  string `json:"reasons"`
	}
	if m == "" || json.Unmarshal([]byte(m), &v) != nil {
		return false, "", fmt.Errorf("unparseable answer: %.120s", j.Result)
	}
	return v.Official, v.Reasons, nil
}

// judgeAll settles every unjudged PDF. With no model available, a PDF served by
// the issuing body itself (CC, ministries) is official by origin; everything else
// stays "unjudged" and is never published.
func (w *watcher) judgeAll() {
	modelOK := w.judge == "claude"
	for i := range w.items {
		it := &w.items[i]
		if it.Kind != "pdf" || it.Status != "unjudged" {
			continue
		}
		if modelOK {
			ok, why, err := w.judgeOne(*it)
			if err != nil {
				w.err("judge", err)
				modelOK = false // likely not logged in: stop calling, keep the rest unjudged
			} else {
				it.Reason = strings.TrimSpace(why)
				if ok {
					it.Status = "official"
				} else {
					it.Status = "draft"
				}
				w.syncFresh(*it)
				continue
			}
		}
		if it.ByOrigin {
			it.Status, it.Reason = "official", "issued by the source body (not model-judged)"
			w.syncFresh(*it)
		}
	}
}

func (w *watcher) syncFresh(it Item) {
	for i := range w.fresh {
		if w.fresh[i].PDFURL == it.PDFURL && w.fresh[i].Kind == "pdf" {
			w.fresh[i] = it
		}
	}
}
