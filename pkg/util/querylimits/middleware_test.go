package querylimits

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
)

func Test_MiddlewareWithoutHeader(t *testing.T) {
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limits := ExtractQueryLimitsContext(r.Context())
		require.Nil(t, limits)
	})
	m := NewQueryLimitsMiddleware(log.NewNopLogger())
	wrapped := m.Wrap(nextHandler)

	rr := httptest.NewRecorder()
	r, err := http.NewRequest("GET", "/example", nil)
	require.NoError(t, err)
	wrapped.ServeHTTP(rr, r)
}

func Test_MiddlewareWithHeader(t *testing.T) {
	limits := QueryLimits{
		MaxQueryLength:          model.Duration(1 * time.Second),
		MaxQueryLookback:        model.Duration(1 * time.Second),
		MaxEntriesLimitPerQuery: 1,
		QueryTimeout:            model.Duration(1 * time.Second),
		RequiredLabels:          []string{"cluster"},
		MaxInterval:             model.Duration(15 * time.Second),
	}

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual := ExtractQueryLimitsContext(r.Context())
		require.Equal(t, limits, *actual)
	})
	m := NewQueryLimitsMiddleware(log.NewNopLogger())
	wrapped := m.Wrap(nextHandler)

	rr := httptest.NewRecorder()
	r, err := http.NewRequest("GET", "/example", nil)
	require.NoError(t, err)
	err = InjectQueryLimitsHTTP(r, &limits)
	require.NoError(t, err)
	wrapped.ServeHTTP(rr, r)
}
