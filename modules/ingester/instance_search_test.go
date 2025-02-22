package ingester

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/tempo/modules/overrides"
	"github.com/grafana/tempo/pkg/model"
	"github.com/grafana/tempo/pkg/tempofb"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/grafana/tempo/tempodb/search"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkEqual(t *testing.T, ids [][]byte, sr *tempopb.SearchResponse) {
	for _, meta := range sr.Traces {
		parsedTraceID, err := util.HexStringToTraceID(meta.TraceID)
		assert.NoError(t, err)

		present := false
		for _, id := range ids {
			if bytes.Equal(parsedTraceID, id) {
				present = true
			}
		}
		assert.True(t, present)
	}
}

func TestInstanceSearch(t *testing.T) {
	limits, err := overrides.NewOverrides(overrides.Limits{})
	assert.NoError(t, err, "unexpected error creating limits")
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	tempDir, err := ioutil.TempDir("/tmp", "")
	assert.NoError(t, err, "unexpected error getting temp dir")
	defer os.RemoveAll(tempDir)

	ingester, _, _ := defaultIngester(t, tempDir)
	i, err := newInstance("fake", limiter, ingester.store, ingester.local)
	assert.NoError(t, err, "unexpected error creating new instance")

	numTraces := 500
	searchAnnotatedFractionDenominator := 100
	ids := [][]byte{}

	// add dummy search data
	var tagKey = "foo"
	var tagValue = "bar"

	for j := 0; j < numTraces; j++ {
		id := make([]byte, 16)
		rand.Read(id)

		trace := test.MakeTrace(10, id)
		model.SortTrace(trace)
		traceBytes, err := trace.Marshal()
		require.NoError(t, err)

		// annotate just a fraction of traces with search data
		var searchData []byte
		if j%searchAnnotatedFractionDenominator == 0 {
			data := &tempofb.SearchEntryMutable{}
			data.TraceID = id
			data.AddTag(tagKey, tagValue)
			searchData = data.ToBytes()

			// these are the only ids we want to test against
			ids = append(ids, id)
		}

		// searchData will be nil if not
		err = i.PushBytes(context.Background(), id, traceBytes, searchData)
		require.NoError(t, err)

		assert.Equal(t, int(i.traceCount.Load()), len(i.traces))
	}

	var req = &tempopb.SearchRequest{
		Tags: map[string]string{},
	}
	req.Tags[tagKey] = tagValue

	sr, err := i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, numTraces/searchAnnotatedFractionDenominator)
	// todo: test that returned results are in sorted time order, create order of id's beforehand
	checkEqual(t, ids, sr)

	// Test after appending to WAL
	err = i.CutCompleteTraces(0, true)
	require.NoError(t, err)
	assert.Equal(t, int(i.traceCount.Load()), len(i.traces))

	sr, err = i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, numTraces/searchAnnotatedFractionDenominator)
	checkEqual(t, ids, sr)

	// Test after cutting new headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	assert.NotEqual(t, blockID, uuid.Nil)

	sr, err = i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, numTraces/searchAnnotatedFractionDenominator)
	checkEqual(t, ids, sr)

	// Test after completing a block
	err = i.CompleteBlock(blockID)
	require.NoError(t, err)

	sr, err = i.Search(context.Background(), req)
	assert.NoError(t, err)
	assert.Len(t, sr.Traces, numTraces/searchAnnotatedFractionDenominator)
	checkEqual(t, ids, sr)

	err = ingester.stopping(nil)
	require.NoError(t, err)

	// create new ingester.  this should replay wal!
	ingester, _, _ = defaultIngester(t, tempDir)

	i, ok := ingester.getInstanceByID("fake")
	assert.True(t, ok)

	sr, err = i.Search(context.Background(), req)
	assert.NoError(t, err)
	// note: search is experimental and removed on every startup. Verify no search results now
	assert.Len(t, sr.Traces, 0)
}

func TestInstanceSearchNoData(t *testing.T) {
	limits, err := overrides.NewOverrides(overrides.Limits{})
	assert.NoError(t, err, "unexpected error creating limits")
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	tempDir, err := ioutil.TempDir("/tmp", "")
	assert.NoError(t, err, "unexpected error getting temp dir")
	defer os.RemoveAll(tempDir)

	ingester, _, _ := defaultIngester(t, tempDir)
	i, err := newInstance("fake", limiter, ingester.store, ingester.local)
	assert.NoError(t, err, "unexpected error creating new instance")

	var req = &tempopb.SearchRequest{
		Tags: map[string]string{},
	}

	sr, err := i.Search(context.Background(), req)
	assert.NoError(t, err)
	require.Len(t, sr.Traces, 0)
}

