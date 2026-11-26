package telemetry

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Role is what a process exports telemetry as.
type Role string

// The roles that export telemetry.
const (
	// Coordinator exports its builds' traces, and its metrics over OTLP, Prometheus or both.
	Coordinator Role = "coordinator"
	// Worker exports its metrics over OTLP alone; its coordinator exports its spans.
	Worker Role = "worker"
)

// The exporters' names, as OTEL_TRACES_EXPORTER and OTEL_METRICS_EXPORTER take them.
const (
	exporterOTLP       = "otlp"
	exporterPrometheus = "prometheus"
	exporterConsole    = "console"
	exporterNone       = "none"
)

// exportersTaken lists the exporters each role takes for each signal, as its error names them.
var exportersTaken = map[Role]map[string][]string{
	Coordinator: {
		"OTEL_TRACES_EXPORTER":  {exporterOTLP, exporterConsole, exporterNone},
		"OTEL_METRICS_EXPORTER": {exporterOTLP, exporterPrometheus, exporterConsole, exporterNone},
	},
	Worker: {
		"OTEL_METRICS_EXPORTER": {exporterOTLP, exporterConsole, exporterNone},
	},
}

// settings is what Setup takes from the environment: the variables that the specifications name and that the
// features vary, read through Config.Getenv, so that a test can set them for one coordinator of many in a process.
// The OTLP exporters read the rest, such as their TLS certificates and compression, from the process's environment
// themselves, which is the same environment in a coordinator or worker that a test doesn't run.
type settings struct {
	traces, metrics []string // the exporters, none for "none"
	sampler         sampler
	serviceName     string
	resource        map[string]string // OTEL_RESOURCE_ATTRIBUTES
	otlp            map[string]otlpSettings
	batch           batchSettings
	metricInterval  time.Duration // zero for the SDK's default
	metricTimeout   time.Duration
	prometheusAddr  string
}

// otlpSettings is how one signal is sent over OTLP; empty fields leave the SDK's defaults.
type otlpSettings struct {
	endpoint string // a URL, with the signal's path
	protocol string // "http/protobuf" or "grpc"
	headers  map[string]string
	timeout  time.Duration
}

// batchSettings are the batch span processor's OTEL_BSP_* settings, or their defaults where they are unset.
type batchSettings struct {
	scheduleDelay, exportTimeout time.Duration
	maxQueueSize, maxExportBatch int
}

// The signals, as the OTLP variables' names and the exporters' paths spell them.
const (
	signalTraces  = "TRACES"
	signalMetrics = "METRICS"
)

