package features

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vamos-build/vamos/internal/testenv"
)

// Steps about the metrics that a coordinator exports: Prometheus scrapes of its own port, plain and OpenMetrics, what
// they hold, and the workers' lines to the coordinator, which a step cuts as a worker that dies would.

// metricsScrapes is where the scenario's coordinator serves Prometheus, and what steps scraped there.
type metricsScrapes struct {
	addr    string   // host:port
	last    []sample // the last scrape's
	before  []sample // the scrape before it
	lastRaw string
}

// sample is one sample of a scrape: a series' name and labels, its value, and its exemplar's labels, if any.
type sample struct {
	name     string
	labels   map[string]string
	value    float64
	exemplar map[string]string
}

// freePort returns a port of 127.0.0.1 that nothing listens on now.
func (w *world) freePort() int {
	w.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		w.t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// GivenTheCoordinatorServesPrometheusMetrics has the scenario's coordinator, which a later step starts, serve
// Prometheus metrics, alongside any exporter set before, on a free port of 127.0.0.1 of its own.
func (w *world) GivenTheCoordinatorServesPrometheusMetrics() {
	w.t.Helper()
	exporters := "prometheus"
	if set := w.telemetryGetenv("OTEL_METRICS_EXPORTER"); set != "" {
		exporters = set + ",prometheus"
	}
	port := w.freePort()
	w.GivenTheCoordinatorEnvironmentSets("OTEL_METRICS_EXPORTER", exporters)
	w.GivenTheCoordinatorEnvironmentSets("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1")
	w.GivenTheCoordinatorEnvironmentSets("OTEL_EXPORTER_PROMETHEUS_PORT", strconv.Itoa(port))
	w.metrics = &metricsScrapes{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
}

// GivenTheCoordinatorExportsMetricsEvery has the scenario's coordinator export its OTLP metrics every interval, rather
// than every minute.
func (w *world) GivenTheCoordinatorExportsMetricsEvery(interval time.Duration) {
	w.t.Helper()
	w.GivenTheCoordinatorEnvironmentSets("OTEL_METRIC_EXPORT_INTERVAL", strconv.FormatInt(interval.Milliseconds(), 10))
}

// GivenAPortThatAnotherProcessListensOn listens on a port of 127.0.0.1 until the scenario ends, which
// OTEL_EXPORTER_PROMETHEUS_PORT then names, on 127.0.0.1.
func (w *world) GivenAPortThatAnotherProcessListensOn() {
	w.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { l.Close() })
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	w.GivenTheCoordinatorEnvironmentSets("OTEL_EXPORTER_PROMETHEUS_HOST", "127.0.0.1")
	w.GivenTheCoordinatorEnvironmentSets("OTEL_EXPORTER_PROMETHEUS_PORT", port)
}

// GivenNothingListensOnThePrometheusDefaultPort skips the scenario where something, such as an exporter of the
// machine's own, listens on localhost:9464, Prometheus's default, of which it couldn't tell the coordinator's.
func (w *world) GivenNothingListensOnThePrometheusDefaultPort() {
	w.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:9464")
	if err != nil {
		testenv.Unavailable(w.t, "an unused localhost:9464", err)
		return
	}
	l.Close()
}

// WhenIScrapeTheMetrics scrapes the coordinator's /metrics in Prometheus's text format, keeping the scrape before.
func (w *world) WhenIScrapeTheMetrics() {
	w.t.Helper()
	w.scrape("text/plain; version=0.0.4")
}

// WhenIScrapeTheMetricsOnceItShows scrapes the metrics, as WhenIScrapeTheMetrics does, until the samples named name
// whose labels include those labels gives add up to want, as a gauge does once the coordinator's state has caught up,
// for receiveDeadline at most.
func (w *world) WhenIScrapeTheMetricsOnceItShows(name string, want float64, labels ...string) {
	w.t.Helper()
	for deadline := time.Now().Add(receiveDeadline); ; time.Sleep(50 * time.Millisecond) {
		w.WhenIScrapeTheMetrics()
		if got, _ := total(w.metrics.last, name, labelsOf(labels...)); got == want || time.Now().After(deadline) {
			break
		}
	}
	w.ThenTheMetricIs(name, want, labels...)
}

// GivenIScrapedTheMetrics scrapes them as WhenIScrapeTheMetrics does, as what a later scrape is compared with.
func (w *world) GivenIScrapedTheMetrics() {
	w.t.Helper()
	w.WhenIScrapeTheMetrics()
}

// WhenIScrapeTheMetricsAsOpenMetrics scrapes them as OpenMetrics, which carries exemplars.
func (w *world) WhenIScrapeTheMetricsAsOpenMetrics() {
	w.t.Helper()
	w.scrape("application/openmetrics-text; version=1.0.0")
}

// scrape scrapes the coordinator's /metrics, accepting accept.
func (w *world) scrape(accept string) {
	w.t.Helper()
	if w.metrics == nil {
		w.t.Fatal("no GivenTheCoordinatorServesPrometheusMetrics step had the coordinator serve metrics")
	}
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodGet, "http://"+w.metrics.addr+"/metrics", nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("scraping the coordinator's metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		w.t.Fatalf("scraping the coordinator's metrics answered %s, %v:\n%s", resp.Status, err, body)
	}
	w.metrics.before = w.metrics.last
	w.metrics.last, w.metrics.lastRaw = parseSamples(w.t, string(body)), string(body)
}

// parseSamples parses a scrape, in Prometheus's text format or OpenMetrics, as far as these steps read it: each
// sample's name, labels and value, and its exemplar's labels.
func parseSamples(t interface{ Fatalf(string, ...any) }, text string) []sample {
	var samples []sample
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := sample{labels: map[string]string{}}
		rest := line
		if i := strings.IndexAny(rest, "{ "); i >= 0 {
			s.name, rest = rest[:i], rest[i:]
		}
		if strings.HasPrefix(rest, "{") {
			var err error
			s.labels, rest, err = parseLabels(rest)
			if err != nil {
				t.Fatalf("the scrape's line %q: %v", line, err)
			}
		}
		value, exemplar, _ := strings.Cut(strings.TrimSpace(rest), " # ")
		fields := strings.Fields(value)
		if len(fields) == 0 {
			t.Fatalf("the scrape's line %q has no value", line)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			t.Fatalf("the scrape's line %q: %v", line, err)
		}
		s.value = v
		if exemplar != "" {
			if s.exemplar, _, err = parseLabels(exemplar); err != nil {
				t.Fatalf("the scrape's line %q's exemplar: %v", line, err)
			}
		}
		samples = append(samples, s)
	}
	return samples
}

