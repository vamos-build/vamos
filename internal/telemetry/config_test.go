package telemetry

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"maps"
	"math/big"
	"os"
	"reflect"
	"testing"
	"time"
)

// envOf returns a getenv that reads env alone.
func envOf(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// TestReadSettings checks the exporters each environment chooses for each role, that nothing configured exports
// nothing, and that OTEL_SDK_DISABLED turns every export off.
func TestReadSettings(t *testing.T) {
	cases := []struct {
		name     string
		role     Role
		env      map[string]string
		traces   []string
		metrics  []string
		warnings []string
	}{
		{
			name: "nothing set exports nothing",
			role: Coordinator,
		},
		{
			name:    "an OTLP endpoint alone exports both over OTLP",
			role:    Coordinator,
			env:     map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"},
			traces:  []string{"otlp"},
			metrics: []string{"otlp"},
		},
		{
			name:   "a signal's own endpoint exports that signal",
			role:   Coordinator,
			env:    map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"},
			traces: []string{"otlp"},
		},
		{
			name: "OTEL_SDK_DISABLED turns an endpoint off",
			role: Coordinator,
			env: map[string]string{
				"OTEL_SDK_DISABLED":           "true",
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
				"OTEL_METRICS_EXPORTER":       "prometheus",
			},
		},
		{
			name: "several metrics exporters, none among them",
			role: Coordinator,
			env: map[string]string{
				"OTEL_METRICS_EXPORTER": "otlp, prometheus,none,otlp",
				"OTEL_TRACES_EXPORTER":  "console",
			},
			traces:  []string{"console"},
			metrics: []string{"otlp", "prometheus"},
		},
		{
			name: "none alone",
			role: Coordinator,
			env: map[string]string{
				"OTEL_TRACES_EXPORTER":        "none",
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
			},
			metrics: []string{"otlp"},
		},
		{
			name: "a worker takes no settings of traces, which it doesn't send",
			role: Worker,
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://collector:4318/v1/metrics",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  "collector",
			},
			metrics: []string{"otlp"},
		},
		{
			name: "a worker takes no settings of the batch span processor, which it has none of",
			role: Worker,
			env: map[string]string{
				"OTEL_METRICS_EXPORTER":   "console",
				"OTEL_BSP_SCHEDULE_DELAY": "0",
				"OTEL_TRACES_SAMPLER":     "sometimes",
			},
			metrics: []string{"console"},
		},
		{
			name: "gRPC headers with capitals, which grpc-go makes lower-case",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Basic%20abc"},
			traces:  []string{"otlp"},
			metrics: []string{"otlp"},
		},
		{
			name: "general headers HTTP takes but gRPC doesn't, which the gRPC signal's own replace",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
				"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_HEADERS": "x!tag=v",
				"OTEL_EXPORTER_OTLP_TRACES_HEADERS": "x-tag=v"},
			traces:  []string{"otlp"},
			metrics: []string{"otlp"},
		},
		{
			name: "binary gRPC metadata, which takes any byte",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_HEADERS": "custom-bin=%00%FF"},
			traces:  []string{"otlp"},
			metrics: []string{"otlp"},
		},
		{
			name: "a worker takes no Prometheus port, which it serves none on",
			role: Worker,
			env: map[string]string{
				"OTEL_METRICS_EXPORTER":         "console",
				"OTEL_EXPORTER_PROMETHEUS_PORT": "metrics",
			},
			metrics: []string{"console"},
		},
		{
			name: "a worker ignores the traces exporter, saying so",
			role: Worker,
			env: map[string]string{
				"OTEL_TRACES_EXPORTER":        "otlp",
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
			},
			metrics: []string{"otlp"},
			warnings: []string{"OTEL_TRACES_EXPORTER=otlp is ignored: a worker exports no traces, " +
				"since its coordinator exports its spans"},
		},
		{
			name:    "a worker takes a traces exporter of none without a word",
			role:    Worker,
			env:     map[string]string{"OTEL_TRACES_EXPORTER": "None, none", "OTEL_METRICS_EXPORTER": "console"},
			metrics: []string{"console"},
		},
		{
			name: "a worker whose SDK is disabled says nothing of its traces exporter",
			role: Worker,
			env:  map[string]string{"OTEL_SDK_DISABLED": "true", "OTEL_TRACES_EXPORTER": "otlp"},
		},
		{
			name:    "Prometheus alone, which is scraped, takes no push settings",
			role:    Coordinator,
			env:     map[string]string{"OTEL_METRICS_EXPORTER": "prometheus", "OTEL_METRIC_EXPORT_INTERVAL": "0"},
			metrics: []string{"prometheus"},
		},
		{
			name: "exporters' names in any case",
			role: Coordinator,
			env: map[string]string{"OTEL_TRACES_EXPORTER": "OTLP", "OTEL_METRICS_EXPORTER": "Prometheus,CONSOLE",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "GRPC"},
			traces:  []string{"otlp"},
			metrics: []string{"prometheus", "console"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, warnings, err := readSettings(c.role, envOf(c.env))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.traces, c.traces) || !reflect.DeepEqual(s.metrics, c.metrics) {
				t.Errorf("exporters = traces %q, metrics %q; want %q, %q", s.traces, s.metrics, c.traces, c.metrics)
			}
			if !reflect.DeepEqual(warnings, c.warnings) {
				t.Errorf("warnings = %q, want %q", warnings, c.warnings)
			}
		})
	}
}