// readSettings reads role's settings through getenv, and returns the first mistake it finds,
// naming the variable, its value and what it takes, and the warnings for what it ignores.
func readSettings(role Role, getenv func(string) string) (settings, []string, error) {
	var s settings
	var warnings []string
	disabled, _, err := readBool("OTEL_SDK_DISABLED", getenv)
	if err != nil {
		return s, nil, err
	}

	traces, err := readExporters(role, "OTEL_TRACES_EXPORTER", signalTraces, getenv)
	if err != nil {
		return s, nil, err
	}
	if role == Worker && !disabled {
		// Read as a coordinator's, so that only a value that would export something, or that is a mistake, warns.
		if v := getenv("OTEL_TRACES_EXPORTER"); strings.TrimSpace(v) != "" {
			if named, err := readExporters(Coordinator, "OTEL_TRACES_EXPORTER", signalTraces, getenv); err != nil ||
				len(named) > 0 {
				warnings = append(warnings, fmt.Sprintf("OTEL_TRACES_EXPORTER=%s is ignored: "+
					"a worker exports no traces, since its coordinator exports its spans", v))
			}
		}
		traces = nil
	}
	metrics, err := readExporters(role, "OTEL_METRICS_EXPORTER", signalMetrics, getenv)
	if err != nil {
		return s, nil, err
	}
	if !disabled {
		s.traces, s.metrics = traces, metrics
	}

	// A coordinator's sampler is checked even if it exports no trace, as its spec asks,
	// unlike the batch span processor's settings below, which only the export of traces has a use for.
	if role == Coordinator {
		if s.sampler, err = readSampler(getenv); err != nil {
			return s, nil, err
		}
	}
	s.serviceName = strings.TrimSpace(getenv("OTEL_SERVICE_NAME"))
	if !utf8.ValidString(s.serviceName) { // which OTLP's encoder would refuse every request for
		return s, nil, fmt.Errorf("OTEL_SERVICE_NAME=%q: not a name; it takes UTF-8", s.serviceName)
	}
	if s.resource, err = readPairs("OTEL_RESOURCE_ATTRIBUTES", false, getenv); err != nil {
		return s, nil, err
	}
	s.otlp = map[string]otlpSettings{}
	for _, sig := range []struct {
		name      string
		exporters []string
	}{{name: signalTraces, exporters: s.traces}, {name: signalMetrics, exporters: s.metrics}} {
		if !slices.Contains(sig.exporters, exporterOTLP) {
			continue // so that a role isn't refused for settings of what it doesn't send
		}
		if s.otlp[sig.name], err = readOTLP(sig.name, getenv); err != nil {
			return s, nil, err
		}
	}
	// Each signal's settings are read only if it is exported, so that a role isn't refused, as a worker might be by a
	// fleet's OTEL_BSP_*, for settings of what it doesn't send.
	// Metrics are pushed, at an interval, by every exporter but Prometheus, which is scraped.
	pushed := slices.ContainsFunc(s.metrics, func(e string) bool { return e != exporterPrometheus })
	for _, d := range []struct {
		name  string
		used  bool
		unset time.Duration
		to    *time.Duration
	}{
		{name: "OTEL_BSP_SCHEDULE_DELAY", used: len(s.traces) > 0, unset: 5 * time.Second, to: &s.batch.scheduleDelay},
		{name: "OTEL_BSP_EXPORT_TIMEOUT", used: len(s.traces) > 0, unset: 30 * time.Second, to: &s.batch.exportTimeout},
		{name: "OTEL_METRIC_EXPORT_INTERVAL", used: pushed, unset: time.Minute, to: &s.metricInterval},
		{name: "OTEL_METRIC_EXPORT_TIMEOUT", used: pushed, unset: 30 * time.Second, to: &s.metricTimeout},
	} {
		if !d.used {
			continue
		}
		if *d.to, err = readMillis(d.name, d.unset, getenv); err != nil {
			return s, nil, err
		}
	}
	for _, n := range []struct {
		name  string
		unset int
		to    *int
	}{
		{name: "OTEL_BSP_MAX_QUEUE_SIZE", unset: defaultMaxQueueSize, to: &s.batch.maxQueueSize},
		{name: "OTEL_BSP_MAX_EXPORT_BATCH_SIZE", unset: defaultMaxExportBatch, to: &s.batch.maxExportBatch},
	} {
		if len(s.traces) == 0 {
			break
		}
		if *n.to, err = readCount(n.name, n.unset, getenv); err != nil {
			return s, nil, err
		}
	}
	// A batch no larger than the queue, which the SDK ensures only of the variables it reads itself, not its options.
	s.batch.maxExportBatch = min(s.batch.maxExportBatch, s.batch.maxQueueSize)

	if !slices.Contains(s.metrics, exporterPrometheus) {
		return s, warnings, nil
	}
	host := strings.TrimSpace(getenv("OTEL_EXPORTER_PROMETHEUS_HOST"))
	port := strings.TrimSpace(getenv("OTEL_EXPORTER_PROMETHEUS_PORT"))
	if host == "" {
		host = "localhost"
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") { // an IPv6 address
		if inner := host[1 : len(host)-1]; inner == "" || strings.ContainsAny(inner, "[]") { // JoinHostPort brackets
			return s, nil, fmt.Errorf("OTEL_EXPORTER_PROMETHEUS_HOST=%s: not a host; it takes a name or an address",
				host)
		}
		host = host[1 : len(host)-1] // as JoinHostPort brackets it itself
	} else if strings.ContainsAny(host, "[]") {
		return s, nil, fmt.Errorf("OTEL_EXPORTER_PROMETHEUS_HOST=%s: not a host; it takes a name or an address", host)
	}
	if port == "" {
		port = "9464"
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return s, nil, fmt.Errorf("OTEL_EXPORTER_PROMETHEUS_PORT=%s: not a port; it takes a number from 0 to 65535",
			port)
	}
	if ip := net.ParseIP(host); ip != nil && ip.Equal(net.IPv6unspecified) {
		// Every address, as a listener with no host takes, which Go makes IPv6's and IPv4's together, or IPv4's alone
		// where the host has no IPv6, where a socket of the IPv6 wildcard itself couldn't be made.
		host = ""
	}
	s.prometheusAddr = net.JoinHostPort(host, port)
	return s, warnings, nil
}