func TestInstanceSearchDoesNotRace(t *testing.T) {
	limits, err := overrides.NewOverrides(overrides.Limits{})
	require.NoError(t, err)
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	ingester, _, _ := defaultIngester(t, t.TempDir())
	i, err := newInstance("fake", limiter, ingester.store, ingester.local)
	require.NoError(t, err)

	// add dummy search data
	var tagKey = "foo"
	var tagValue = "bar"

	var req = &tempopb.SearchRequest{
		Tags: map[string]string{tagKey: tagValue},
	}

	end := make(chan struct{})

	concurrent := func(f func()) {
		for {
			select {
			case <-end:
				return
			default:
				f()
			}
		}
	}

	go concurrent(func() {
		id := make([]byte, 16)
		rand.Read(id)

		trace := test.MakeTrace(10, id)
		traceBytes, err := trace.Marshal()
		require.NoError(t, err)

		searchData := &tempofb.SearchEntryMutable{}
		searchData.TraceID = id
		searchData.AddTag(tagKey, tagValue)
		searchBytes := searchData.ToBytes()

		// searchData will be nil if not
		err = i.PushBytes(context.Background(), id, traceBytes, searchBytes)
		require.NoError(t, err)
	})

	go concurrent(func() {
		err := i.CutCompleteTraces(0, true)
		require.NoError(t, err, "error cutting complete traces")
	})

	go concurrent(func() {
		_, err := i.FindTraceByID(context.Background(), []byte{0x01})
		assert.NoError(t, err, "error finding trace by id")
	})

	go concurrent(func() {
		// Cut wal, complete, delete wal, then flush
		blockID, _ := i.CutBlockIfReady(0, 0, true)
		if blockID != uuid.Nil {
			err := i.CompleteBlock(blockID)
			require.NoError(t, err)
			err = i.ClearCompletingBlock(blockID)
			require.NoError(t, err)
			block := i.GetBlockToBeFlushed(blockID)
			require.NotNil(t, block)
			err = ingester.store.WriteBlock(context.Background(), block)
			require.NoError(t, err)
		}
	})

	go concurrent(func() {
		err = i.ClearFlushedBlocks(0)
		require.NoError(t, err)
	})

	go concurrent(func() {
		_, err := i.Search(context.Background(), req)
		require.NoError(t, err, "error finding trace by id")
	})

	go concurrent(func() {
		i.GetSearchTags()
	})

	go concurrent(func() {
		i.GetSearchTagValues(tagKey)
	})

	time.Sleep(2000 * time.Millisecond)
	close(end)
	// Wait for go funcs to quit before
	// exiting and cleaning up
	time.Sleep(2 * time.Second)
}

func TestWALBlockDeletedDuringSearch(t *testing.T) {
	limits, err := overrides.NewOverrides(overrides.Limits{})
	require.NoError(t, err)
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	ingester, _, _ := defaultIngester(t, t.TempDir())
	i, err := newInstance("fake", limiter, ingester.store, ingester.local)
	require.NoError(t, err)

	end := make(chan struct{})

	concurrent := func(f func()) {
		for {
			select {
			case <-end:
				return
			default:
				f()
			}
		}
	}

	for j := 0; j < 500; j++ {
		id := make([]byte, 16)
		rand.Read(id)

		trace := test.MakeTrace(10, id)
		traceBytes, err := trace.Marshal()
		require.NoError(t, err)

		entry := &tempofb.SearchEntryMutable{}
		entry.TraceID = id
		entry.AddTag("foo", "bar")
		searchBytes := entry.ToBytes()

		err = i.PushBytes(context.Background(), id, traceBytes, searchBytes)
		require.NoError(t, err)
	}

	err = i.CutCompleteTraces(0, true)
	require.NoError(t, err)

	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)

	go concurrent(func() {
		_, err := i.Search(context.Background(), &tempopb.SearchRequest{
			Tags: map[string]string{
				// Not present in the data, so it will be an exhaustive
				// search
				"wuv": "xyz",
			},
		})
		require.NoError(t, err)
	})

	// Let search get going
	time.Sleep(100 * time.Millisecond)

	err = i.ClearCompletingBlock(blockID)
	require.NoError(t, err)

	// Wait for go funcs to quit before
	// exiting and cleaning up
	close(end)
	time.Sleep(2 * time.Second)
}

