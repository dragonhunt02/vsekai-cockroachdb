// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package rangefeed_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/rangefeed"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/errors/oserror"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func runCatchUpBenchmark(b *testing.B, emk engineMaker, opts benchOptions) {
	eng, _ := setupData(context.Background(), b, emk, opts.dataOpts)
	defer eng.Close()
	startKey := roachpb.Key(encoding.EncodeUvarintAscending([]byte("key-"), uint64(0)))
	endKey := roachpb.Key(encoding.EncodeUvarintAscending([]byte("key-"), uint64(opts.dataOpts.numKeys)))
	span := roachpb.Span{
		Key:    startKey,
		EndKey: endKey,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		func() {
			iter := rangefeed.NewCatchUpIterator(eng, &roachpb.RangeFeedRequest{
				Header: roachpb.Header{
					Timestamp: opts.ts,
				},
				WithDiff: opts.withDiff,
				Span:     span,
			}, opts.useTBI, func() {})
			defer iter.Close()
			counter := 0
			err := iter.CatchUpScan(storage.MakeMVCCMetadataKey(startKey), storage.MakeMVCCMetadataKey(endKey), opts.ts, opts.withDiff, func(*roachpb.RangeFeedEvent) error {
				counter++
				return nil
			})
			if err != nil {
				b.Fatalf("failed catchUp scan: %+v", err)
			}
			if counter < 1 {
				b.Fatalf("didn't emit any events!")
			}
		}()
	}
}

func BenchmarkCatchUpScan(b *testing.B) {
	defer log.Scope(b).Close(b)

	numKeys := 1_000_000
	valueBytes := 64

	dataOpts := map[string]benchDataOptions{
		// linear-keys is one of our best-case scenarios. In
		// this case, each newly written row is at a key
		// following the previously written row and at a later
		// timestamp. Further, once compacted, all of the SSTs
		// should be in L5 and L6. As a result, the time-based
		// optimization can exclude SSTs fairly easily.
		"linear-keys": {
			numKeys:    numKeys,
			valueBytes: valueBytes,
		},
		// random-keys is our worst case. We write keys in
		// random order but with timestamps that keep marching
		// forward. Once compacted, most of the data is in L5
		// and L6. So, we have very few overlapping SSTs and
		// most SSTs in our lower level will have at least 1
		// key that needs to be included in our scan, despite
		// the time based optimization.
		"random-keys": {
			randomKeyOrder: true,
			numKeys:        numKeys,
			valueBytes:     valueBytes,
		},
		// mixed-case is a middling case.
		//
		// This case is trying to simulate a larger store, but
		// with fewer bytes. If we did not reduce
		// LBaseMaxBytes, almost all data would be in Lbase or
		// L6, and TBI would be ineffective. By reducing
		// LBaseMaxBytes, the data should spread out over more
		// levels, like in a real store. The LSM state
		// depicted below shows that this was only partially
		// successful.
		//
		// We return a read only engine to prevent read-based
		// compactions after the initial data generation.
		//
		// As of 2021-08-18 data generated using these
		// settings looked like:
		//
		//__level_____count____size___score______in__ingest(sz_cnt)____move(sz_cnt)___write(sz_cnt)____read___r-amp___w-amp
		//   WAL         1     0 B       -     0 B       -       -       -       -     0 B       -       -       -     0.0
		//     0         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		//     1         2   4.4 M 1819285.94     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		//     2         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		//     3         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		//     4         1   397 K    0.79     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		//     5         0     0 B    0.00     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		//     6         1    83 M       -     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		// total         4    88 M       -     0 B     0 B       0     0 B       0     0 B       0     0 B       0     0.0
		"mixed-case": {
			randomKeyOrder: true,
			numKeys:        numKeys,
			valueBytes:     valueBytes,
			readOnlyEngine: true,
			lBaseMaxBytes:  256,
		},
	}

	for name, do := range dataOpts {
		b.Run(name, func(b *testing.B) {
			for _, useTBI := range []bool{true, false} {
				b.Run(fmt.Sprintf("useTBI=%v", useTBI), func(b *testing.B) {
					for _, withDiff := range []bool{true, false} {
						b.Run(fmt.Sprintf("withDiff=%v", withDiff), func(b *testing.B) {
							for _, tsExcludePercent := range []float64{0.0, 0.50, 0.75, 0.95, 0.99} {
								wallTime := int64((5 * (float64(numKeys)*tsExcludePercent + 1)))
								ts := hlc.Timestamp{WallTime: wallTime}
								b.Run(fmt.Sprintf("perc=%2.2f", tsExcludePercent*100), func(b *testing.B) {
									runCatchUpBenchmark(b, setupMVCCPebble, benchOptions{
										dataOpts: do,
										ts:       ts,
										useTBI:   useTBI,
										withDiff: withDiff,
									})
								})
							}
						})
					}
				})
			}
		})
	}
}

type benchDataOptions struct {
	numKeys        int
	valueBytes     int
	randomKeyOrder bool
	readOnlyEngine bool
	lBaseMaxBytes  int64
}

