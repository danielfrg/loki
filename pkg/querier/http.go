package querier

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/websocket"
	"github.com/grafana/dskit/httpgrpc"
	"github.com/grafana/dskit/middleware"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/grafana/dskit/tenant"

	"github.com/grafana/loki/pkg/loghttp"
	loghttp_legacy "github.com/grafana/loki/pkg/loghttp/legacy"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/logqlmodel"
	"github.com/grafana/loki/pkg/logqlmodel/stats"
	"github.com/grafana/loki/pkg/querier/queryrange"
	index_stats "github.com/grafana/loki/pkg/storage/stores/index/stats"
	"github.com/grafana/loki/pkg/util/httpreq"
	util_log "github.com/grafana/loki/pkg/util/log"
	"github.com/grafana/loki/pkg/util/marshal"
	marshal_legacy "github.com/grafana/loki/pkg/util/marshal/legacy"
	serverutil "github.com/grafana/loki/pkg/util/server"
	"github.com/grafana/loki/pkg/util/spanlogger"
	util_validation "github.com/grafana/loki/pkg/util/validation"
)

const (
	wsPingPeriod = 1 * time.Second
)

type QueryResponse struct {
	ResultType parser.ValueType `json:"resultType"`
	Result     parser.Value     `json:"result"`
}

type Engine interface {
	Query(logql.Params) logql.Query
}

// nolint // QuerierAPI defines HTTP handler functions for the querier.
type QuerierAPI struct {
	querier Querier
	cfg     Config
	limits  Limits
	engine  Engine
}

// NewQuerierAPI returns an instance of the QuerierAPI.
func NewQuerierAPI(cfg Config, querier Querier, limits Limits, logger log.Logger) *QuerierAPI {
	engine := logql.NewEngine(cfg.Engine, querier, limits, logger)
	return &QuerierAPI{
		cfg:     cfg,
		limits:  limits,
		querier: querier,
		engine:  engine,
	}
}

// RangeQueryHandler is a http.HandlerFunc for range queries.
func (q *QuerierAPI) RangeQueryHandler(ctx context.Context, req *queryrange.LokiRequest) (logqlmodel.Result, error) {
	params, err := queryrange.ParamsFromRequest(req)
	if err != nil {
		return logqlmodel.Result{}, err
	}

	query := q.engine.Query(params)
	return query.Exec(ctx)
}

// InstantQueryHandler is a http.HandlerFunc for instant queries.
func (q *QuerierAPI) InstantQueryHandler(ctx context.Context, req *queryrange.LokiInstantRequest) (logqlmodel.Result, error) {
	if err := q.validateMaxEntriesLimits(ctx, req.Query, req.Limit); err != nil {
		return logqlmodel.Result{}, err
	}

	params, err := queryrange.ParamsFromRequest(req)
	if err != nil {
		return logqlmodel.Result{}, err
	}
	query := q.engine.Query(params)
	return query.Exec(ctx)
}

// LogQueryHandler is a http.HandlerFunc for log only queries.
func (q *QuerierAPI) LogQueryHandler(w http.ResponseWriter, r *http.Request) {
	request, err := loghttp.ParseRangeQuery(r)
	if err != nil {
		serverutil.WriteError(httpgrpc.Errorf(http.StatusBadRequest, err.Error()), w)
		return
	}
	request.Query, err = parseRegexQuery(r)
	if err != nil {
		serverutil.WriteError(httpgrpc.Errorf(http.StatusBadRequest, err.Error()), w)
		return
	}

	expr, err := syntax.ParseExpr(request.Query)
	if err != nil {
		serverutil.WriteError(err, w)
		return
	}

	// short circuit metric queries
	if _, ok := expr.(syntax.SampleExpr); ok {
		serverutil.WriteError(httpgrpc.Errorf(http.StatusBadRequest, "legacy endpoints only support %s result type", logqlmodel.ValueTypeStreams), w)
		return
	}

	ctx := r.Context()
	if err := q.validateMaxEntriesLimits(ctx, request.Query, request.Limit); err != nil {
		serverutil.WriteError(err, w)
		return
	}

	params := logql.NewLiteralParams(
		request.Query,
		request.Start,
		request.End,
		request.Step,
		request.Interval,
		request.Direction,
		request.Limit,
		request.Shards,
	)
	query := q.engine.Query(params)

	result, err := query.Exec(ctx)
	if err != nil {
		serverutil.WriteError(err, w)
		return
	}

	if err := queryrange.WriteResponse(r, &params, result, w); err != nil {
		serverutil.WriteError(err, w)
	}
}

