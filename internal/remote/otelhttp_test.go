package remote

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/telemetry"
)

// authenticated is the key of what a middleware adds to a request's context.
type authenticated struct{}

// TestInstrumentLabelsByRoute checks that the pinned otelhttp, wrapped around the server's routes through Instrument,
// inside a middleware that passes on a copy of each request, as r.WithContext makes, and viewed through
// telemetry.HTTPView, labels http.server.request.duration with the ServeMux pattern that served a request,
// its path as http.route and its method as http.request.method, as the semantic conventions say, and its status,
// so that requests for different jobs and blobs make one series a route and status,
// and never with the request's path, which holds a job's ID or a blob's digest that would make a series of each,
// nor with the Host or port a client chose, on any of otelhttp's instruments; and that the bytes of blobs put and
// read are told to Transferred.
func TestInstrumentLabelsByRoute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(telemetry.HTTPView()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ex := newFakeExecutor(2)
	ex.jobs["job-2"] = scriptedEvents(2)
	store := cache.NewMemory()
	copying := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authenticated{}, true)))
		})
	}
	var mu sync.Mutex
	transferred := map[string]int64{}
	h := (&Server{Executor: ex, Content: store, Middleware: copying, Instrument: telemetry.Instrument(provider),
		Transferred: func(direction string, n int64) {
			mu.Lock()
			defer mu.Unlock()
			transferred[direction] += n
		}}).Handler()
	identifiers := []string{"job-1", "job-2", "job-3", "other.example", "8443"} // what no label may hold
	requests := []struct {
		method, target string
		body           string
		code           int
	}{
		{method: http.MethodGet, target: "/v1/jobs/job-1/events?from=0", code: http.StatusOK},
		{method: http.MethodGet, target: "http://other.example:8443/v1/jobs/job-2/events?from=0", code: http.StatusOK},
		{method: http.MethodGet, target: "/v1/jobs/job-3/events?from=0", code: http.StatusNotFound},
		{method: http.MethodGet, target: pathCapabilities, code: http.StatusOK},
		{method: http.MethodPost, target: pathBlobs + "?limit=100", body: "put", code: http.StatusCreated},
	}
	var blobs []cache.Blob
	for _, data := range []string{"one", "two"} {
		b, err := store.PutBytes(t.Context(), []byte(data))
		if err != nil {
			t.Fatal(err)
		}
		blobs = append(blobs, b)
		identifiers = append(identifiers, b.Digest.Encoded())
		requests = append(requests, struct {
			method, target string
			body           string
			code           int
		}{method: http.MethodHead, target: fmt.Sprintf("/v1/blobs/%s?size=%d", b.Digest, b.Size), code: http.StatusOK})
	}
	requests = append(requests, struct {
		method, target string
		body           string
		code           int
	}{method: http.MethodGet, target: fmt.Sprintf("/v1/blobs/%s?size=%d", blobs[0].Digest, blobs[0].Size),
		code: http.StatusOK})
	for _, r := range requests {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(r.method, r.target, strings.NewReader(r.body)))
		if w.Code != r.code {
			t.Fatalf("%s %s answered %d: %s; want %d", r.method, r.target, w.Code, w.Body, r.code)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{} // the requests counted in the series of each method, route and status
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, attrs := range attributesOf(m.Data) {
				for _, kv := range attrs.ToSlice() {
					for _, id := range identifiers {
						if strings.Contains(kv.Value.String(), id) {
							t.Errorf("%s's attribute %s = %q holds %s", m.Name, kv.Key, kv.Value.String(), id)
						}
					}
				}
			}
			if m.Name != "http.server.request.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("http.server.request.duration is a %T, want a histogram", m.Data)
			}
			for _, dp := range hist.DataPoints {
				method, _ := dp.Attributes.Value(attribute.Key("http.request.method"))
				route, _ := dp.Attributes.Value(attribute.Key("http.route"))
				code, _ := dp.Attributes.Value(attribute.Key("http.response.status_code"))
				series := fmt.Sprint(method.AsString(), " ", route.AsString(), " ", code.AsInt64())
				if _, ok := counts[series]; ok {
					t.Errorf("%s has more than one series; another is labeled %v", series, dp.Attributes.ToSlice())
				}
				counts[series] = dp.Count
			}
		}
	}
	want := map[string]uint64{
		"GET " + pathEvents + " 200":       2,
		"GET " + pathEvents + " 404":       1,
		"GET " + pathCapabilities + " 200": 1,
		"POST " + pathBlobs + " 201":       1,
		"HEAD " + pathBlob + " 200":        2,
		"GET " + pathBlob + " 200":         1,
	}
	if !maps.Equal(counts, want) {
		t.Errorf("requests by method, route and status = %v, want %v", counts, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := map[string]int64{"in": 3, "out": blobs[0].Size}; !maps.Equal(transferred, want) {
		t.Errorf("the bytes transferred = %v; want %v", transferred, want)
	}
}

// attributesOf returns the attribute sets of every point of data.
func attributesOf(data metricdata.Aggregation) []attribute.Set {
	var sets []attribute.Set
	switch d := data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	}
	return sets
}
