package main

import (
	"strings"
	"testing"
)

func TestMergeMajority(t *testing.T) {
	g1 := "بعبدا في ٥ آب ٢٠٠٦\nالإمضاء: إميل لحود"
	g2 := "بعبدا في ٥ آب ٢٠٢٦\nالإمضاء: جوزاف عون"
	tess := "بعبدا في 5 آب 2026\nالامضاء: جوزاف عون"
	m, flagged, _, _, settled := mergeReaders(g1, g2, tess)
	if flagged != 0 || settled == 0 {
		t.Fatalf("flagged=%d settled=%d: %q", flagged, settled, m)
	}
	if strings.Contains(m, "لحود") || !strings.Contains(m, "جوزاف") {
		t.Fatalf("the invented signature won: %q", m)
	}
}

func TestMergeAllDiffer(t *testing.T) {
	m, flagged, _, _, _ := mergeReaders("رقم ١٢٣ تاريخ", "رقم ٤٥٦ تاريخ", "رقم 789 تاريخ")
	if flagged != 1 || !strings.Contains(m, mkStart) {
		t.Fatalf("expected one flagged span, got %d: %q", flagged, m)
	}
}

func TestStripHeaderLines(t *testing.T) {
	in := "الجريدة الرسمية ــ العدد ٣٢ ــ ٢٠٢٦/٧/٣٠\n٢٧٦٩\nالمادة الأولى: نص"
	if got := stripHeaderLines(in); got != "المادة الأولى: نص" {
		t.Fatalf("got %q", got)
	}
	if got := stripHeaderLines("٢ ــ توفير وسائل"); got != "٢ ــ توفير وسائل" {
		t.Fatalf("a numbered line was dropped: %q", got)
	}
}