// parseLabels parses the labels that text starts with, {name="value",...}, and returns what follows them.
func parseLabels(text string) (map[string]string, string, error) {
	labels := map[string]string{}
	rest := strings.TrimPrefix(text, "{")
	for {
		rest = strings.TrimLeft(rest, ", ")
		if strings.HasPrefix(rest, "}") {
			return labels, rest[1:], nil
		}
		name, after, ok := strings.Cut(rest, "=")
		if !ok || !strings.HasPrefix(after, `"`) {
			return nil, "", fmt.Errorf("malformed labels %q", text)
		}
		var value strings.Builder
		i := 1
		for ; i < len(after) && after[i] != '"'; i++ {
			if after[i] == '\\' && i+1 < len(after) {
				i++
				switch after[i] {
				case 'n':
					value.WriteByte('\n')
				default:
					value.WriteByte(after[i])
				}
				continue
			}
			value.WriteByte(after[i])
		}
		if i == len(after) {
			return nil, "", fmt.Errorf("unterminated label value in %q", text)
		}
		labels[strings.TrimSpace(name)] = value.String()
		rest = after[i+1:]
	}
}

// labelsOf returns the labels that pairs, names and values in turn, give.
func labelsOf(pairs ...string) map[string]string {
	labels := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		labels[pairs[i]] = pairs[i+1]
	}
	return labels
}

// total returns the sum of the samples named name, among samples, whose labels include labels, and whether any is.
func total(samples []sample, name string, labels map[string]string) (float64, bool) {
	sum, found := 0.0, false
	for _, s := range samples {
		if s.name != name {
			continue
		}
		matches := true
		for k, v := range labels {
			matches = matches && s.labels[k] == v
		}
		if matches {
			sum, found = sum+s.value, true
		}
	}
	return sum, found
}

