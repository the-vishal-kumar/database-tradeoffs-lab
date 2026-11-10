// Package engines runs the same key-value workload on a B+tree (bbolt, the
// engine under etcd) and an LSM tree (Pebble, the engine under
// CockroachDB), both embedded in this process, and measures what each
// costs: write throughput, bytes really written to disk (from the kernel's
// per-process I/O counters), point reads, range scans and space.
package engines

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	bolt "go.etcd.io/bbolt"
)

// Engine is the small interface both implement here.
type Engine interface {
	Name() string
	// Write puts every key/value in kvs as one durable batch (one fsync).
	Write(kvs [][2][]byte) error
	Get(k []byte) ([]byte, error)
	// Scan reads n keys in order starting at k.
	Scan(k []byte, n int) (int, error)
	Flush() error // make background work (compactions) settle
	Close() error
}

// ---- bbolt (B+tree) ------------------------------------------------------------

type boltEngine struct{ db *bolt.DB }

var bucket = []byte("kv")

func OpenBolt(dir string) (Engine, error) {
	db, err := bolt.Open(filepath.Join(dir, "bolt.db"), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucketIfNotExists(bucket); return err })
	return &boltEngine{db}, err
}

func (b *boltEngine) Name() string { return "B+tree (bbolt)" }
func (b *boltEngine) Write(kvs [][2][]byte) error {
	return b.db.Update(func(tx *bolt.Tx) error { // one transaction, one fsync
		bk := tx.Bucket(bucket)
		for _, kv := range kvs {
			if err := bk.Put(kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
}
func (b *boltEngine) Get(k []byte) (v []byte, err error) {
	err = b.db.View(func(tx *bolt.Tx) error {
		v = bytes.Clone(tx.Bucket(bucket).Get(k))
		return nil
	})
	return v, err
}
func (b *boltEngine) Scan(k []byte, n int) (got int, err error) {
	err = b.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucket).Cursor()
		for key, _ := c.Seek(k); key != nil && got < n; key, _ = c.Next() {
			got++
		}
		return nil
	})
	return got, err
}
func (b *boltEngine) Flush() error { return nil }
func (b *boltEngine) Close() error { return b.db.Close() }

// ---- Pebble (LSM tree) -----------------------------------------------------------

type pebbleEngine struct {
	db   *pebble.DB
	name string
}

func OpenPebble(dir string) (Engine, error) {
	db, err := pebble.Open(filepath.Join(dir, "pebble"), &pebble.Options{Logger: quiet{}})
	return &pebbleEngine{db, "LSM tree (Pebble, defaults)"}, err
}

// OpenPebbleBloom adds a Bloom filter (10 bits per key) to every level, as
// production LSM deployments do, so a read can skip files that can't hold
// the key.
func OpenPebbleBloom(dir string) (Engine, error) {
	o := &pebble.Options{Logger: quiet{}}
	for i := range o.Levels {
		o.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
	}
	db, err := pebble.Open(filepath.Join(dir, "pebble"), o)
	return &pebbleEngine{db, "LSM tree (Pebble, Bloom filters)"}, err
}

// quiet drops Pebble's informational log lines (keeping fatal ones).
type quiet struct{}

func (quiet) Infof(string, ...any)      {}
func (quiet) Errorf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
func (quiet) Fatalf(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

func (p *pebbleEngine) Name() string { return p.name }
func (p *pebbleEngine) Write(kvs [][2][]byte) error {
	b := p.db.NewBatch()
	for _, kv := range kvs {
		if err := b.Set(kv[0], kv[1], nil); err != nil {
			return err
		}
	}
	return b.Commit(pebble.Sync) // the WAL is fsynced; memtables and SSTables come later
}
func (p *pebbleEngine) Get(k []byte) ([]byte, error) {
	v, closer, err := p.db.Get(k)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return bytes.Clone(v), nil
}
func (p *pebbleEngine) Scan(k []byte, n int) (int, error) {
	it, err := p.db.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer it.Close()
	got := 0
	for ok := it.SeekGE(k); ok && got < n; ok = it.Next() {
		got++
	}
	return got, it.Error()
}
func (p *pebbleEngine) Flush() error {
	if err := p.db.Flush(); err != nil {
		return err
	}
	// Wait for running compactions to finish, so their writes are counted.
	// (Estimated debt can stay above zero on a small store, so it's not
	// waited for.)
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 300; i++ {
		if p.db.Metrics().Compact.NumInProgress == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
func (p *pebbleEngine) Close() error { return p.db.Close() }

// ---- measuring ---------------------------------------------------------------------

// ioWritten is the bytes this process has caused to be written to storage,
// from /proc/self/io (Linux), so it includes everything an engine writes:
// pages, logs, compactions.
func ioWritten() (int64, error) {
	f, err := os.Open("/proc/self/io")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "write_bytes: "); ok {
			return strconv.ParseInt(v, 10, 64)
		}
	}
	return 0, fmt.Errorf("no write_bytes in /proc/self/io")
}

func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	return n, err
}

func key(i int) []byte {
	k := make([]byte, 16)
	binary.BigEndian.PutUint64(k, uint64(i)*0x9e3779b97f4a7c15) // spread keys randomly over the key space
	binary.BigEndian.PutUint64(k[8:], uint64(i))
	return k
}

// Phase is one part of the workload on one engine.
type Phase struct {
	Name               string
	Ops                int
	PerSec             float64
	P50, P99           time.Duration // per operation (per batch for writes)
	LogicalBytes       int64
	DiskWrittenBytes   int64 // bytes the kernel wrote to storage for this phase
	WriteAmplification float64
}

type Result struct {
	Engine    string
	Phases    []Phase
	DiskBytes int64 // size on disk at the end
	LiveBytes int64 // keys + values currently stored
}

func pct(d []time.Duration, q float64) time.Duration {
	slices.Sort(d)
	return d[min(len(d)-1, int(float64(len(d))*q))]
}

// Run: insert n keys in random order, overwrite every key once (random
// order), then point-read and range-scan.
func Run(open func(string) (Engine, error), dir string, n, valueSize, batch int) (Result, error) {
	e, err := open(dir)
	if err != nil {
		return Result{}, err
	}
	defer e.Close()
	r := Result{Engine: e.Name()}
	rng := rand.New(rand.NewPCG(1, 1))
	val := make([]byte, valueSize)

	write := func(name string) error {
		order := rng.Perm(n)
		before, err := ioWritten()
		if err != nil {
			return err
		}
		var lat []time.Duration
		t0 := time.Now()
		for i := 0; i < n; i += batch {
			kvs := make([][2][]byte, 0, batch)
			for _, j := range order[i:min(i+batch, n)] {
				for b := range val {
					val[b] = byte(rng.IntN(256)) // incompressible values
				}
				kvs = append(kvs, [2][]byte{key(j), bytes.Clone(val)})
			}
			t := time.Now()
			if err := e.Write(kvs); err != nil {
				return err
			}
			lat = append(lat, time.Since(t))
		}
		took := time.Since(t0)
		if err := e.Flush(); err != nil {
			return err
		}
		after, err := ioWritten()
		if err != nil {
			return err
		}
		logical := int64(n * (16 + valueSize))
		r.Phases = append(r.Phases, Phase{name, n, float64(n) / took.Seconds(), pct(lat, 0.5), pct(lat, 0.99),
			logical, after - before, float64(after-before) / float64(logical)})
		return nil
	}
	if err := write(fmt.Sprintf("insert %d keys (random order, batches of %d, fsync per batch)", n, batch)); err != nil {
		return r, err
	}
	if err := write("overwrite every key once"); err != nil {
		return r, err
	}

	var lat []time.Duration
	t0 := time.Now()
	const reads = 100_000
	for range reads {
		t := time.Now()
		if _, err := e.Get(key(rng.IntN(n))); err != nil {
			return r, err
		}
		lat = append(lat, time.Since(t))
	}
	r.Phases = append(r.Phases, Phase{Name: "point reads (keys that exist)", Ops: reads, PerSec: reads / time.Since(t0).Seconds(), P50: pct(lat, 0.5), P99: pct(lat, 0.99)})

	// Keys that were never written: "do we have this?" checks, where a
	// filter can rule out most files without reading them.
	lat = lat[:0]
	t0 = time.Now()
	for range reads {
		t := time.Now()
		if _, err := e.Get(key(n + rng.IntN(n))); err != nil && !isNotFound(err) {
			return r, err
		}
		lat = append(lat, time.Since(t))
	}
	r.Phases = append(r.Phases, Phase{Name: "point reads (keys that don't exist)", Ops: reads, PerSec: reads / time.Since(t0).Seconds(), P50: pct(lat, 0.5), P99: pct(lat, 0.99)})

	lat = lat[:0]
	t0 = time.Now()
	const scans = 2_000
	for range scans {
		t := time.Now()
		if _, err := e.Scan(key(rng.IntN(n)), 100); err != nil {
			return r, err
		}
		lat = append(lat, time.Since(t))
	}
	r.Phases = append(r.Phases, Phase{Name: "range scans of 100 keys", Ops: scans, PerSec: scans / time.Since(t0).Seconds(), P50: pct(lat, 0.5), P99: pct(lat, 0.99)})

	if err := e.Flush(); err != nil {
		return r, err
	}
	r.DiskBytes, err = dirSize(dir)
	r.LiveBytes = int64(n * (16 + valueSize))
	return r, err
}

func isNotFound(err error) bool { return errors.Is(err, pebble.ErrNotFound) }
