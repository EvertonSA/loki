package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/opentracing/opentracing-go"

	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/storage/stores/index/seriesvolume"

	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/concurrency"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/storage/chunk"
	"github.com/grafana/loki/pkg/storage/chunk/fetcher"
	"github.com/grafana/loki/pkg/storage/config"
	"github.com/grafana/loki/pkg/storage/stores/index/stats"
	util_log "github.com/grafana/loki/pkg/util/log"
	"github.com/grafana/loki/pkg/util/spanlogger"
)

type AsyncStoreCfg struct {
	IngesterQuerier IngesterQuerier
	// QueryIngestersWithin defines maximum lookback beyond which ingesters are not queried for chunk ids.
	QueryIngestersWithin time.Duration
}

// AsyncStore does querying to both ingesters and chunk store and combines the results after deduping them.
// This should be used when using an async store like boltdb-shipper.
// AsyncStore is meant to be used only in queriers or any other service other than ingesters.
// It should never be used in ingesters otherwise it would start spiraling around doing queries over and over again to other ingesters.
type AsyncStore struct {
	store                ReadStore
	scfg                 config.SchemaConfig
	ingesterQuerier      IngesterQuerier
	queryIngestersWithin time.Duration
}

// Enforce ReadWriteStore interface implementation of AsyncStore.
// AsyncStore needs to implement the full ReadWriteStore interface, because it is
// assigned to the application's main Loki struct.
// TODO(chaudum): Come up with a solution where the AsyncStore only needs to
// implement the ReadStore interface.
var _ ReadWriteStore = &AsyncStore{}

func NewAsyncStore(cfg AsyncStoreCfg, store ReadStore, scfg config.SchemaConfig) *AsyncStore {
	return &AsyncStore{
		store:                store,
		scfg:                 scfg,
		ingesterQuerier:      cfg.IngesterQuerier,
		queryIngestersWithin: cfg.QueryIngestersWithin,
	}
}

// queryIngesters uses the queryIngestersWithin flag but will always query them when it's 0.
func (a *AsyncStore) shouldQueryIngesters(through, now model.Time) bool {
	// don't query ingesters if the query does not overlap with queryIngestersWithin.
	return a.queryIngestersWithin == 0 || through.After(now.Add(-a.queryIngestersWithin))
}

func (a *AsyncStore) GetChunkRefs(ctx context.Context, userID string, from, through model.Time, matchers ...*labels.Matcher) ([][]chunk.Chunk, []*fetcher.Fetcher, error) {
	spanLogger := spanlogger.FromContext(ctx)

	errs := make(chan error)

	var storeChunks [][]chunk.Chunk
	var fetchers []*fetcher.Fetcher
	go func() {
		var err error
		storeChunks, fetchers, err = a.store.GetChunkRefs(ctx, userID, from, through, matchers...)
		errs <- err
	}()

	var ingesterChunks []string

	go func() {
		if !a.shouldQueryIngesters(through, model.Now()) {
			level.Debug(util_log.Logger).Log("msg", "skipping querying ingesters for chunk ids", "query-from", from, "query-through", through)
			errs <- nil
			return
		}

		var err error
		ingesterChunks, err = a.ingesterQuerier.GetChunkIDs(ctx, from, through, matchers...)

		if err == nil {
			level.Debug(spanLogger).Log("ingester-chunks-count", len(ingesterChunks))
			level.Debug(util_log.Logger).Log("msg", "got chunk ids from ingester", "count", len(ingesterChunks))
		}
		errs <- err
	}()

	for i := 0; i < 2; i++ {
		err := <-errs
		if err != nil {
			return nil, nil, err
		}
	}

	if len(ingesterChunks) == 0 {
		return storeChunks, fetchers, nil
	}

	return a.mergeIngesterAndStoreChunks(userID, storeChunks, fetchers, ingesterChunks)
}