// LabelHandler is a http.HandlerFunc for handling label queries.
func (q *QuerierAPI) LabelHandler(ctx context.Context, req *logproto.LabelRequest) (*logproto.LabelResponse, error) {
	timer := prometheus.NewTimer(logql.QueryTime.WithLabelValues("labels"))
	defer timer.ObserveDuration()

	start := time.Now()
	statsCtx, ctx := stats.NewContext(ctx)

	resp, err := q.querier.Label(ctx, req)
	queueTime, _ := ctx.Value(httpreq.QueryQueueTimeHTTPHeader).(time.Duration)

	resLength := 0
	if resp != nil {
		resLength = len(resp.Values)
	}
	// record stats about the label query
	statResult := statsCtx.Result(time.Since(start), queueTime, resLength)
	log := spanlogger.FromContext(ctx)
	statResult.Log(level.Debug(log))

	status := 200
	if err != nil {
		status, _ = serverutil.ClientHTTPStatusAndError(err)
	}

	logql.RecordLabelQueryMetrics(ctx, log, *req.Start, *req.End, req.Name, req.Query, strconv.Itoa(status), statResult)

	return resp, err
}

// TailHandler is a http.HandlerFunc for handling tail queries.
func (q *QuerierAPI) TailHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	logger := util_log.WithContext(r.Context(), util_log.Logger)

	req, err := loghttp.ParseTailQuery(r)
	if err != nil {
		serverutil.WriteError(httpgrpc.Errorf(http.StatusBadRequest, err.Error()), w)
		return
	}

	req.Query, err = parseRegexQuery(r)
	if err != nil {
		serverutil.WriteError(httpgrpc.Errorf(http.StatusBadRequest, err.Error()), w)
		return
	}

	tenantID, err := tenant.TenantID(r.Context())
	if err != nil {
		level.Warn(logger).Log("msg", "error getting tenant id", "err", err)
		serverutil.WriteError(httpgrpc.Errorf(http.StatusBadRequest, err.Error()), w)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		level.Error(logger).Log("msg", "Error in upgrading websocket", "err", err)
		return
	}

	level.Info(logger).Log("msg", "starting to tail logs", "tenant", tenantID, "selectors", req.Query)

	defer func() {
		level.Info(logger).Log("msg", "ended tailing logs", "tenant", tenantID, "selectors", req.Query)
	}()

	defer func() {
		if err := conn.Close(); err != nil {
			level.Error(logger).Log("msg", "Error closing websocket", "err", err)
		}
	}()

	tailer, err := q.querier.Tail(r.Context(), req)
	if err != nil {
		if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
			level.Error(logger).Log("msg", "Error connecting to ingesters for tailing", "err", err)
		}
		return
	}
	defer func() {
		if err := tailer.close(); err != nil {
			level.Error(logger).Log("msg", "Error closing Tailer", "err", err)
		}
	}()

	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()

	var response *loghttp_legacy.TailResponse
	responseChan := tailer.getResponseChan()
	closeErrChan := tailer.getCloseErrorChan()

	doneChan := make(chan struct{})
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				if closeErr, ok := err.(*websocket.CloseError); ok {
					if closeErr.Code == websocket.CloseNormalClosure {
						break
					}
					level.Error(logger).Log("msg", "Error from client", "err", err)
					break
				} else if tailer.stopped {
					return
				} else {
					level.Error(logger).Log("msg", "Unexpected error from client", "err", err)
					break
				}
			}
		}
		doneChan <- struct{}{}
	}()

	for {
		select {
		case response = <-responseChan:
			var err error
			if loghttp.GetVersion(r.RequestURI) == loghttp.VersionV1 {
				err = marshal.WriteTailResponseJSON(*response, conn)
			} else {
				err = marshal_legacy.WriteTailResponseJSON(*response, conn)
			}
			if err != nil {
				level.Error(logger).Log("msg", "Error writing to websocket", "err", err)
				if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
					level.Error(logger).Log("msg", "Error writing close message to websocket", "err", err)
				}
				return
			}

		case err := <-closeErrChan:
			level.Error(logger).Log("msg", "Error from iterator", "err", err)
			if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
				level.Error(logger).Log("msg", "Error writing close message to websocket", "err", err)
			}
			return
		case <-ticker.C:
			// This is to periodically check whether connection is active, useful to clean up dead connections when there are no entries to send
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				level.Error(logger).Log("msg", "Error writing ping message to websocket", "err", err)
				if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
					level.Error(logger).Log("msg", "Error writing close message to websocket", "err", err)
				}
				return
			}
		case <-doneChan:
			return
		}
	}
}