// readExporters returns the exporters that the variable name lists for the signal, none for "none",
// checked against those role takes, or nil if role exports none of the signal.
// Unset, it is otlp if an OTLP endpoint is set for the signal, and none otherwise,
// rather than the specification's otlp to localhost, which would send to a collector no one set up.
func readExporters(role Role, name, signal string, getenv func(string) string) ([]string, error) {
	taken, ok := exportersTaken[role][name]
	if !ok {
		return nil, nil
	}
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		if strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" ||
			strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT")) != "" {
			return []string{exporterOTLP}, nil
		}
		return nil, nil
	}
	var exporters []string
	for e := range strings.SplitSeq(value, ",") {
		e = strings.ToLower(strings.TrimSpace(e)) // as the specification has enumerations read
		switch {
		case role == Worker && e == exporterPrometheus:
			return nil, fmt.Errorf("%s=%s: a worker can't serve Prometheus; workers export metrics over OTLP, "+
				"so it takes %s", name, value, orList(taken))
		case !slices.Contains(taken, e):
			return nil, fmt.Errorf("%s=%s: %q isn't an exporter; it takes %s", name, value, e, orList(taken))
		case e == exporterNone:
			continue
		case !slices.Contains(exporters, e):
			exporters = append(exporters, e)
		}
	}
	if role == Worker && len(exporters) > 1 {
		return nil, fmt.Errorf("%s=%s: a worker takes one exporter, %s", name, value, orList(taken))
	}
	return exporters, nil
}