// endpointMistake is the mistake of an endpoint variable name, which doesn't show its value.
func endpointMistake(name string) string {
	return name + ": not an endpoint; it takes an http or https URL without credentials, a query or an escaped path, " +
		"such as http://collector:4318 (its value isn't shown, since it may hold a secret)"
}

// headerMistake is the mistake of a headers variable name whose header transport doesn't take.
func headerMistake(name, transport string) string {
	return name + ": a header isn't one " + transport + " takes (its names and values aren't shown, " +
		"since they may be secret)"
}

// TestReadSettingsMistakes checks that each value Setup can't take fails it, naming the variable, its value and what
// it takes.
func TestReadSettingsMistakes(t *testing.T) {
	ca, certificate, key := tlsFiles(t)
	cases := []struct {
		name string
		role Role
		env  map[string]string
		want string
	}{
		{
			name: "an unknown traces exporter",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"},
			want: `OTEL_TRACES_EXPORTER=zipkin: "zipkin" isn't an exporter; it takes otlp, console or none`,
		},
		{
			name: "an unknown metrics exporter among others",
			role: Coordinator,
			env:  map[string]string{"OTEL_METRICS_EXPORTER": "otlp,statsd"},
			want: `OTEL_METRICS_EXPORTER=otlp,statsd: "statsd" isn't an exporter; ` +
				`it takes otlp, prometheus, console or none`,
		},
		{
			name: "Prometheus on a worker",
			role: Worker,
			env:  map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"},
			want: "OTEL_METRICS_EXPORTER=prometheus: a worker can't serve Prometheus; " +
				"workers export metrics over OTLP, so it takes otlp, console or none",
		},
		{
			name: "two exporters on a worker",
			role: Worker,
			env:  map[string]string{"OTEL_METRICS_EXPORTER": "otlp,console"},
			want: "OTEL_METRICS_EXPORTER=otlp,console: a worker takes one exporter, otlp, console or none",
		},
		{
			name: "a mistake is one even when the SDK is disabled",
			role: Coordinator,
			env:  map[string]string{"OTEL_SDK_DISABLED": "true", "OTEL_TRACES_EXPORTER": "jaeger"},
			want: `OTEL_TRACES_EXPORTER=jaeger: "jaeger" isn't an exporter; it takes otlp, console or none`,
		},
		{
			name: "an unknown protocol",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"},
			want: "OTEL_EXPORTER_OTLP_PROTOCOL=http/json: not a protocol vamos sends; it takes http/protobuf or grpc",
		},
		{
			name: "a client certificate without its key, which the exporters would ignore",
			role: Coordinator,
			env: map[string]string{"OTEL_TRACES_EXPORTER": "otlp",
				"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": certificate},
			want: "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE and OTEL_EXPORTER_OTLP_CLIENT_KEY: one is set without the " +
				"other; they take a client's certificate and its key together",
		},
		{
			name: "a CA certificate that isn't one, which the exporters would ignore for the system's",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": key},
			want: "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE=" + key + ": holds no PEM certificate; it takes a CA's",
		},
		{
			name: "a gRPC endpoint with a path, which gRPC drops",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4317/otlp",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
			want: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: a path, which gRPC doesn't send; it takes the collector's " +
				"address alone, such as http://collector:4317 (its value isn't shown, since it may hold a secret)",
		},
		{
			name: "an endpoint with credentials, which the exporters would drop",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://user:s3cret@collector/v1/traces"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		},
		{
			name: "an endpoint whose port can't be dialed",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:65536/v1/traces"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		},
		{
			name: "an endpoint with an escaped path, which the exporters would decode",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/a%2Fb"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		},
		{
			name: "an http endpoint though a certificate asks for TLS",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
				"OTEL_EXPORTER_OTLP_CERTIFICATE": ca},
			want: "OTEL_EXPORTER_OTLP_ENDPOINT: an http endpoint, though OTEL_EXPORTER_OTLP_CERTIFICATE " +
				"asks for TLS; it takes https",
		},
		{
			name: "an http endpoint though a certificate asks for TLS, whatever INSECURE says",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces",
				"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": ca, "OTEL_EXPORTER_OTLP_INSECURE": "false"},
			want: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: an http endpoint, " +
				"though OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE asks for TLS; it takes https",
		},
		{
			name: "INSECURE=true beside a certificate",
			role: Coordinator,
			env: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": certificate,
				"OTEL_EXPORTER_OTLP_CLIENT_KEY": key, "OTEL_EXPORTER_OTLP_TRACES_INSECURE": "true"},
			want: "OTEL_EXPORTER_OTLP_TRACES_INSECURE=true asks for plaintext, though " +
				"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE asks for TLS; it takes false",
		},
		{
			name: "an endpoint whose path the exporters would trim",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/route%20"},
			want: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: its path ends in spaces, which the exporter would trim; " +
				"it takes one that doesn't (its value isn't shown, since it may hold a secret)",
		},
		{
			name: "an INSECURE that isn't a boolean, which would otherwise choose the transport's security",
			role: Coordinator,
			env: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_INSECURE": "false",
				"OTEL_EXPORTER_OTLP_TRACES_INSECURE": "flase"},
			want: "OTEL_EXPORTER_OTLP_TRACES_INSECURE=flase: not a boolean; it takes true or false",
		},
		{
			name: "OTEL_SDK_DISABLED that isn't a boolean",
			role: Coordinator,
			env:  map[string]string{"OTEL_SDK_DISABLED": "yes"},
			want: "OTEL_SDK_DISABLED=yes: not a boolean; it takes true or false",
		},
		{
			name: "headers of one name in two cases, of which either would be sent",
			role: Coordinator,
			env: map[string]string{"OTEL_TRACES_EXPORTER": "otlp",
				"OTEL_EXPORTER_OTLP_HEADERS": "authorization=old,Authorization=new"},
			want: "OTEL_EXPORTER_OTLP_HEADERS: two headers have one name in different cases (their names and values " +
				"aren't shown, since they may be secret)",
		},
		{
			name: "an endpoint that isn't a URL",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "collector:4318"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		},
		{
			name: "an endpoint of another scheme, whose credentials aren't shown",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "ftp://user:s3cret@collector"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		},
		{
			name: "a general endpoint with a query, which the signal's path would land in",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318?token=s3cret"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_ENDPOINT"),
		},
		{
			name: "a general endpoint the exporter would read, though the signal's own wins",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:s3cret@[collector",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_ENDPOINT"),
		},
		{
			name: "general headers the exporter would read, though the signal's own win",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
				"OTEL_EXPORTER_OTLP_HEADERS": "api-key=s3cr%zzet", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "api-key=ok"},
			want: "OTEL_EXPORTER_OTLP_HEADERS: its pair 1 isn't one; it takes key=value pairs separated by commas " +
				"(its values aren't shown, since they may be secret)",
		},
		{
			name: "a signal's own endpoint with a query, which the exporter wouldn't send",
			role: Coordinator,
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://ingest.example/v1/traces?tenant=a"},
			want: endpointMistake("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		},
		{
			name: "headers that aren't pairs, whose values aren't shown",
			role: Coordinator,
			env: map[string]string{"OTEL_TRACES_EXPORTER": "otlp",
				"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer%20s3cret,s3cret"},
			want: "OTEL_EXPORTER_OTLP_HEADERS: its pair 2 isn't one; it takes key=value pairs separated by commas " +
				"(its values aren't shown, since they may be secret)",
		},
		{
			name: "a header whose name isn't an HTTP token, which isn't shown",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
				"OTEL_EXPORTER_OTLP_HEADERS": "api-key=ok,s3cret token=x"},
			want: headerMistake("OTEL_EXPORTER_OTLP_HEADERS", "HTTP"),
		},
		{
			name: "a header whose value holds a line break, which isn't shown",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
				"OTEL_EXPORTER_OTLP_HEADERS": "api-key=s3cret%0D%0AX-Injected: yes"},
			want: headerMistake("OTEL_EXPORTER_OTLP_HEADERS", "HTTP"),
		},
		{
			name: "a gRPC header whose name grpc-go refuses",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_HEADERS": "api!key=s3cret"},
			want: headerMistake("OTEL_EXPORTER_OTLP_HEADERS", "gRPC"),
		},
		{
			name: "a service name that isn't UTF-8",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_SERVICE_NAME": "vamos\xff"},
			want: `OTEL_SERVICE_NAME="vamos\xff": not a name; it takes UTF-8`,
		},
		{
			name: "a gRPC header whose value has a tab, which grpc-go refuses though HTTP takes it",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%09s3cret"},
			want: headerMistake("OTEL_EXPORTER_OTLP_HEADERS", "gRPC"),
		},
		{
			name: "resource attributes that aren't UTF-8, which OTLP refuses",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_RESOURCE_ATTRIBUTES": "label=%FF"},
			want: `OTEL_RESOURCE_ATTRIBUTES=label=%FF: "label=%FF" isn't a pair; ` +
				"it takes key=value pairs separated by commas",
		},
		{
			name: "resource attributes that aren't pairs",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_RESOURCE_ATTRIBUTES": "team"},
			want: `OTEL_RESOURCE_ATTRIBUTES=team: "team" isn't a pair; it takes key=value pairs separated by commas`,
		},
		{
			name: "a schedule delay that isn't a number",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_BSP_SCHEDULE_DELAY": "1h"},
			want: "OTEL_BSP_SCHEDULE_DELAY=1h: not a duration; it takes a positive number of milliseconds",
		},
		{
			name: "a schedule delay of zero",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_BSP_SCHEDULE_DELAY": "0"},
			want: "OTEL_BSP_SCHEDULE_DELAY=0: not a duration; it takes a positive number of milliseconds",
		},
		{
			name: "a timeout longer than a duration holds",
			role: Coordinator,
			env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
				"OTEL_EXPORTER_OTLP_TIMEOUT": "9223372036854775"},
			want: "OTEL_EXPORTER_OTLP_TIMEOUT=9223372036854775: not a duration; " +
				"it takes a positive number of milliseconds",
		},
		{
			name: "a queue of none",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_BSP_MAX_QUEUE_SIZE": "0"},
			want: "OTEL_BSP_MAX_QUEUE_SIZE=0: not a count; it takes a number from 1 to 1048576",
		},
		{
			name: "a batch no machine could hold",
			role: Coordinator,
			env: map[string]string{"OTEL_TRACES_EXPORTER": "console",
				"OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "9223372036854775807"},
			want: "OTEL_BSP_MAX_EXPORT_BATCH_SIZE=9223372036854775807: not a count; " +
				"it takes a number from 1 to 1048576",
		},
		{
			name: "an empty bracketed Prometheus host",
			role: Coordinator,
			env:  map[string]string{"OTEL_METRICS_EXPORTER": "prometheus", "OTEL_EXPORTER_PROMETHEUS_HOST": "[]\n"},
			want: "OTEL_EXPORTER_PROMETHEUS_HOST=[]: not a host; it takes a name or an address",
		},
		{
			name: "a Prometheus host with a stray bracket",
			role: Coordinator,
			env:  map[string]string{"OTEL_METRICS_EXPORTER": "prometheus", "OTEL_EXPORTER_PROMETHEUS_HOST": "[::1"},
			want: "OTEL_EXPORTER_PROMETHEUS_HOST=[::1: not a host; it takes a name or an address",
		},
		{
			name: "a Prometheus port that isn't one",
			role: Coordinator,
			env:  map[string]string{"OTEL_METRICS_EXPORTER": "prometheus", "OTEL_EXPORTER_PROMETHEUS_PORT": "metrics"},
			want: "OTEL_EXPORTER_PROMETHEUS_PORT=metrics: not a port; it takes a number from 0 to 65535",
		},
		{
			name: "an unknown sampler",
			role: Coordinator,
			env:  map[string]string{"OTEL_TRACES_SAMPLER": "sometimes"},
			want: "OTEL_TRACES_SAMPLER=sometimes: not a sampler; it takes always_on, always_off, traceidratio, " +
				"parentbased_always_on, parentbased_always_off or parentbased_traceidratio",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := readSettings(c.role, envOf(c.env))
			if err == nil || err.Error() != c.want {
				t.Errorf("readSettings = %v, want %s", err, c.want)
			}
		})
	}
}