// SeriesHandler returns the list of time series that match a certain label set.
// See https://prometheus.io/docs/prometheus/latest/querying/api/#finding-series-by-label-matchers
func (q *QuerierAPI) SeriesHandler(ctx context.Context, req *logproto.SeriesRequest) (*logproto.SeriesResponse, stats.Result, error) {
	timer := prometheus.NewTimer(logql.QueryTime.WithLabelValues("series"))
	defer timer.ObserveDuration()

	start := time.Now()
	statsCtx, ctx := stats.NewContext(ctx)

	resp, err := q.querier.Series(ctx, req)
	queueTime, _ := ctx.Value(httpreq.QueryQueueTimeHTTPHeader).(time.Duration)

	resLength := 0
	if resp != nil {
		resLength = len(resp.Series)
	}

	// record stats about the label query
	statResult := statsCtx.Result(time.Since(start), queueTime, resLength)
	log := spanlogger.FromContext(ctx)
	statResult.Log(level.Debug(log))

	status := 200
	if err != nil {
		status, _ = serverutil.ClientHTTPStatusAndError(err)
	}

	logql.RecordSeriesQueryMetrics(ctx, log, req.Start, req.End, req.Groups, strconv.Itoa(status), statResult)

	return resp, statResult, err
}

// IndexStatsHandler queries the index for the data statistics related to a query
func (q *QuerierAPI) IndexStatsHandler(ctx context.Context, req *loghttp.RangeQuery) (*logproto.IndexStatsResponse, error) {
	// TODO(karsten): we might want to change IndexStats to receive a logproto.IndexStatsRequest instead
	// TODO(owen-d): log metadata, record stats?
	resp, err := q.querier.IndexStats(ctx, req)
	if resp == nil {
		// Some stores don't implement this
		resp = &index_stats.Stats{}
	}

	return resp, err
}

//TODO(trevorwhitney): add test for the handler split

// VolumeHandler queries the index label volumes related to the passed matchers and given time range.
// Returns either N values where N is the time range / step and a single value for a time range depending on the request.
func (q *QuerierAPI) VolumeHandler(ctx context.Context, req *logproto.VolumeRequest) (*logproto.VolumeResponse, error) {
	resp, err := q.querier.Volume(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil { // Some stores don't implement this
		return &logproto.VolumeResponse{Volumes: []logproto.Volume{}}, nil
	}

	return resp, nil
}

// parseRegexQuery parses regex and query querystring from httpRequest and returns the combined LogQL query.
// This is used only to keep regexp query string support until it gets fully deprecated.
func parseRegexQuery(httpRequest *http.Request) (string, error) {
	query := httpRequest.Form.Get("query")
	regexp := httpRequest.Form.Get("regexp")
	if regexp != "" {
		expr, err := syntax.ParseLogSelector(query, true)
		if err != nil {
			return "", err
		}
		newExpr, err := syntax.AddFilterExpr(expr, labels.MatchRegexp, "", regexp)
		if err != nil {
			return "", err
		}
		query = newExpr.String()
	}
	return query, nil
}

func (q *QuerierAPI) validateMaxEntriesLimits(ctx context.Context, query string, limit uint32) error {
	tenantIDs, err := tenant.TenantIDs(ctx)
	if err != nil {
		return httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	expr, err := syntax.ParseExpr(query)
	if err != nil {
		return err
	}

	// entry limit does not apply to metric queries.
	if _, ok := expr.(syntax.SampleExpr); ok {
		return nil
	}

	maxEntriesCapture := func(id string) int { return q.limits.MaxEntriesLimitPerQuery(ctx, id) }
	maxEntriesLimit := util_validation.SmallestPositiveNonZeroIntPerTenant(tenantIDs, maxEntriesCapture)
	if int(limit) > maxEntriesLimit && maxEntriesLimit != 0 {
		return httpgrpc.Errorf(http.StatusBadRequest,
			"max entries limit per query exceeded, limit > max_entries_limit (%d > %d)", limit, maxEntriesLimit)
	}
	return nil
}

// WrapQuerySpanAndTimeout applies a context deadline and a span logger to a query call.
//
// The timeout is based on the per-tenant query timeout configuration.
func WrapQuerySpanAndTimeout(call string, limits Limits) middleware.Interface {
	return middleware.Func(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			sp, ctx := opentracing.StartSpanFromContext(req.Context(), call)
			defer sp.Finish()
			log := spanlogger.FromContext(req.Context())
			defer log.Finish()

			tenants, err := tenant.TenantIDs(ctx)
			if err != nil {
				level.Error(log).Log("msg", "couldn't fetch tenantID", "err", err)
				serverutil.WriteError(httpgrpc.Errorf(http.StatusBadRequest, err.Error()), w)
				return
			}

			timeoutCapture := func(id string) time.Duration { return limits.QueryTimeout(ctx, id) }
			timeout := util_validation.SmallestPositiveNonZeroDurationPerTenant(tenants, timeoutCapture)
			newCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			newReq := req.WithContext(newCtx)
			next.ServeHTTP(w, newReq)
		})
	})
}
