// Command engines runs the same key-value workload on a B+tree (bbolt) and
// an LSM tree (Pebble).
//
//	go run ./cmd/engines
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/the-vishal-kumar/database-tradeoffs-lab/internal/engines"
)

func dur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1f µs", float64(d.Nanoseconds())/1000)
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000)
	default:
		return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
	}
}

func mb(b int64) string { return fmt.Sprintf("%.0f MB", float64(b)/1e6) }

func main() {
	n := flag.Int("keys", 1_000_000, "keys")
	vs := flag.Int("value", 100, "value size in bytes")
	batch := flag.Int("batch", 1000, "writes per durable batch")
	flag.Parse()
	fmt.Printf("%d keys of 16 bytes, values of %d random bytes, batches of %d writes (one fsync each). Go %s, both engines embedded in this process; \"written to disk\" is the kernel's write_bytes for the process.\n\n",
		*n, *vs, *batch, runtime.Version())
	fmt.Println("| Engine | Phase | Ops/s | p50 | p99 | Written to disk | Write amplification |")
	fmt.Println("|---|---|---:|---:|---:|---:|---:|")
	var res []engines.Result
	for _, open := range []func(string) (engines.Engine, error){engines.OpenBolt, engines.OpenPebble, engines.OpenPebbleBloom} {
		dir, err := os.MkdirTemp("", "engines")
		if err != nil {
			panic(err)
		}
		r, err := engines.Run(open, dir, *n, *vs, *batch)
		os.RemoveAll(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		res = append(res, r)
		for _, p := range r.Phases {
			w, amp := "-", "-"
			if p.LogicalBytes > 0 {
				w, amp = mb(p.DiskWrittenBytes), fmt.Sprintf("%.1f×", p.WriteAmplification)
			}
			fmt.Printf("| %s | %s | %.0f | %s | %s | %s | %s |\n", r.Engine, p.Name, p.PerSec, dur(p.P50), dur(p.P99), w, amp)
		}
	}
	fmt.Println("\n| Engine | Live data (keys + values) | Size on disk at the end | Space amplification |\n|---|---:|---:|---:|")
	for _, r := range res {
		fmt.Printf("| %s | %s | %s | %.2f× |\n", r.Engine, mb(r.LiveBytes), mb(r.DiskBytes), float64(r.DiskBytes)/float64(r.LiveBytes))
	}
}