func (a *AsyncStore) Stats(ctx context.Context, userID string, from, through model.Time, matchers ...*labels.Matcher) (*stats.Stats, error) {
	logger := util_log.WithContext(ctx, util_log.Logger)
	matchersStr := syntax.MatchersString(matchers)
	type f func() (*stats.Stats, error)
	var jobs []f

	if a.shouldQueryIngesters(through, model.Now()) {
		jobs = append(jobs, f(func() (*stats.Stats, error) {
			stats, err := a.ingesterQuerier.Stats(ctx, userID, from, through, matchers...)
			level.Debug(logger).Log(
				append(
					stats.LoggingKeyValues(),
					"msg", "queried statistics",
					"matchers", matchersStr,
					"source", "ingesters",
				)...,
			)
			return stats, err
		}))
	}
	jobs = append(jobs, f(func() (*stats.Stats, error) {
		stats, err := a.store.Stats(ctx, userID, from, through, matchers...)
		level.Debug(logger).Log(
			append(
				stats.LoggingKeyValues(),
				"msg", "queried statistics",
				"matchers", matchersStr,
				"source", "store",
			)...,
		)
		return stats, err
	}))

	resps := make([]*stats.Stats, len(jobs))
	if err := concurrency.ForEachJob(
		ctx,
		len(jobs),
		len(jobs),
		func(ctx context.Context, i int) error {
			resp, err := jobs[i]()
			resps[i] = resp
			return err
		},
	); err != nil {
		return nil, err
	}

	// TODO: fix inflated stats. This happens because:
	//       - All ingesters are queried. Since we have a replication factor of 3, we get 3x the stats.
	//       - For the same timespan, we are querying the store as well. This means we can get duplicated stats for
	//         chunks that are already in the store but also still in the ingesters.
	merged := stats.MergeStats(resps...)
	return &merged, nil
}

func (a *AsyncStore) Volume(ctx context.Context, userID string, from, through model.Time, limit int32, targetLabels []string, aggregateBy string, matchers ...*labels.Matcher) (*logproto.VolumeResponse, error) {
	sp, ctx := opentracing.StartSpanFromContext(ctx, "AsyncStore.Volume")
	defer sp.Finish()

	logger := util_log.WithContext(ctx, util_log.Logger)
	matchersStr := syntax.MatchersString(matchers)
	type f func() (*logproto.VolumeResponse, error)
	var jobs []f

	if a.shouldQueryIngesters(through, model.Now()) {
		jobs = append(jobs, func() (*logproto.VolumeResponse, error) {
			vols, err := a.ingesterQuerier.Volume(ctx, userID, from, through, limit, targetLabels, aggregateBy, matchers...)
			level.Debug(logger).Log(
				"msg", "queried label volumes",
				"matchers", matchersStr,
				"source", "ingesters",
			)
			return vols, err
		})
	}
	jobs = append(jobs, func() (*logproto.VolumeResponse, error) {
		vols, err := a.store.Volume(ctx, userID, from, through, limit, targetLabels, aggregateBy, matchers...)
		level.Debug(logger).Log(
			"msg", "queried label volume",
			"matchers", matchersStr,
			"source", "store",
		)
		return vols, err
	})

	resps := make([]*logproto.VolumeResponse, len(jobs))
	if err := concurrency.ForEachJob(
		ctx,
		len(jobs),
		len(jobs),
		func(ctx context.Context, i int) error {
			resp, err := jobs[i]()
			resps[i] = resp
			return err
		},
	); err != nil {
		return nil, err
	}

	sp.LogKV(
		"user", userID,
		"from", from.Time(),
		"through", through.Time(),
		"matchers", syntax.MatchersString(matchers),
		"limit", limit,
	)

	merged := seriesvolume.Merge(resps, limit)
	return merged, nil
}