func TestInstanceSearchMetrics(t *testing.T) {

	i := defaultInstance(t, t.TempDir())

	numTraces := uint32(500)
	numBytes := uint64(0)
	for j := uint32(0); j < numTraces; j++ {
		id := make([]byte, 16)
		rand.Read(id)

		trace := test.MakeTrace(10, id)
		traceBytes, err := trace.Marshal()
		require.NoError(t, err)

		data := &tempofb.SearchEntryMutable{}
		data.TraceID = id
		data.AddTag("foo", "bar")
		searchData := data.ToBytes()

		numBytes += uint64(len(searchData))

		err = i.PushBytes(context.Background(), id, traceBytes, searchData)
		require.NoError(t, err)

		assert.Equal(t, int(i.traceCount.Load()), len(i.traces))
	}

	search := func() *tempopb.SearchMetrics {
		sr, err := i.Search(context.Background(), &tempopb.SearchRequest{
			// Exhaustive search
			Tags: map[string]string{search.SecretExhaustiveSearchTag: "!"},
		})
		require.NoError(t, err)
		return sr.Metrics
	}

	// Live traces
	m := search()
	require.Equal(t, numTraces, m.InspectedTraces)
	require.Equal(t, numBytes, m.InspectedBytes)
	require.Equal(t, uint32(1), m.InspectedBlocks) // 1 head block

	// Test after appending to WAL
	err := i.CutCompleteTraces(0, true)
	require.NoError(t, err)
	m = search()
	require.Equal(t, numTraces, m.InspectedTraces)
	require.Equal(t, numBytes, m.InspectedBytes)
	require.Equal(t, uint32(1), m.InspectedBlocks) // 1 head block

	// Test after cutting new headblock
	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	m = search()
	require.Equal(t, numTraces, m.InspectedTraces)
	require.Equal(t, numBytes, m.InspectedBytes)
	require.Equal(t, uint32(2), m.InspectedBlocks) // 1 head block, 1 completing block

	// Test after completing a block
	err = i.CompleteBlock(blockID)
	require.NoError(t, err)
	err = i.ClearCompletingBlock(blockID)
	require.NoError(t, err)
	// Complete blocks are paged and search data is normalized, therefore smaller than individual wal entries.
	m = search()
	require.Equal(t, numTraces, m.InspectedTraces)
	require.Less(t, m.InspectedBytes, numBytes)
	require.Equal(t, uint32(2), m.InspectedBlocks) // 1 head block, 1 complete block
}

func BenchmarkInstanceSearchUnderLoad(b *testing.B) {
	ctx := context.TODO()
	//n := 1_000_000

	i := defaultInstance(b, b.TempDir())

	end := make(chan struct{})

	concurrent := func(f func()) {
		for {
			select {
			case <-end:
				return
			default:
				f()
			}
		}
	}

	go concurrent(func() {
		id := make([]byte, 16)
		rand.Read(id)

		trace := test.MakeTrace(10, id)
		traceBytes, err := trace.Marshal()
		require.NoError(b, err)

		searchData := &tempofb.SearchEntryMutable{}
		searchData.TraceID = id
		searchData.AddTag("foo", "bar")
		searchBytes := searchData.ToBytes()

		// searchData will be nil if not
		err = i.PushBytes(context.Background(), id, traceBytes, searchBytes)
		require.NoError(b, err)
	})

	go concurrent(func() {
		err := i.CutCompleteTraces(0, true)
		require.NoError(b, err, "error cutting complete traces")
	})

	go concurrent(func() {
		// Slow this down to prevent "too many open files" error
		time.Sleep(10 * time.Millisecond)
		_, err := i.CutBlockIfReady(0, 0, true)
		require.NoError(b, err)
	})

	b.ResetTimer()
	start := time.Now()
	bytesInspected := uint64(0)
	for j := 0; j < b.N; j++ {
		var req = &tempopb.SearchRequest{
			Tags: map[string]string{"nomatch": "nomatch"},
		}
		resp, err := i.Search(ctx, req)
		require.NoError(b, err)
		bytesInspected += resp.Metrics.InspectedBytes
	}
	elapsed := time.Since(start)

	fmt.Printf("Instance search throughput under load: %v elapsed %.2f MB = %.2f MiB/s throughput \n",
		elapsed,
		float64(bytesInspected)/(1024*1024),
		float64(bytesInspected)/(elapsed.Seconds())/(1024*1024))

	b.StopTimer()
	close(end)
	// Wait for go funcs to quit before
	// exiting and cleaning up
	time.Sleep(1 * time.Second)
}