type benchOptions struct {
	ts       hlc.Timestamp
	useTBI   bool
	withDiff bool
	dataOpts benchDataOptions
}

//
// The following code was copied and then modified from the testing
// code in pkg/storage.
//

type engineMaker func(testing.TB, string, int64, bool) storage.Engine

func setupMVCCPebble(b testing.TB, dir string, lBaseMaxBytes int64, readOnly bool) storage.Engine {
	opts := storage.DefaultPebbleOptions()
	opts.FS = vfs.Default
	opts.LBaseMaxBytes = lBaseMaxBytes
	opts.ReadOnly = readOnly
	opts.FormatMajorVersion = pebble.FormatBlockPropertyCollector
	peb, err := storage.NewPebble(
		context.Background(),
		storage.PebbleConfig{
			StorageConfig: base.StorageConfig{Dir: dir, Settings: cluster.MakeTestingClusterSettings()},
			Opts:          opts,
		})
	if err != nil {
		b.Fatalf("could not create new pebble instance at %s: %+v", dir, err)
	}
	return peb
}

// setupData data writes numKeys keys. One version of each key
// is written. The write timestamp starts at 5ns and then in 5ns
// increments. This allows scans at various times, starting at t=5ns,
// and continuing to t=5ns*(numKeys+1). The goal of this is to
// approximate an append-only type workload.
//
// A read-only engin can be returned if opts.readOnlyEngine is
// set. The goal of this is to prevent read-triggered compactions that
// might change the distribution of data across levels.
//
// The creation of the database is time consuming, especially for
// larger numbers of versions. The database is persisted between runs
// and stored in the current directory.
func setupData(
	ctx context.Context, b *testing.B, emk engineMaker, opts benchDataOptions,
) (storage.Engine, string) {
	orderStr := "linear"
	if opts.randomKeyOrder {
		orderStr = "random"
	}
	readOnlyStr := ""
	if opts.readOnlyEngine {
		readOnlyStr = "_readonly"
	}
	loc := fmt.Sprintf("rangefeed_bench_data_%s%s_%d_%d_%d",
		orderStr, readOnlyStr, opts.numKeys, opts.valueBytes, opts.lBaseMaxBytes)
	exists := true
	if _, err := os.Stat(loc); oserror.IsNotExist(err) {
		exists = false
	} else if err != nil {
		b.Fatal(err)
	}

	if exists {
		testutils.ReadAllFiles(filepath.Join(loc, "*"))
		return emk(b, loc, opts.lBaseMaxBytes, opts.readOnlyEngine), loc
	}

	eng := emk(b, loc, opts.lBaseMaxBytes, false)
	log.Infof(ctx, "creating rangefeed benchmark data: %s", loc)

	// Generate the same data every time.
	rng := rand.New(rand.NewSource(1449168817))

	keys := make([]roachpb.Key, opts.numKeys)
	order := make([]int, 0, opts.numKeys)
	for i := 0; i < opts.numKeys; i++ {
		keys[i] = roachpb.Key(encoding.EncodeUvarintAscending([]byte("key-"), uint64(i)))
		order = append(order, i)
	}

	if opts.randomKeyOrder {
		rng.Shuffle(len(order), func(i, j int) {
			order[i], order[j] = order[j], order[i]
		})
	}

	writeKey := func(batch storage.Batch, idx int, pos int) {
		key := keys[idx]
		value := roachpb.MakeValueFromBytes(randutil.RandBytes(rng, opts.valueBytes))
		value.InitChecksum(key)
		ts := hlc.Timestamp{WallTime: int64((pos + 1) * 5)}
		if err := storage.MVCCPut(ctx, batch, nil /* ms */, key, ts, value, nil); err != nil {
			b.Fatal(err)
		}
	}

	batch := eng.NewBatch()
	for i, idx := range order {
		// Output the keys in ~20 batches. If we used a single batch to output all
		// of the keys rocksdb would create a single sstable. We want multiple
		// sstables in order to exercise filtering of which sstables are examined
		// during iterator seeking. We fix the number of batches we output so that
		// optimizations which change the data size result in the same number of
		// sstables.
		if scaled := len(order) / 20; i > 0 && (i%scaled) == 0 {
			log.Infof(ctx, "committing (%d/~%d) (%d/%d)", i/scaled, 20, i, len(order))
			if err := batch.Commit(false /* sync */); err != nil {
				b.Fatal(err)
			}
			batch.Close()
			batch = eng.NewBatch()
			if err := eng.Flush(); err != nil {
				b.Fatal(err)
			}
		}
		writeKey(batch, idx, i)
	}
	if err := batch.Commit(false /* sync */); err != nil {
		b.Fatal(err)
	}
	batch.Close()
	if err := eng.Flush(); err != nil {
		b.Fatal(err)
	}

	if opts.readOnlyEngine {
		eng.Close()
		eng = emk(b, loc, opts.lBaseMaxBytes, opts.readOnlyEngine)
	}
	return eng, loc
}