// readOTLP returns how signal is sent over OTLP: the signal's own variables, or else the general ones.
func readOTLP(signal string, getenv func(string) string) (otlpSettings, error) {
	var o otlpSettings
	own := func(suffix string) (string, string) {
		name := "OTEL_EXPORTER_OTLP_" + signal + "_" + suffix
		if v := getenv(name); strings.TrimSpace(v) != "" { // as the exporters take one of spaces as unset
			return name, v
		}
		name = "OTEL_EXPORTER_OTLP_" + suffix
		return name, getenv(name)
	}
	name, protocol := own("PROTOCOL")
	switch p := strings.ToLower(strings.TrimSpace(protocol)); p {
	case "", "http/protobuf", "grpc":
		o.protocol = p
	default:
		return o, fmt.Errorf("%s=%s: not a protocol vamos sends; it takes http/protobuf or grpc", name, protocol)
	}
	// Both the signal's own endpoint and the general one are checked, though one wins, since the exporter reads both
	// itself, and would log a mistake in either, credentials and all.
	// Neither may have a query, which the exporters don't send.
	var endpointName string // the variable o.endpoint comes from
	grpcPath := false       // whether it has a path, which gRPC would drop
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT"} {
		endpoint := strings.TrimSpace(getenv(name)) // as the exporters trim it, taking one of spaces as unset
		if endpoint == "" {
			continue
		}
		// Nothing the exporters would drop or read otherwise: they send no user, password or query, and decode an
		// escaped path, so that one would reach another route.
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !isPort(u.Port()) {
			// Without its value, whose user, password or query may be a credential.
			return o, fmt.Errorf("%s: not an endpoint; it takes an http or https URL without credentials, a query or "+
				"an escaped path, such as http://collector:4318 (its value isn't shown, since it may hold a secret)",
				name)
		}
		endpointName = name
		if name == "OTEL_EXPORTER_OTLP_ENDPOINT" && o.protocol != "grpc" {
			u = u.JoinPath("v1", strings.ToLower(signal)) // the general one, to which HTTP appends the signal's path
		}
		if o.protocol != "grpc" && strings.TrimSpace(u.Path) != u.Path { // which HTTP would trim, onto another route
			return o, fmt.Errorf("%s: its path ends in spaces, which the exporter would trim; it takes one that "+
				"doesn't (its value isn't shown, since it may hold a secret)", name)
		}
		o.endpoint = u.String() // the signal's own, if it is set, after the general one
		grpcPath = strings.Trim(u.Path, "/") != ""
	}
	// Of the one that wins only, as the general one may serve another signal over HTTP.
	if o.protocol == "grpc" && grpcPath { // which gRPC drops, dialing the host alone
		return o, fmt.Errorf("%s: a path, which gRPC doesn't send; it takes the collector's address alone, "+
			"such as http://collector:4317 (its value isn't shown, since it may hold a secret)", endpointName)
	}
	// A certificate means TLS: the HTTP exporter refuses one for plaintext, and the gRPC one uses it whatever the
	// scheme or INSECURE say, so an http endpoint beside one fails here, naming both, as INSECURE=true does where it
	// would choose the endpoint, which an endpoint's own scheme otherwise does.
	insecure, insecureName, err := readInsecure(signal, getenv)
	if err != nil {
		return o, err
	}
	certificate := certificateOf(signal, getenv)
	if err := checkTLSFiles(signal, getenv); err != nil {
		return o, err
	}
	switch {
	case certificate != "" && insecure && o.endpoint == "":
		return o, fmt.Errorf("%s=true asks for plaintext, though %s asks for TLS; it takes false", insecureName,
			certificate)
	case certificate != "" && strings.HasPrefix(o.endpoint, "http:"):
		return o, fmt.Errorf("%s: an http endpoint, though %s asks for TLS; it takes https", endpointName,
			certificate)
	case o.endpoint == "":
		o.endpoint = defaultEndpoint(signal, o.protocol, certificate != "" || insecureName != "" && !insecure)
	}
	var headersName string // the variable o.headers come from
	// Both header variables are read too, for the same reason, the signal's own winning, so a malformed one fails
	// either way. The headers the signal sends are then checked against the rules of the transport that sends them,
	// which would otherwise refuse one, logging its name as it is, or refuse every request it makes.
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_" + signal + "_HEADERS"} {
		headers, err := readPairs(name, true, getenv)
		if err != nil {
			return o, err
		}
		if headers != nil {
			o.headers, headersName = headers, name
		}
	}
	if o.headers == nil {
		o.headers = map[string]string{} // none, rather than what the process's environment holds
	}
	transport, valid := "HTTP", isHTTPHeader
	if o.protocol == "grpc" {
		transport, valid = "gRPC", isGRPCHeader
	}
	lower := map[string]bool{}
	for k, v := range o.headers {
		if lower[strings.ToLower(k)] { // as both transports take names in any case, and would send either
			return o, fmt.Errorf("%s: two headers have one name in different cases (their names and values "+
				"aren't shown, since they may be secret)", headersName)
		}
		lower[strings.ToLower(k)] = true
		if !valid(k, v) {
			return o, fmt.Errorf("%s: a header isn't one %s takes (its names and values aren't shown, "+
				"since they may be secret)", headersName, transport)
		}
	}
	name, _ = own("TIMEOUT")
	o.timeout, err = readMillis(name, 10*time.Second, getenv)
	return o, err
}