// ThenTheMetricIs checks that the samples of the last scrape named name, whose labels include those that labels give,
// names and values in turn, add up to want.
func (w *world) ThenTheMetricIs(name string, want float64, labels ...string) {
	w.t.Helper()
	if got, found := total(w.metrics.last, name, labelsOf(labels...)); !found || got != want {
		w.t.Errorf("%s%v = %v (found: %t); want %v; the scrape:\n%s", name, labels, got, found, want,
			w.scrapedOf(name))
	}
}

// ThenTheMetricIsAtLeast checks the same as ThenTheMetricIs, but that they add up to least or more.
func (w *world) ThenTheMetricIsAtLeast(name string, least float64, labels ...string) {
	w.t.Helper()
	if got, found := total(w.metrics.last, name, labelsOf(labels...)); !found || got < least {
		w.t.Errorf("%s%v = %v (found: %t); want at least %v; the scrape:\n%s", name, labels, got, found, least,
			w.scrapedOf(name))
	}
}

// ThenTheMetricIsLessThanBefore checks that name, at labels, in the last scrape is less than in the one before.
func (w *world) ThenTheMetricIsLessThanBefore(name string, labels ...string) {
	w.t.Helper()
	before, _ := total(w.metrics.before, name, labelsOf(labels...))
	if got, found := total(w.metrics.last, name, labelsOf(labels...)); !found || got >= before {
		w.t.Errorf("%s%v = %v (found: %t); want less than the %v of the scrape before", name, labels, got, found,
			before)
	}
}

