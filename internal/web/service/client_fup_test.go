package service

import (
	"testing"
	"time"
)

func TestFupPeriodStartDaily(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 6, 18, 15, 30, 0, 0, loc)
	start := fupPeriodStart(now, "00:00", "daily", 0, 0)
	want := time.Date(2026, 6, 18, 0, 0, 0, 0, loc)
	if !start.Equal(want) {
		t.Fatalf("daily start = %v, want %v", start, want)
	}

	beforeReset := time.Date(2026, 6, 17, 23, 30, 0, 0, loc)
	start2 := fupPeriodStart(beforeReset, "00:00", "daily", 0, 0)
	want2 := time.Date(2026, 6, 17, 0, 0, 0, 0, loc)
	if !start2.Equal(want2) {
		t.Fatalf("daily before reset = %v, want %v", start2, want2)
	}
}

func TestFupPeriodStartWeeklyMonday(t *testing.T) {
	loc := time.Local
	// Wednesday 2026-06-18, weekly reset Monday 00:00
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, loc)
	start := fupPeriodStart(now, "00:00", "weekly", 1, 0)
	want := time.Date(2026, 6, 15, 0, 0, 0, 0, loc) // Monday
	if !start.Equal(want) {
		t.Fatalf("weekly start = %v, want %v", start, want)
	}
}