// readInsecure returns what OTEL_EXPORTER_OTLP_[signal_]INSECURE holds, the signal's own winning, and the name of the
// variable it comes from, or "" if neither is set.
func readInsecure(signal string, getenv func(string) string) (insecure bool, name string, err error) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_" + signal + "_INSECURE", "OTEL_EXPORTER_OTLP_INSECURE"} {
		insecure, set, err := readBool(name, getenv)
		if err != nil || set {
			return insecure, name, err
		}
	}
	return false, "", nil
}

// certificateOf returns the name of a certificate variable set for signal that the exporters load, which asks for TLS,
// or "" if none is: a CA certificate, or a client certificate with the key of the same prefix, as they load only both.
func certificateOf(signal string, getenv func(string) string) string {
	set := func(name string) bool { return strings.TrimSpace(getenv(name)) != "" } // as the exporters trim them
	for _, prefix := range []string{"OTEL_EXPORTER_OTLP_" + signal + "_", "OTEL_EXPORTER_OTLP_"} {
		if set(prefix + "CERTIFICATE") {
			return prefix + "CERTIFICATE"
		}
		if set(prefix+"CLIENT_CERTIFICATE") && set(prefix+"CLIENT_KEY") {
			return prefix + "CLIENT_CERTIFICATE"
		}
	}
	return ""
}

// defaultEndpoint returns the endpoint of a signal whose endpoint isn't set: the specification's, plain
// http://localhost, unless tls, as a certificate or INSECURE=false ask for it.
// It is given whole, so that neither the SDK's own default, which is TLS, nor the scheme of an endpoint the process's
// environment holds decides the transport's security; the exporters still read the certificates themselves.
func defaultEndpoint(signal, protocol string, tls bool) string {
	scheme := "http"
	if tls {
		scheme = "https"
	}
	if protocol == "grpc" {
		return scheme + "://localhost:4317"
	}
	return scheme + "://localhost:4318/v1/" + strings.ToLower(signal)
}

// checkTLSFiles returns the first certificate or key that the variables of signal name and the exporters couldn't load,
// which they would only log, going on with the system's roots or without the client's identity.
func checkTLSFiles(signal string, getenv func(string) string) error {
	path := func(name string) string { return strings.TrimSpace(getenv(name)) }
	for _, prefix := range []string{"OTEL_EXPORTER_OTLP_", "OTEL_EXPORTER_OTLP_" + signal + "_"} {
		if name := prefix + "CERTIFICATE"; path(name) != "" {
			data, err := os.ReadFile(path(name))
			if err != nil {
				return fmt.Errorf("%s=%s: %w", name, path(name), err)
			}
			if !x509.NewCertPool().AppendCertsFromPEM(data) {
				return fmt.Errorf("%s=%s: holds no PEM certificate; it takes a CA's", name, path(name))
			}
		}
		certificate, key := prefix+"CLIENT_CERTIFICATE", prefix+"CLIENT_KEY"
		switch {
		case path(certificate) == "" && path(key) == "":
		case path(certificate) == "" || path(key) == "":
			return fmt.Errorf("%s and %s: one is set without the other; they take a client's certificate and its key "+
				"together", certificate, key)
		default:
			if _, err := tls.LoadX509KeyPair(path(certificate), path(key)); err != nil {
				return fmt.Errorf("%s=%s and %s: not a certificate and its key: %w", certificate, path(certificate),
					key, err)
			}
		}
	}
	return nil
}

// readBool returns the boolean the variable name holds, true or false in any case, and whether it is set at all.
func readBool(name string, getenv func(string) string) (value, set bool, err error) {
	switch v := strings.TrimSpace(getenv(name)); strings.ToLower(v) {
	case "":
		return false, false, nil
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("%s=%s: not a boolean; it takes true or false", name, v)
	}
}

