package main

import "testing"

func TestTopupDeficitDays(t *testing.T) {
	const rate = uint64(512) // nAVAX/s
	day := rate * 24 * 3600
	tests := []struct {
		name    string
		balance uint64
		target  int
		want    int
	}{
		{"below target", 2 * day, 15, 13},
		{"above target", 30 * day, 15, 0},
		{"exactly at target", 15 * day, 15, 0},
		{"sub-day deficit skipped", 15*day - 100*rate, 15, 0}, // 100s burned since last top-up
		{"zero balance", 0, 15, 15},
	}
	for _, tt := range tests {
		if got := topupDeficitDays(tt.balance, rate, tt.target); got != tt.want {
			t.Errorf("%s: topupDeficitDays(%d, %d, %d) = %d, want %d",
				tt.name, tt.balance, rate, tt.target, got, tt.want)
		}
	}
}