func (a *AsyncStore) mergeIngesterAndStoreChunks(userID string, storeChunks [][]chunk.Chunk, fetchers []*fetcher.Fetcher, ingesterChunkIDs []string) ([][]chunk.Chunk, []*fetcher.Fetcher, error) {
	ingesterChunkIDs = filterDuplicateChunks(a.scfg, storeChunks, ingesterChunkIDs)
	level.Debug(util_log.Logger).Log("msg", "post-filtering ingester chunks", "count", len(ingesterChunkIDs))

	fetcherToChunksGroupIdx := make(map[*fetcher.Fetcher]int, len(fetchers))

	for i, fetcher := range fetchers {
		fetcherToChunksGroupIdx[fetcher] = i
	}

	for _, chunkID := range ingesterChunkIDs {
		chk, err := chunk.ParseExternalKey(userID, chunkID)
		if err != nil {
			return nil, nil, err
		}

		// ToDo(Sandeep) possible optimization: Keep the chunk fetcher reference handy after first call since it is expected to stay the same.
		fetcher := a.store.GetChunkFetcher(chk.Through)
		if fetcher == nil {
			return nil, nil, fmt.Errorf("got a nil fetcher for chunk %s", a.scfg.ExternalKey(chk.ChunkRef))
		}

		if _, ok := fetcherToChunksGroupIdx[fetcher]; !ok {
			fetchers = append(fetchers, fetcher)
			storeChunks = append(storeChunks, []chunk.Chunk{})
			fetcherToChunksGroupIdx[fetcher] = len(fetchers) - 1
		}
		chunksGroupIdx := fetcherToChunksGroupIdx[fetcher]

		storeChunks[chunksGroupIdx] = append(storeChunks[chunksGroupIdx], chk)
	}

	return storeChunks, fetchers, nil
}

func filterDuplicateChunks(scfg config.SchemaConfig, storeChunks [][]chunk.Chunk, ingesterChunkIDs []string) []string {
	filteredChunkIDs := make([]string, 0, len(ingesterChunkIDs))
	seen := make(map[string]struct{}, len(storeChunks))

	for i := range storeChunks {
		for j := range storeChunks[i] {
			seen[scfg.ExternalKey(storeChunks[i][j].ChunkRef)] = struct{}{}
		}
	}

	for _, chunkID := range ingesterChunkIDs {
		if _, ok := seen[chunkID]; !ok {
			filteredChunkIDs = append(filteredChunkIDs, chunkID)
			seen[chunkID] = struct{}{}
		}
	}

	return filteredChunkIDs
}

// GetChunkFetcher implements stores.Store.
func (*AsyncStore) GetChunkFetcher(tm model.Time) *fetcher.Fetcher {
	panic("unimplemented")
}

// GetSeries implements stores.Store.
func (*AsyncStore) GetSeries(ctx context.Context, userID string, from model.Time, through model.Time, matchers ...*labels.Matcher) ([]labels.Labels, error) {
	panic("unimplemented")
}

// LabelNamesForMetricName implements stores.Store.
func (*AsyncStore) LabelNamesForMetricName(ctx context.Context, userID string, from model.Time, through model.Time, metricName string) ([]string, error) {
	panic("unimplemented")
}

// LabelValuesForMetricName implements stores.Store.
func (*AsyncStore) LabelValuesForMetricName(ctx context.Context, userID string, from model.Time, through model.Time, metricName string, labelName string, matchers ...*labels.Matcher) ([]string, error) {
	panic("unimplemented")
}

// Put implements stores.Store.
func (*AsyncStore) Put(ctx context.Context, chunks []chunk.Chunk) error {
	panic("unimplemented")
}

// PutOne implements stores.Store.
func (*AsyncStore) PutOne(ctx context.Context, from model.Time, through model.Time, chunk chunk.Chunk) error {
	panic("unimplemented")
}

// SetChunkFilterer implements stores.Store.
func (*AsyncStore) SetChunkFilterer(chunkFilter chunk.RequestChunkFilterer) {
	panic("unimplemented")
}

// Stop implements stores.Store.
func (s *AsyncStore) Stop() {
	s.store.Stop()
}
