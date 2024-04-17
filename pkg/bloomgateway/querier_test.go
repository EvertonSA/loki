package bloomgateway

import (
	"context"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/querier/plan"
)

type noopClient struct {
	err       error // error to return
	callCount int
}

// FilterChunks implements Client.
func (c *noopClient) FilterChunks(ctx context.Context, tenant string, from, through model.Time, groups []*logproto.GroupedChunkRefs, plan plan.QueryPlan) ([]*logproto.GroupedChunkRefs, error) { // nolint:revive
	c.callCount++
	return groups, c.err
}

func TestBloomQuerier(t *testing.T) {
	logger := log.NewNopLogger()
	tenant := "fake"

	t.Run("client not called when filters are empty", func(t *testing.T) {
		c := &noopClient{}
		bq := NewQuerier(c, nil, logger)

		ctx := context.Background()
		through := model.Now()
		from := through.Add(-12 * time.Hour)
		chunkRefs := []*logproto.ChunkRef{
			{Fingerprint: 3000, UserID: tenant, Checksum: 1},
			{Fingerprint: 1000, UserID: tenant, Checksum: 2},
			{Fingerprint: 2000, UserID: tenant, Checksum: 3},
		}
		expr, err := syntax.ParseExpr(`{foo="bar"}`)
		require.NoError(t, err)
		res, err := bq.FilterChunkRefs(ctx, tenant, from, through, chunkRefs, plan.QueryPlan{AST: expr})
		require.NoError(t, err)
		require.Equal(t, chunkRefs, res)
		require.Equal(t, 0, c.callCount)
	})

	t.Run("client not called when chunkRefs are empty", func(t *testing.T) {
		c := &noopClient{}
		bq := NewQuerier(c, nil, logger)

		ctx := context.Background()
		through := model.Now()
		from := through.Add(-12 * time.Hour)
		chunkRefs := []*logproto.ChunkRef{}
		expr, err := syntax.ParseExpr(`{foo="bar"} |= "uuid"`)
		require.NoError(t, err)
		res, err := bq.FilterChunkRefs(ctx, tenant, from, through, chunkRefs, plan.QueryPlan{AST: expr})
		require.NoError(t, err)
		require.Equal(t, chunkRefs, res)
		require.Equal(t, 0, c.callCount)
	})
	t.Run("querier propagates error from client", func(t *testing.T) {
		c := &noopClient{err: errors.New("something went wrong")}
		bq := NewQuerier(c, nil, logger)

		ctx := context.Background()
		through := model.Now()
		from := through.Add(-12 * time.Hour)
		chunkRefs := []*logproto.ChunkRef{
			{Fingerprint: 3000, UserID: tenant, From: from, Through: through, Checksum: 1},
			{Fingerprint: 1000, UserID: tenant, From: from, Through: through, Checksum: 2},
			{Fingerprint: 2000, UserID: tenant, From: from, Through: through, Checksum: 3},
		}
		expr, err := syntax.ParseExpr(`{foo="bar"} |= "uuid"`)
		require.NoError(t, err)
		res, err := bq.FilterChunkRefs(ctx, tenant, from, through, chunkRefs, plan.QueryPlan{AST: expr})
		require.Error(t, err)
		require.Nil(t, res)
	})

	t.Run("client called once for each day of the interval", func(t *testing.T) {
		c := &noopClient{}
		bq := NewQuerier(c, nil, logger)

		ctx := context.Background()
		from := mktime("2024-04-16 22:00")
		through := mktime("2024-04-17 02:00")
		chunkRefs := []*logproto.ChunkRef{
			{Fingerprint: 1000, UserID: tenant, From: mktime("2024-04-16 22:30"), Through: mktime("2024-04-16 23:30"), Checksum: 1}, // day 1
			{Fingerprint: 2000, UserID: tenant, From: mktime("2024-04-16 23:30"), Through: mktime("2024-04-17 00:30"), Checksum: 2}, // day 1
			{Fingerprint: 3000, UserID: tenant, From: mktime("2024-04-17 00:30"), Through: mktime("2024-04-17 01:30"), Checksum: 3}, // day 2
		}
		expr, err := syntax.ParseExpr(`{foo="bar"} |= "uuid"`)
		require.NoError(t, err)
		res, err := bq.FilterChunkRefs(ctx, tenant, from, through, chunkRefs, plan.QueryPlan{AST: expr})
		require.NoError(t, err)
		require.Equal(t, chunkRefs, res)
		require.Equal(t, 2, c.callCount)
	})

}
