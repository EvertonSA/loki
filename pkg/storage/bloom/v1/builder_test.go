package v1

import (
	"bytes"
	"errors"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/pkg/chunkenc"
)

func EqualIterators[T any](t *testing.T, test func(a, b T), expected, actual Iterator[T]) {
	for expected.Next() {
		require.True(t, actual.Next())
		a, b := expected.At(), actual.At()
		test(a, b)
	}
	require.False(t, actual.Next())
	require.Nil(t, expected.Err())
	require.Nil(t, actual.Err())
}

func TestBlockBuilderRoundTrip(t *testing.T) {
	numSeries := 100
	numKeysPerSeries := 10000
	data, keys := mkBasicSeriesWithBlooms(numSeries, numKeysPerSeries, 0, 0xffff, 0, 10000)

	// references for linking in memory reader+writer
	indexBuf := bytes.NewBuffer(nil)
	bloomsBuf := bytes.NewBuffer(nil)
	// directory for directory reader+writer
	tmpDir := t.TempDir()

	for _, tc := range []struct {
		desc   string
		writer BlockWriter
		reader BlockReader
	}{
		{
			desc:   "in-memory",
			writer: NewMemoryBlockWriter(indexBuf, bloomsBuf),
			reader: NewByteReader(indexBuf, bloomsBuf),
		},
		{
			desc:   "directory",
			writer: NewDirectoryBlockWriter(tmpDir),
			reader: NewDirectoryBlockReader(tmpDir),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			schema := Schema{
				version:     DefaultSchemaVersion,
				encoding:    chunkenc.EncSnappy,
				nGramLength: 10,
				nGramSkip:   2,
			}

			builder, err := NewBlockBuilder(
				BlockOptions{
					schema:         schema,
					SeriesPageSize: 100,
					BloomPageSize:  10 << 10,
				},
				tc.writer,
			)

			require.Nil(t, err)
			itr := NewSliceIter[SeriesWithBloom](data)
			_, err = builder.BuildFrom(itr)
			require.Nil(t, err)
			block := NewBlock(tc.reader)
			querier := NewBlockQuerier(block)

			err = block.LoadHeaders()
			require.Nil(t, err)
			require.Equal(t, block.blooms.schema, schema)

			for i := 0; i < len(data); i++ {
				require.Equal(t, true, querier.Next(), "on iteration %d with error %v", i, querier.Err())
				got := querier.At()
				require.Equal(t, data[i].Series, got.Series)
				for _, key := range keys[i] {
					require.True(t, got.Bloom.Test(key))
				}
				require.NoError(t, querier.Err())
			}
			// ensure it's exhausted
			require.False(t, querier.Next())

			// test seek
			i := numSeries / 2
			halfData := data[i:]
			halfKeys := keys[i:]
			require.Nil(t, querier.Seek(halfData[0].Series.Fingerprint))
			for j := 0; j < len(halfData); j++ {
				require.Equal(t, true, querier.Next(), "on iteration %d", j)
				got := querier.At()
				require.Equal(t, halfData[j].Series, got.Series)
				for _, key := range halfKeys[j] {
					require.True(t, got.Bloom.Test(key))
				}
				require.NoError(t, querier.Err())
			}
			require.False(t, querier.Next())

		})
	}
}

func TestMergeBuilder(t *testing.T) {

	nBlocks := 10
	numSeries := 100
	numKeysPerSeries := 100
	blocks := make([]PeekingIterator[*SeriesWithBloom], 0, nBlocks)
	data, _ := mkBasicSeriesWithBlooms(numSeries, numKeysPerSeries, 0, 0xffff, 0, 10000)
	blockOpts := BlockOptions{
		schema: Schema{
			version:  DefaultSchemaVersion,
			encoding: chunkenc.EncSnappy,
		},
		SeriesPageSize: 100,
		BloomPageSize:  10 << 10,
	}

	// Build a list of blocks containing overlapping & duplicated parts of the dataset
	for i := 0; i < nBlocks; i++ {
		// references for linking in memory reader+writer
		indexBuf := bytes.NewBuffer(nil)
		bloomsBuf := bytes.NewBuffer(nil)

		min := i * numSeries / nBlocks
		max := (i + 2) * numSeries / nBlocks // allow some overlap
		if max > len(data) {
			max = len(data)
		}

		writer := NewMemoryBlockWriter(indexBuf, bloomsBuf)
		reader := NewByteReader(indexBuf, bloomsBuf)

		builder, err := NewBlockBuilder(
			blockOpts,
			writer,
		)

		require.Nil(t, err)
		itr := NewSliceIter[SeriesWithBloom](data[min:max])
		_, err = builder.BuildFrom(itr)
		require.Nil(t, err)
		blocks = append(blocks, NewPeekingIter[*SeriesWithBloom](NewBlockQuerier(NewBlock(reader))))
	}

	// We're not testing the ability to extend a bloom in this test
	pop := func(_ *Series, _ *Bloom) error {
		return errors.New("not implemented")
	}

	// storage should contain references to all the series we ingested,
	// regardless of block allocation/overlap.
	storeItr := NewMapIter[SeriesWithBloom, *Series](
		NewSliceIter[SeriesWithBloom](data),
		func(swb SeriesWithBloom) *Series {
			return swb.Series
		},
	)

	// Ensure that the merge builder combines all the blocks correctly
	mergeBuilder := NewMergeBuilder(blocks, storeItr, pop)
	indexBuf := bytes.NewBuffer(nil)
	bloomsBuf := bytes.NewBuffer(nil)
	writer := NewMemoryBlockWriter(indexBuf, bloomsBuf)
	reader := NewByteReader(indexBuf, bloomsBuf)

	builder, err := NewBlockBuilder(
		blockOpts,
		writer,
	)
	require.Nil(t, err)

	_, err = mergeBuilder.Build(builder)
	require.Nil(t, err)

	block := NewBlock(reader)
	querier := NewBlockQuerier(block)

	EqualIterators[*SeriesWithBloom](
		t,
		func(a, b *SeriesWithBloom) {
			require.Equal(t, a.Series, b.Series, "expected %+v, got %+v", a, b)
		},
		NewSliceIter[*SeriesWithBloom](PointerSlice(data)),
		querier,
	)
}