// isPort reports whether p is a URL's port that the exporters can dial, or none.
func isPort(p string) bool {
	if p == "" {
		return true
	}
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n <= 65535
}

// readPairs returns the comma-separated key=value pairs that the variable name holds, percent-decoded, as
// OTEL_RESOURCE_ATTRIBUTES and the OTLP headers take them.
// The mistake of a secret variable, as headers may hold credentials, names the pair by its place, not its text.
func readPairs(name string, secret bool, getenv func(string) string) (map[string]string, error) {
	value := getenv(name)
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	pairs := map[string]string{}
	i := 0
	for pair := range strings.SplitSeq(value, ",") {
		i++
		if strings.TrimSpace(pair) == "" { // as a trailing comma leaves
			continue
		}
		// The key trimmed, not decoded, as both the exporters and the SDK read it. A header's value is decoded, then
		// trimmed, as the exporters read it, so that an escaped space at an end isn't sent; a resource attribute's is
		// trimmed, then decoded, as the SDK reads it, so that an escaped space is kept.
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		var verr error
		if secret {
			v, verr = url.PathUnescape(v)
			v = strings.TrimSpace(v)
		} else {
			v, verr = url.PathUnescape(strings.TrimSpace(v))
		}
		if !ok || k == "" || verr != nil || !secret && (!utf8.ValidString(k) || !utf8.ValidString(v)) {
			if secret {
				return nil, fmt.Errorf("%s: its pair %d isn't one; it takes key=value pairs separated by commas "+
					"(its values aren't shown, since they may be secret)", name, i)
			}
			return nil, fmt.Errorf("%s=%s: %q isn't a pair; it takes key=value pairs separated by commas",
				name, value, pair)
		}
		pairs[k] = v
	}
	if len(pairs) == 0 { // as a lone comma leaves, which is as good as unset
		return nil, nil
	}
	return pairs, nil
}

// isHTTPHeader reports whether k and v may be an HTTP header's name and value, as RFC 9110 says:
// a token, and no control character but a tab.
func isHTTPHeader(k, v string) bool {
	for _, r := range k {
		if r > 0x7e || r <= ' ' || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r) {
			return false
		}
	}
	for _, r := range v {
		if r < ' ' && r != '\t' || r == 0x7f {
			return false
		}
	}
	return k != ""
}

// isGRPCHeader reports whether k and v may be gRPC metadata, as grpc-go takes it once it has made k lower-case:
// a key of letters, digits, "-", "_" and ".", and a value of printable ASCII, or anything for a binary key, one that
// ends in -bin.
func isGRPCHeader(k, v string) bool {
	k = strings.ToLower(k)
	if k == "" {
		return false
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	if strings.HasSuffix(k, "-bin") {
		return true
	}
	for _, r := range v {
		if r < ' ' || r > '~' {
			return false
		}
	}
	return true
}

// maxMillis is the most milliseconds a time.Duration holds.
const maxMillis = int64(math.MaxInt64 / time.Millisecond)

// readMillis returns the duration that the variable name holds in milliseconds, or unset if it is unset.
// Zero is a mistake, rather than the SDK's default or no limit, which the variables' readers don't agree on.
func readMillis(name string, unset time.Duration, getenv func(string) string) (time.Duration, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return unset, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || n > maxMillis {
		return 0, fmt.Errorf("%s=%s: not a duration; it takes a positive number of milliseconds", name, value)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// maxCount bounds the counts the batch span processor takes, which the SDK makes channels and slices of,
// so that a count no machine could hold fails Setup rather than panicking in the SDK.
const maxCount = 1 << 20

// readCount returns the positive count, at most maxCount, that the variable name holds, or unset if it is unset.
func readCount(name string, unset int, getenv func(string) string) (int, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return unset, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 || n > maxCount {
		return 0, fmt.Errorf("%s=%s: not a count; it takes a number from 1 to %d", name, value, maxCount)
	}
	return n, nil
}

// orList returns items as a list ending in "or".
func orList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}
