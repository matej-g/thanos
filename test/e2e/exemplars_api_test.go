// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/efficientgo/e2e"
	"github.com/prometheus/prometheus/pkg/timestamp"
	"github.com/thanos-io/thanos/pkg/exemplars/exemplarspb"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/testutil"
	"github.com/thanos-io/thanos/test/e2e/e2ethanos"
)

const (
	traceIDLabel = "traceID"
)

func TestExemplarsAPI_Fanout(t *testing.T) {
	t.Parallel()

	netName := "e2e_test_exemplars_fanout"

	var (
		prom1, prom2       *e2e.InstrumentedRunnable
		sidecar1, sidecar2 *e2e.InstrumentedRunnable
		err                error
		e                  *e2e.DockerEnvironment
	)

	e, err = e2e.NewDockerEnvironment(netName)
	testutil.Ok(t, err)
	t.Cleanup(e2ethanos.CleanScenario(t, e))

	tracingCfg := fmt.Sprintf(`type: JAEGER
config:
  sampler_type: const
  sampler_param: 1
  service_name: %s`, netName+"-query")

	stores := []string{netName + "-" + "sidecar-prom1:9091", netName + "-" + "sidecar-prom2:9091"}
	q, err := e2ethanos.NewQuerierBuilder(
		e, "query", stores).
		WithExemplarAddresses(stores).
		WithTracingConfig(tracingCfg).
		Build()
	testutil.Ok(t, err)
	testutil.Ok(t, e2e.StartAndWaitReady(q))

	// Recreate Prometheus and sidecar with Thanos query scrape target.
	prom1, sidecar1, err = e2ethanos.NewPrometheusWithSidecar(
		e,
		netName,
		"prom1",
		defaultPromConfig("ha", 0, "", "", "localhost:9090", q.InternalEndpoint("http")),
		e2ethanos.DefaultPrometheusImage(),
		e2ethanos.FeatureExemplarStorage,
	)
	testutil.Ok(t, err)
	prom2, sidecar2, err = e2ethanos.NewPrometheusWithSidecar(
		e,
		netName,
		"prom2",
		defaultPromConfig("ha", 1, "", "", "localhost:9090", q.InternalEndpoint("http")),
		e2ethanos.DefaultPrometheusImage(),
		e2ethanos.FeatureExemplarStorage,
	)
	testutil.Ok(t, err)
	testutil.Ok(t, e2e.StartAndWaitReady(prom1, sidecar1, prom2, sidecar2))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	t.Cleanup(cancel)

	testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(2), []string{"thanos_store_nodes_grpc_connections"}, e2e.WaitMissingMetrics()))
	testutil.Ok(t, q.WaitSumMetricsWithOptions(e2e.Equals(2), []string{"thanos_query_exemplar_apis_dns_provider_results"}, e2e.WaitMissingMetrics()))

	now := time.Now()
	start := timestamp.FromTime(now.Add(-time.Hour))
	end := timestamp.FromTime(now.Add(time.Hour))

	// Send HTTP requests to thanos query to trigger exemplars.
	labelNames(t, ctx, q.Endpoint("http"), nil, start, end, func(res []string) bool {
		return true
	})

	t.Run("Basic exemplars query", func(t *testing.T) {
		requiredSeriesLabels := map[string]string{
			"__name__":   "http_request_duration_seconds_bucket",
			"handler":    "label_names",
			"job":        "myself",
			"method":     "get",
			"prometheus": "ha",
		}
		queryExemplars(t, ctx, q.Endpoint("http"), `http_request_duration_seconds_bucket{handler="label_names"}`,
			start, end, func(data []*exemplarspb.ExemplarData) bool {
				if len(data) != 1 {
					return false
				}

				// Compare series labels.
				seriesLabels := labelpb.ZLabelSetsToPromLabelSets(data[0].SeriesLabels)
				for _, lbls := range seriesLabels {
					for k, v := range requiredSeriesLabels {
						if lbls.Get(k) != v {
							return false
						}
					}
				}

				// Make sure the exemplar contains the correct traceID label.
				for _, exemplar := range data[0].Exemplars {
					for _, lbls := range labelpb.ZLabelSetsToPromLabelSets(exemplar.Labels) {
						if !lbls.Has(traceIDLabel) {
							return false
						}
					}
				}
				return true
			})
	})

	t.Run("Exemplars query with matched external label", func(t *testing.T) {
		requiredSeriesLabels := map[string]string{
			"__name__":   "http_request_duration_seconds_bucket",
			"handler":    "label_names",
			"job":        "myself",
			"method":     "get",
			"prometheus": "ha",
		}
		// Here replica is an external label.
		queryExemplars(t, ctx, q.Endpoint("http"), `http_request_duration_seconds_bucket{handler="label_names", replica="0"}`,
			start, end, func(data []*exemplarspb.ExemplarData) bool {
				if len(data) != 1 {
					return false
				}

				// Compare series labels.
				seriesLabels := labelpb.ZLabelSetsToPromLabelSets(data[0].SeriesLabels)
				for _, lbls := range seriesLabels {
					for k, v := range requiredSeriesLabels {
						if lbls.Get(k) != v {
							return false
						}
					}
				}

				// Make sure the exemplar contains the correct traceID label.
				for _, exemplar := range data[0].Exemplars {
					for _, lbls := range labelpb.ZLabelSetsToPromLabelSets(exemplar.Labels) {
						if !lbls.Has(traceIDLabel) {
							return false
						}
					}
				}
				return true
			})
	})

	t.Run("Exemplars query doesn't match external label", func(t *testing.T) {
		// Here replica is an external label, but it doesn't match.
		queryExemplars(t, ctx, q.Endpoint("http"), `http_request_duration_seconds_bucket{handler="label_names", replica="foo"}`,
			start, end, func(data []*exemplarspb.ExemplarData) bool {
				return len(data) == 0
			})
	})
}