func TestBlockReset(t *testing.T) {
	numSeries := 100
	numKeysPerSeries := 10000
	data, _ := mkBasicSeriesWithBlooms(numSeries, numKeysPerSeries, 1, 0xffff, 0, 10000)

	indexBuf := bytes.NewBuffer(nil)
	bloomsBuf := bytes.NewBuffer(nil)
	writer := NewMemoryBlockWriter(indexBuf, bloomsBuf)
	reader := NewByteReader(indexBuf, bloomsBuf)

	schema := Schema{
		version:     DefaultSchemaVersion,
		encoding:    chunkenc.EncSnappy,
		nGramLength: 10,
		nGramSkip:   2,
	}

	builder, err := NewBlockBuilder(
		BlockOptions{
			schema:         schema,
			SeriesPageSize: 100,
			BloomPageSize:  10 << 10,
		},
		writer,
	)

	require.Nil(t, err)
	itr := NewSliceIter[SeriesWithBloom](data)
	_, err = builder.BuildFrom(itr)
	require.Nil(t, err)
	block := NewBlock(reader)
	querier := NewBlockQuerier(block)

	rounds := make([][]model.Fingerprint, 2)

	for i := 0; i < len(rounds); i++ {
		for querier.Next() {
			rounds[i] = append(rounds[i], querier.At().Series.Fingerprint)
		}

		err = querier.Seek(0) // reset at end
		require.Nil(t, err)
	}

	require.Equal(t, rounds[0], rounds[1])
}

func TestBlockChecksums(t *testing.T) {
	testCases := []struct {
		name         string
		fingerprint1 model.Fingerprint
		fingerprint2 model.Fingerprint
		timestamp1   model.Time
		timestamp2   model.Time
		expectEqual  bool
	}{
		{"DifferentFPsSameTs", 0x0000, 0x1111, 0, 0, false},
		{"SameFPsDifferentTs", 0xffff, 0xffff, 0, 123400, false},
		{"DifferentFPsDifferentTs", 0x0000, 0x11aa, 0, 10000, false},
		{"SameFPsSameTs", 0xffff, 0xffff, 0, 0, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			tmpDir2 := t.TempDir()
			writer := NewDirectoryBlockWriter(tmpDir)
			writer2 := NewDirectoryBlockWriter(tmpDir2)

			schema := Schema{
				version:     DefaultSchemaVersion,
				encoding:    chunkenc.EncSnappy,
				nGramLength: 10,
				nGramSkip:   2,
			}

			builder, err := NewBlockBuilder(
				BlockOptions{
					schema:         schema,
					SeriesPageSize: 100,
					BloomPageSize:  10 << 10,
				},
				writer,
			)
			require.Nil(t, err)

			builder2, err := NewBlockBuilder(
				BlockOptions{
					schema:         schema,
					SeriesPageSize: 100,
					BloomPageSize:  10 << 10,
				},
				writer2,
			)
			require.Nil(t, err)

			data1, _ := mkBasicSeriesWithBlooms(4, 100, 0, tc.fingerprint1, tc.timestamp1, 10000)
			data2, _ := mkBasicSeriesWithBlooms(4, 100, 0, tc.fingerprint2, tc.timestamp2, 10000)

			itr1 := NewSliceIter[SeriesWithBloom](data1)
			checksum1, err := builder.BuildFrom(itr1)
			require.NoError(t, err)

			itr2 := NewSliceIter[SeriesWithBloom](data2)
			checksum2, err := builder2.BuildFrom(itr2)
			require.NoError(t, err)

			if tc.expectEqual {
				require.Equal(t, checksum1, checksum2, "checksums should be equal")
			} else {
				require.NotEqual(t, checksum1, checksum2, "checksums should not be equal")
			}
		})
	}
}

