//go:build !race

// Skipped under -race: Pebble is many times slower there, and this test is
// single-threaded. CI runs it in a separate step without -race.

package engines

import (
	"os"
	"testing"
)

// The trade-off the post relies on, at a small size (20,000 keys, so it stays quick under -race): random writes cost a
// B+tree far more disk writes than an LSM tree; reads go the other way.
func TestTradeOff(t *testing.T) {
	if _, err := os.Stat("/proc/self/io"); err != nil {
		t.Skip("needs Linux /proc/self/io")
	}
	run := func(open func(string) (Engine, error)) Result {
		r, err := Run(open, t.TempDir(), 20_000, 100, 200)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	b, p := run(OpenBolt), run(OpenPebbleBloom)
	if b.Phases[1].WriteAmplification < 2*p.Phases[1].WriteAmplification {
		t.Errorf("B+tree overwrite amplification %.1f should be well above the LSM's %.1f",
			b.Phases[1].WriteAmplification, p.Phases[1].WriteAmplification)
	}
	if b.Phases[2].P50 > p.Phases[2].P50 {
		t.Errorf("B+tree point reads (%s) should be faster than the LSM's (%s)", b.Phases[2].P50, p.Phases[2].P50)
	}
}