// TestDefaultEndpoint checks that an unset endpoint is the specification's plain http://localhost, unless an INSECURE
// variable of false or a certificate the exporters load asks for TLS, the signal's own INSECURE over the general one,
// and that an endpoint's own scheme decides it otherwise.
func TestDefaultEndpoint(t *testing.T) {
	ca, certificate, key := tlsFiles(t)
	for _, c := range []struct {
		protocol string
		env      map[string]string
		want     string
	}{
		{want: "http://localhost:4318/v1/traces"},
		{protocol: "grpc", want: "http://localhost:4317"},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "FALSE"}, want: "https://localhost:4318/v1/traces"},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "false", "OTEL_EXPORTER_OTLP_TRACES_INSECURE": "true"},
			want: "http://localhost:4318/v1/traces"},
		{protocol: "grpc", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": ca},
			want: "https://localhost:4317"},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": certificate,
			"OTEL_EXPORTER_OTLP_CLIENT_KEY": key}, want: "https://localhost:4318/v1/traces"},
		// A blank certificate, which the exporters don't load.
		{env: map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": " "}, want: "http://localhost:4318/v1/traces"},
		// An explicit https endpoint and its CA, beside a fleet's INSECURE=true, which its scheme overrides.
		{env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "true",
			"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE": ca,
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":    "https://collector:4318/v1/traces"},
			want: "https://collector:4318/v1/traces"},
		// A general endpoint whose path ends in a space, which the signal's path then follows, and a blank one.
		{env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318/route%20"},
			want: "http://collector:4318/route%20/v1/traces"},
		{env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "  "}, want: "http://localhost:4318/v1/traces"},
		// A blank signal's protocol, which the general one's gRPC stands for.
		{protocol: "grpc", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": " "},
			want: "http://localhost:4317"},
	} {
		env := map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": c.protocol}
		maps.Copy(env, c.env)
		o, err := readOTLP(signalTraces, envOf(env))
		if err != nil {
			t.Fatal(err)
		}
		if o.endpoint != c.want {
			t.Errorf("the endpoint of %s, %v = %s, want %s", c.protocol, c.env, o.endpoint, c.want)
		}
	}
}

