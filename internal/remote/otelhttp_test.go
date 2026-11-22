package remote

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vamos-build/vamos/internal/cache"
)

// TestOtelhttpLabelsByRoute checks that the pinned otelhttp, wrapped around the server's handler as its middleware is,
// labels http.server.request.duration with the ServeMux pattern that served a request,
// its path as http.route and its method as http.request.method, as the semantic conventions say,
// so that requests for different jobs and blobs make one series a route,
// and never with the request's path, which holds a job's ID or a blob's digest that would make a series of each.
func TestOtelhttpLabelsByRoute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ex := newFakeExecutor(2)
	ex.jobs["job-2"] = scriptedEvents(2)
	store := cache.NewMemory()
	middleware := func(h http.Handler) http.Handler {
		return otelhttp.NewHandler(h, "vamos", otelhttp.WithMeterProvider(provider))
	}
	h := (&Server{Executor: ex, Content: store, Middleware: middleware}).Handler()
	identifiers := []string{"job-1", "job-2"} // what no label may hold
	requests := []struct{ method, target string }{
		{method: http.MethodGet, target: "/v1/jobs/job-1/events?from=0"},
		{method: http.MethodGet, target: "/v1/jobs/job-2/events?from=0"},
		{method: http.MethodGet, target: pathCapabilities},
	}
	for _, data := range []string{"one", "two"} {
		b, err := store.PutBytes(t.Context(), []byte(data))
		if err != nil {
			t.Fatal(err)
		}
		identifiers = append(identifiers, b.Digest.Encoded())
		requests = append(requests, struct{ method, target string }{
			method: http.MethodHead,
			target: fmt.Sprintf("/v1/blobs/%s?size=%d", b.Digest, b.Size),
		})
	}
	for _, r := range requests {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(r.method, r.target, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s answered %d: %s", r.method, r.target, w.Code, w.Body)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{} // the requests counted in the series of each method and route
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("http.server.request.duration is a %T, want a histogram", m.Data)
			}
			for _, dp := range hist.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					for _, id := range identifiers {
						if strings.Contains(kv.Value.String(), id) {
							t.Errorf("attribute %s = %q holds %s", kv.Key, kv.Value.String(), id)
						}
					}
				}
				method, _ := dp.Attributes.Value(attribute.Key("http.request.method"))
				route, _ := dp.Attributes.Value(attribute.Key("http.route"))
				series := method.AsString() + " " + route.AsString()
				if _, ok := counts[series]; ok {
					t.Errorf("%s has more than one series; another is labeled %v", series, dp.Attributes.ToSlice())
				}
				counts[series] = dp.Count
			}
		}
	}
	want := map[string]uint64{
		"GET " + pathEvents:       2,
		"GET " + pathCapabilities: 1,
		"HEAD " + pathBlob:        2,
	}
	if !maps.Equal(counts, want) {
		t.Errorf("requests by method and route = %v, want %v", counts, want)
	}
}