// scrapedOf returns the lines of the last scrape that name name, for a failure to show.
func (w *world) scrapedOf(name string) string {
	var lines []string
	for line := range strings.SplitSeq(w.metrics.lastRaw, "\n") {
		if strings.HasPrefix(line, name) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// ThenTheMetricsHaveTheLabel checks that some sample of the last scrape named name has the label key at value.
func (w *world) ThenTheMetricsHaveTheLabel(name, key, value string) {
	w.t.Helper()
	if _, found := total(w.metrics.last, name, labelsOf(key, value)); !found {
		w.t.Errorf("no %s has %s=%q; the scrape:\n%s", name, key, value, w.scrapedOf(name))
	}
}

// ThenNoSeriesNames checks that no sample of the last scrape names any of words, or a job the coordinator accepted,
// in its name or its labels.
func (w *world) ThenNoSeriesNames(words ...string) {
	w.t.Helper()
	w.jobsMu.Lock()
	words = append(words, w.jobIDs...)
	w.jobsMu.Unlock()
	for _, s := range w.metrics.last {
		texts := []string{s.name}
		for k, v := range s.labels {
			texts = append(texts, k, v)
		}
		for _, text := range texts {
			for _, word := range words {
				if strings.Contains(text, word) {
					w.t.Errorf("%s%v names %q", s.name, s.labels, word)
				}
			}
		}
	}
}

// ThenTheMetricHasAnExemplarOfTheExportedSpan checks that a sample named name of the last scrape, an OpenMetrics
// one, has an exemplar whose trace_id and span_id are those of a span named span that the collector received.
func (w *world) ThenTheMetricHasAnExemplarOfTheExportedSpan(name, span string) {
	w.t.Helper()
	received := map[string]bool{}
	for _, s := range w.collected() {
		if s.name == span {
			received[s.trace+"/"+s.id] = true
		}
	}
	for _, s := range w.metrics.last {
		if s.name == name && s.exemplar != nil && received[s.exemplar["trace_id"]+"/"+s.exemplar["span_id"]] {
			return
		}
	}
	w.t.Errorf("no %s has an exemplar of the %d spans named %q the collector received; the scrape:\n%s", name,
		len(received), span, w.scrapedOf(name))
}

// ThenTheMetricHasNoExemplar checks that no sample named name of the last scrape, an OpenMetrics one, has an exemplar.
func (w *world) ThenTheMetricHasNoExemplar(name string) {
	w.t.Helper()
	for _, s := range w.metrics.last {
		if s.name == name && s.exemplar != nil {
			w.t.Errorf("%s%v has the exemplar %v; want none", s.name, s.labels, s.exemplar)
		}
	}
}

// ThenNothingAnswersOnThePrometheusDefaultPort checks that nothing answers on localhost:9464.
func (w *world) ThenNothingAnswersOnThePrometheusDefaultPort() {
	w.t.Helper()
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:9464", time.Second); err == nil {
		conn.Close()
		w.t.Error("something answers on localhost:9464; want nothing")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		w.t.Errorf("connecting to localhost:9464 = %v; want it refused", err)
	}
}

// ThenTheAPIServesNoMetrics checks that the coordinator's API, at its --listen address, answers /metrics with a 404.
func (w *world) ThenTheAPIServesNoMetrics() {
	w.t.Helper()
	resp, err := http.Get(w.atCoordinator("<coordinator>") + "/metrics")
	if err != nil {
		w.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		w.t.Errorf("the API answered /metrics with %s; want %d", resp.Status, http.StatusNotFound)
	}
}

// ThenTheCollectorReceivedTheMetric checks that the collector has received, by now or within its export interval, a
// metric named name.
func (w *world) ThenTheCollectorReceivedTheMetric(name string) {
	w.t.Helper()
	deadline := time.Now().Add(receiveDeadline)
	for !w.collector.hasMetric(name) {
		if time.Now().After(deadline) {
			w.t.Fatalf("the collector received no %s", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ThenTheCollectorReceivedTheMetricAt checks that the collector has received, by now or within its export interval,
// the sum named name, at want.
func (w *world) ThenTheCollectorReceivedTheMetricAt(name string, want float64) {
	w.t.Helper()
	w.ThenTheCollectorReceivedTheMetric(name)
	if got, _ := w.collector.metric(name); got != want {
		w.t.Errorf("the collector received %s at %v; want %v", name, got, want)
	}
}

// WhenIStartAWorker runs `vamos worker`, with the workers' telemetry environment, for a coordinator that nothing
// serves, so that it either fails as it starts or is interrupted once it has tried for a while.
func (w *world) WhenIStartAWorker() {
	w.t.Helper()
	w.sendsNothing("WhenIStartAWorker")
	interruptedAfter(5*time.Second).apply(w, &commandLine{})
	coordinators := w.telemetryEnv
	w.telemetryEnv = w.workerTelemetryEnv // which the command reads, as the commands' Getenv is the coordinator's
	defer func() { w.telemetryEnv = coordinators }()
	w.run("worker", "--coordinator", "http://127.0.0.1:1")
}

// GivenTheWorkerExportsMetricsToTheCollector has the scenario's workers export their metrics to GivenAnOTLPCollector's
// collector, every interval.
func (w *world) GivenTheWorkerExportsMetricsToTheCollector(interval time.Duration) {
	w.t.Helper()
	if w.collector == nil {
		w.t.Fatal("no GivenAnOTLPCollector step started a collector")
	}
	w.GivenTheWorkerEnvironmentSets("OTEL_EXPORTER_OTLP_ENDPOINT", w.collector.url)
	w.GivenTheWorkerEnvironmentSets("OTEL_METRIC_EXPORT_INTERVAL", strconv.FormatInt(interval.Milliseconds(), 10))
}

// GivenTheBlobsOfTheCoordinatorsBuildCacheAreRemoved removes every blob that the coordinator's build cache holds, as
// a bad disk might, leaving the results that name them.
func (w *world) GivenTheBlobsOfTheCoordinatorsBuildCacheAreRemoved() {
	w.t.Helper()
	if w.coordinatorCache == "" {
		w.t.Fatal("no GivenACoordinatorWithAWorker step started a coordinator with a build cache of its own")
	}
	dir := filepath.Join(w.coordinatorCache, "cas", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			w.t.Fatal(err)
		}
	}
}

// WhenIInterruptTheCoordinator interrupts GivenACoordinatorWithAWorker's coordinator, and waits until it has ended.
func (w *world) WhenIInterruptTheCoordinator() {
	w.t.Helper()
	if err := w.stopCoordinator(); err != nil {
		w.t.Errorf("the coordinator ended with %v once interrupted; want it to end cleanly", err)
	}
}

// ThenTheCollectorAlreadyReceivedTheTraceOf checks that the collector holds, now, without waiting, a job span of kind.
func (w *world) ThenTheCollectorAlreadyReceivedTheTraceOf(kind string) {
	w.t.Helper()
	if len(jobSpans(w.collector.spans())[kind]) == 0 {
		w.t.Errorf("the collector had received no %s job span by the time the coordinator ended", kind)
	}
}

// workerLine is a worker's connection to the coordinator, a proxy that a step cuts, after which it answers nothing
// the worker sends, as a worker that has died sends nothing: no heartbeat, report or poll reaches the coordinator.
type workerLine struct {
	url string
	cut atomic.Bool
	// cuts ends the requests in flight as the line is cut, such as a poll for an assignment, which would otherwise
	// still hand the worker an attempt it then never heartbeats.
	cuts       context.Context
	cutsCancel context.CancelFunc
	runs       atomic.Bool // the coordinator has assigned the worker an attempt that runs a command
}

// newWorkerLine starts a line to the coordinator at address, until the scenario ends.
func (w *world) newWorkerLine(address string) *workerLine {
	w.t.Helper()
	backend, err := url.Parse(address)
	if err != nil {
		w.t.Fatal(err)
	}
	line := &workerLine{}
	line.cuts, line.cutsCancel = context.WithCancel(context.Background())
	proxy := &httputil.ReverseProxy{FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(backend)
	}}
	proxy.ModifyResponse = func(r *http.Response) error {
		// remote's pathAssignments, /v1/workers/{id}/assignments, whose answer holds the work's steps
		if !strings.HasSuffix(r.Request.URL.Path, "/assignments") {
			return nil
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte(`"op":"run"`)) {
			line.runs.Store(true)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if line.cut.Load() {
			http.Error(rw, "the line is cut", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(line.cuts, cancel)()
		proxy.ServeHTTP(rw, r.WithContext(ctx))
	}))
	w.t.Cleanup(server.Close)
	w.t.Cleanup(line.cutsCancel) // after server.Close's, so first, ending the polls it would otherwise wait for
	line.url = server.URL
	return line
}

// GivenTheWorkerRunningAStepIsStoppedOnceItReachesTheBarrier cuts the line of the worker that runs a command, once a
// step has reached GivenABarrier's barrier, and then opens the barrier: the attempt is lost once its lease expires,
// and tried again, on the other worker, which passes the barrier at once.
func (w *world) GivenTheWorkerRunningAStepIsStoppedOnceItReachesTheBarrier() {
	w.t.Helper()
	if len(w.workerLines) < 2 {
		w.t.Fatal("no GivenACoordinatorWithTwoWorkers step started workers whose lines a step cuts")
	}
	b := w.barrier
	w.openBarrierOnce("a step reached the barrier on a worker", func() bool {
		b.mu.Lock()
		reached := b.reached
		b.mu.Unlock()
		if reached == 0 {
			return false
		}
		for _, line := range w.workerLines {
			if line.runs.Load() {
				line.cut.Store(true)
				line.cutsCancel()
				return true
			}
		}
		return false
	})
}

// ThenTheReferenceListsTheScrapedMetrics checks that the first column of the table under the heading "Metrics" of the
// document at path lists exactly the metrics, named vamos_ or http_server_, that the last scrape declares, but for
// unscraped, which it lists though the scrape needn't.
func (w *world) ThenTheReferenceListsTheScrapedMetrics(path string, unscraped ...string) {
	w.t.Helper()
	doc, err := os.ReadFile(path)
	if err != nil {
		w.t.Fatal(err)
	}
	_, section, found := strings.Cut(string(doc), "\n## Metrics\n")
	if !found {
		w.t.Fatalf("%s has no Metrics section", path)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	listed := map[string]bool{}
	for line := range strings.SplitSeq(section, "\n") {
		if cells := strings.Split(line, "|"); len(cells) > 2 && strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			listed[strings.Trim(strings.TrimSpace(cells[1]), "`")] = true
		}
	}
	scraped := map[string]bool{}
	for line := range strings.SplitSeq(w.metrics.lastRaw, "\n") {
		if fields := strings.Fields(line); len(fields) == 4 && fields[1] == "TYPE" &&
			(strings.HasPrefix(fields[2], "vamos_") || strings.HasPrefix(fields[2], "http_server_")) {
			scraped[fields[2]] = true
		}
	}
	for name := range scraped {
		if !listed[name] {
			w.t.Errorf("the scrape shows %s, which %s doesn't list", name, path)
		}
	}
	for name := range listed {
		if !scraped[name] && !slices.Contains(unscraped, name) {
			w.t.Errorf("%s lists %s, which the scrape doesn't show", path, name)
		}
	}
}