// TestReadSettingsValues checks how the OTLP, batch and Prometheus variables are read: a signal's own over the general
// ones, the general endpoint with the signal's path for HTTP and without for gRPC, the defaults of what is unset,
// a trailing comma, and Prometheus's address, whose host may be bracketed.
func TestReadSettingsValues(t *testing.T) {
	s, _, err := readSettings(Coordinator, envOf(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://collector:4318/otlp/",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://metrics:4317",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc",
		"OTEL_EXPORTER_OTLP_HEADERS":          "api-key=s%3Dcret, team=build,pad=x%20,x%2Fauth=t",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT":   "2500",
		"OTEL_BSP_SCHEDULE_DELAY":             "3600000\n", // as a ConfigMap's value may end
		"OTEL_BSP_MAX_EXPORT_BATCH_SIZE":      "64",
		"OTEL_METRIC_EXPORT_INTERVAL":         "100",
		"OTEL_METRICS_EXPORTER":               "otlp,prometheus",
		"OTEL_EXPORTER_PROMETHEUS_HOST":       "[::]",
		"OTEL_EXPORTER_PROMETHEUS_PORT":       "9000\n",
		"OTEL_RESOURCE_ATTRIBUTES":            "deployment.environment=ci,team=%20build%20,team%2Fname=a",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS":  "team=metrics",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":   ",", // as good as unset, so the general ones stand
		"OTEL_BSP_MAX_QUEUE_SIZE":             "32",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]otlpSettings{
		signalTraces: {
			endpoint: "http://collector:4318/otlp/v1/traces",
			protocol: "http/protobuf",
			// Values trimmed once decoded; names as they are.
			headers: map[string]string{"api-key": "s=cret", "team": "build", "pad": "x", "x%2Fauth": "t"},
			timeout: 2500 * time.Millisecond,
		},
		signalMetrics: {
			endpoint: "http://metrics:4317",
			protocol: "grpc",
			headers:  map[string]string{"team": "metrics"},
			timeout:  10 * time.Second,
		},
	}
	if !reflect.DeepEqual(s.otlp, want) {
		t.Errorf("OTLP = %+v, want %+v", s.otlp, want)
	}
	wantBatch := batchSettings{scheduleDelay: time.Hour, exportTimeout: 30 * time.Second, maxQueueSize: 32,
		maxExportBatch: 32} // no larger than the queue
	if s.batch != wantBatch || s.metricInterval != 100*time.Millisecond {
		t.Errorf("batch = %+v, metric interval %s; want %+v, 100ms", s.batch, s.metricInterval, wantBatch)
	}
	if s.prometheusAddr != "[::]:9000" {
		t.Errorf("Prometheus address = %s, want [::]:9000", s.prometheusAddr)
	}
	if !reflect.DeepEqual(s.resource, map[string]string{"deployment.environment": "ci", "team": " build ",
		"team%2Fname": "a"}) {
		t.Errorf("resource = %v", s.resource)
	}
}

// tlsFiles returns the paths of a CA certificate, and a client's certificate and key, that the exporters can load.
func tlsFiles(t *testing.T) (ca, certificate, key string) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ca, key = dir+"/ca.pem", dir+"/client.key"
	write := func(path, kind string, data []byte) {
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(ca, "CERTIFICATE", der)
	write(key, "PRIVATE KEY", keyDER)
	return ca, ca, key // the CA is its own client certificate
}
