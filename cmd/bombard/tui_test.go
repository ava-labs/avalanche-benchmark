package main

import (
	"reflect"
	"testing"
)

func TestZeroPaddedLatestSeriesAnchorsHistoryRight(t *testing.T) {
	got := zeroPaddedLatestSeries([]float64{10, 20, 30}, 6)
	want := []float64{0, 0, 0, 10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zeroPaddedLatestSeries() = %v, want %v", got, want)
	}
}

func TestZeroPaddedLatestSeriesKeepsLatestSamples(t *testing.T) {
	got := zeroPaddedLatestSeries([]float64{10, 20, 30, 40}, 2)
	want := []float64{30, 40}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zeroPaddedLatestSeries() = %v, want %v", got, want)
	}
}