// This test is a basic roundtrip test for the merge builder.
// It creates one set of blocks with the same (duplicate) data, and another set of blocks with
// disjoint data. It then merges the two sets of blocks and ensures that the merged blocks contain
// one copy of the first set (duplicate data) and one copy of the second set (disjoint data).
func TestMergeBuilder_Roundtrip(t *testing.T) {
	numSeries := 100
	numKeysPerSeries := 100
	minTs, maxTs := model.Time(0), model.Time(10000)
	xs, _ := mkBasicSeriesWithBlooms(numSeries, numKeysPerSeries, 0, 0xffff, minTs, maxTs)

	var data [][]*SeriesWithBloom

	// First, we build the blocks

	sets := []int{
		2, // 2 blocks containint the same data
		1, // 1 block containing disjoint data
	}

	for i, copies := range sets {
		for j := 0; j < copies; j++ {
			// references for linking in memory reader+writer
			indexBuf := bytes.NewBuffer(nil)
			bloomsBuf := bytes.NewBuffer(nil)

			writer := NewMemoryBlockWriter(indexBuf, bloomsBuf)
			reader := NewByteReader(indexBuf, bloomsBuf)

			builder, err := NewBlockBuilder(
				BlockOptions{
					schema: Schema{
						version:  DefaultSchemaVersion,
						encoding: chunkenc.EncSnappy,
					},
					SeriesPageSize: 100,
					BloomPageSize:  10 << 10,
				},
				writer,
			)

			require.Nil(t, err)
			// each set of copies gets a different slice of the data
			minIdx, maxIdx := i*len(xs)/len(sets), (i+1)*len(xs)/len(sets)
			itr := NewSliceIter[SeriesWithBloom](xs[minIdx:maxIdx])
			_, err = builder.BuildFrom(itr)
			require.Nil(t, err)
			block := NewBlock(reader)
			querier := NewBlockQuerier(block)

			// rather than use the block querier directly, collect it's data
			// so we can use it in a few places later
			var tmp []*SeriesWithBloom
			for querier.Next() {
				tmp = append(tmp, querier.At())
			}
			data = append(data, tmp)
		}
	}

	// we keep 2 copies of the data as iterators. One for the blocks, and one for the "store"
	// which will force it to reference the same series
	var blocks []PeekingIterator[*SeriesWithBloom]
	var store []PeekingIterator[*SeriesWithBloom]

	for _, x := range data {
		blocks = append(blocks, NewPeekingIter[*SeriesWithBloom](NewSliceIter[*SeriesWithBloom](x)))
		store = append(store, NewPeekingIter[*SeriesWithBloom](NewSliceIter[*SeriesWithBloom](x)))
	}

	orderedStore := NewHeapIterForSeriesWithBloom(store...)
	dedupedStore := NewDedupingIter[*SeriesWithBloom, *Series](
		func(a *SeriesWithBloom, b *Series) bool {
			return a.Series.Fingerprint == b.Fingerprint
		},
		func(swb *SeriesWithBloom) *Series {
			return swb.Series
		},
		func(a *SeriesWithBloom, b *Series) *Series {
			if len(a.Series.Chunks) > len(b.Chunks) {
				return a.Series
			}
			return b
		},
		NewPeekingIter[*SeriesWithBloom](orderedStore),
	)

	// build the new block from the old ones
	indexBuf, bloomBuf := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	writer := NewMemoryBlockWriter(indexBuf, bloomBuf)
	reader := NewByteReader(indexBuf, bloomBuf)
	mb := NewMergeBuilder(
		blocks,
		dedupedStore,
		func(s *Series, b *Bloom) error {
			// We're not actually indexing new data in this test
			return nil
		},
	)
	builder, err := NewBlockBuilder(NewBlockOptions(4, 0), writer)
	require.Nil(t, err)

	checksum, err := mb.Build(builder)
	require.Nil(t, err)
	require.Equal(t, uint32(0x779633b5), checksum)

	// ensure the new block contains one copy of all the data
	// by comparing it against an iterator over the source data
	mergedBlockQuerier := NewBlockQuerier(NewBlock(reader))
	sourceItr := NewSliceIter[*SeriesWithBloom](PointerSlice[SeriesWithBloom](xs))

	EqualIterators[*SeriesWithBloom](
		t,
		func(a, b *SeriesWithBloom) {
			require.Equal(t, a.Series.Fingerprint, b.Series.Fingerprint)
		},
		sourceItr,
		mergedBlockQuerier,
	)
}
