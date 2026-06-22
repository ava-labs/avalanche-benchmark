package main

import (
	"os"
	"strconv"
)

// Load-generator throttle control for restore catch-up. When a rejoining site is
// losing ground to the live tip, the restore sync gate writes a reduced rps to a
// throttle file; bombard (running separately on the same control host) watches the
// file and caps its issue rate there, then resumes full rate when the file is
// removed. This automates the "fail back during a lull" guidance with NO permanent
// change to the benchmark target — full throughput resumes the instant the site is
// caught up. The path must match bombard's defaultThrottleFile / the
// BOMBARD_THROTTLE_FILE override. A no-op if bombard isn't running (the file is
// simply ignored), so restore behaves identically with or without load.

const (
	restoreThrottleRPS  = 2000 // initial ingress cap applied when a target first loses ground
	restoreThrottleStep = 500  // step the cap down by this each poll a target isn't converging
	// defaultThrottleFloor near-pauses block production so even a delivery-straggler
	// (a node that fell behind and can't catch a high block-rate chain at full load)
	// can close the gap. Because this chain's min-block-delay is ~0, the block RATE
	// barely drops until rps is very low — so the floor must be aggressive. Override
	// with RESTORE_THROTTLE_FLOOR (e.g. =10 to nearly stop the chain during catch-up).
	defaultThrottleFloor = 25
)

func throttleFilePath() string {
	if p := os.Getenv("BOMBARD_THROTTLE_FILE"); p != "" {
		return p
	}
	return "/tmp/bombard.throttle"
}

// initialThrottleRPS is the first cap applied when a target loses ground; override
// with RESTORE_THROTTLE_RPS.
func initialThrottleRPS() int {
	if v := os.Getenv("RESTORE_THROTTLE_RPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return restoreThrottleRPS
}

// throttleFloor is the lowest ingress cap the gate will step down to; override with
// RESTORE_THROTTLE_FLOOR.
func throttleFloor() int {
	if v := os.Getenv("RESTORE_THROTTLE_FLOOR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultThrottleFloor
}

// setThrottle caps the load generator at rps (a no-op if bombard isn't running).
func setThrottle(rps int) {
	_ = os.WriteFile(throttleFilePath(), []byte(strconv.Itoa(rps)+"\n"), 0o644)
}

// clearThrottle removes any active throttle, restoring full ingress.
func clearThrottle() {
	_ = os.Remove(throttleFilePath())
}
