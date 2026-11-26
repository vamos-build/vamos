{{/* The chart's name, as nameOverride may change it. */}}
{{- define "vamos.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* The prefix of every object's name: the release's name, with the chart's unless the release already has it. */}}
{{- define "vamos.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* The labels of every object. */}}
{{- define "vamos.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: {{ include "vamos.name" . }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/* The selector labels of one component, given as (dict "root" $ "component" "coordinator"). */}}
{{- define "vamos.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vamos.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* The Vamos image. */}}
{{- define "vamos.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{/* The coordinator's URL, as the workers name it. */}}
{{- define "vamos.coordinatorURL" -}}
{{- printf "http://%s-coordinator:%v" (include "vamos.fullname" .) .Values.coordinator.service.port }}
{{- end }}

{{/* The cache registry's host:port, as everyone names it. */}}
{{- define "vamos.cacheRegistry" -}}
{{- if .Values.cacheRegistry.address }}
{{- .Values.cacheRegistry.address }}
{{- else if .Values.cacheRegistry.enabled }}
{{- printf "%s-registry:%v" (include "vamos.fullname" .) .Values.cacheRegistry.service.port }}
{{- else }}
{{- fail "cacheRegistry.address is required when cacheRegistry.enabled is false" }}
{{- end }}
{{- end }}

{{/* The host:port of the pull-through cache given as (dict "root" $ "name" $name "cache" $cache). */}}
{{- define "vamos.pullThroughAddress" -}}
{{- if .cache.address }}
{{- .cache.address }}
{{- else if .cache.enabled }}
{{- printf "%s-%s:%v" (include "vamos.fullname" .root) .name .cache.service.port }}
{{- else }}
{{- fail (printf "pullThrough.caches.%s.address is required when the cache isn't enabled" .name) }}
{{- end }}
{{- end }}

{{/*
The --pull-through mappings, one upstream=cache per line: the chart's caches, then the external ones.
An upstream mapped twice fails, since the coordinator would keep only one of them.
*/}}
{{- define "vamos.pullThroughMappings" -}}
{{- $seen := dict }}
{{- range $name, $cache := .Values.pullThrough.caches }}
{{- if hasKey $seen $cache.upstream }}
{{- fail (printf "pullThrough: %s is mapped more than once" $cache.upstream) }}
{{- end }}
{{- $_ := set $seen $cache.upstream true }}
{{ $cache.upstream }}={{ include "vamos.pullThroughAddress" (dict "root" $ "name" $name "cache" $cache) }}
{{- end }}
{{- range $upstream, $address := .Values.pullThrough.external }}
{{- if hasKey $seen $upstream }}
{{- fail (printf "pullThrough: %s is mapped more than once" $upstream) }}
{{- end }}
{{- $_ := set $seen $upstream true }}
{{ $upstream }}={{ $address }}
{{- end }}
{{- end }}

{{/* The platforms the worker pools serve, one per line, without repeats. */}}
{{- define "vamos.poolPlatforms" -}}
{{- $platforms := list }}
{{- range $name, $pool := .Values.workers.pools }}
{{- if not $pool }}{{ continue }}{{ end }}
{{- if not $pool.platform }}
{{- fail (printf "workers.pools.%s.platform is required, such as linux/amd64" $name) }}
{{- end }}
{{- $platforms = append $platforms $pool.platform }}
{{- end }}
{{- range concat $platforms .Values.coordinator.offer | uniq }}
{{ . }}
{{- end }}
{{- end }}

{{/* The registry settings volume and its mount, which every Vamos process reads as $CONTAINERS_REGISTRIES_CONF. */}}
{{- define "vamos.registriesVolume" -}}
- name: registries
  configMap:
    name: {{ include "vamos.fullname" . }}-registries
{{- end }}

{{- define "vamos.registriesMount" -}}
- name: registries
  mountPath: /etc/vamos/registries.conf
  subPath: registries.conf
  readOnly: true
{{- end }}

{{/*
Registry credentials, given as (dict "secret" $name): a volume and a mount, and the variable that names the file.
*/}}
{{- define "vamos.authVolume" -}}
{{- if .secret }}
- name: registry-auth
  secret:
    secretName: {{ .secret }}
    items:
      - key: .dockerconfigjson
        path: auth.json
{{- end }}
{{- end }}

{{- define "vamos.authMount" -}}
{{- if .secret }}
- name: registry-auth
  mountPath: /etc/vamos/auth
  readOnly: true
{{- end }}
{{- end }}

{{- define "vamos.authEnv" -}}
{{- if .secret }}
- name: REGISTRY_AUTH_FILE
  value: /etc/vamos/auth/auth.json
{{- end }}
{{- end }}

{{/* The PVC spec of a component's persistence values. */}}
{{- define "vamos.pvcSpec" -}}
accessModes:
  {{- toYaml .accessModes | nindent 2 }}
{{- with .storageClass }}
storageClassName: {{ if eq . "-" }}""{{ else }}{{ . }}{{ end }}
{{- end }}
resources:
  requests:
    storage: {{ .size }}
{{- end }}

{{/* A Service's spec, given as (dict "root" $ "component" $component "service" $service). */}}
{{- define "vamos.serviceSpec" -}}
type: {{ .service.type }}
selector:
  {{- include "vamos.selectorLabels" (dict "root" .root "component" .component) | nindent 2 }}
ports:
  - name: http
    port: {{ .service.port }}
    targetPort: http
    protocol: TCP
    {{- if and (has .service.type (list "NodePort" "LoadBalancer")) .service.nodePort }}
    nodePort: {{ .service.nodePort }}
    {{- end }}
  {{- with .metricsPort }}
  - name: metrics
    port: {{ . }}
    targetPort: metrics
    protocol: TCP
  {{- end }}
{{- end }}

{{/*
The telemetry values, each defaulted, as a release upgraded with --reuse-values from a chart before them has none.
*/}}
{{- define "vamos.telemetryValues" -}}
{{- $d := dict "otlp" (dict "endpoint" "" "protocol" "" "headersSecret" "")
      "traces" (dict "sampler" "" "samplerArg" "")
      "prometheus" (dict "enabled" false "port" 9464 "podAnnotations" false) }}
{{- $t := mergeOverwrite $d (deepCopy (.Values.telemetry | default dict)) }}
{{- /* Switches given as strings or numbers, as --set-string or --set may give them, are switches all the same. */}}
{{- range $name := list "enabled" "podAnnotations" }}
{{- $v := get $t.prometheus $name | default "" | toString | lower | trim }}
{{- if has $v (list "true" "1") }}
{{- $_ := set $t.prometheus $name true }}
{{- else if has $v (list "false" "0" "") }}
{{- $_ := set $t.prometheus $name false }}
{{- else }}
{{- fail (printf "telemetry.prometheus.%s is %q; it takes true or false" $name $v) }}
{{- end }}
{{- end }}
{{- toYaml $t }}
{{- end }}

{{/*
The OTEL_* environment of a role, given as (dict "root" $ "role" "coordinator" "extraEnv" $extraEnv) or "worker", from
the telemetry values: none for the defaults, and none that extraEnv sets, which overrides it. Workers export metrics
alone, so they get no sampler, and serve no Prometheus.
*/}}
{{- define "vamos.telemetryEnv" -}}
{{- $t := include "vamos.telemetryValues" .root | fromYaml }}
{{- $skip := list }}
{{- $endpoints := list }}
{{- range .extraEnv }}
{{- $skip = append $skip .name }}
{{- if or .valueFrom (ne (.value | default "" | toString | trim) "") }}
{{- $endpoints = append $endpoints .name }}
{{- end }}
{{- end }}
{{- /* With Prometheus on, the chart chooses the exporters and serves the port, so an extraEnv can't set them. */}}
{{- if and $t.prometheus.enabled (eq .role "coordinator") }}
{{- range $name, $value := dict "OTEL_METRICS_EXPORTER" "the exporters, which telemetry.prometheus.enabled and an OTLP endpoint choose"
      "OTEL_EXPORTER_PROMETHEUS_HOST" "the host Prometheus is served on, every address, for the Service to reach"
      "OTEL_EXPORTER_PROMETHEUS_PORT" "the port Prometheus is served on, telemetry.prometheus.port, which the Service exposes" }}
{{- if has $name $skip }}
{{- fail (printf "coordinator.extraEnv can't set %s with Prometheus on: it is %s" $name $value) }}
{{- end }}
{{- end }}
{{- end }}
{{- $endpoint := $t.otlp.endpoint | toString | trim }}
{{- $env := list }}
{{- with $endpoint }}
{{- $env = append $env (dict "name" "OTEL_EXPORTER_OTLP_ENDPOINT" "value" .) }}
{{- end }}
{{- with $t.otlp.protocol | toString | trim }}
{{- $env = append $env (dict "name" "OTEL_EXPORTER_OTLP_PROTOCOL" "value" .) }}
{{- end }}
{{- with $t.otlp.headersSecret | toString | trim }}
{{- $env = append $env (dict "name" "OTEL_EXPORTER_OTLP_HEADERS"
      "valueFrom" (dict "secretKeyRef" (dict "name" . "key" "headers"))) }}
{{- end }}
{{- if eq .role "coordinator" }}
{{- with $t.traces.sampler | toString | trim }}
{{- $env = append $env (dict "name" "OTEL_TRACES_SAMPLER" "value" .) }}
{{- end }}
{{- /* A sampler argument of 0, which samples nothing, is a value, not an empty one. */}}
{{- if ne ($t.traces.samplerArg | toString | trim) "" }}
{{- $env = append $env (dict "name" "OTEL_TRACES_SAMPLER_ARG" "value" ($t.traces.samplerArg | toString)) }}
{{- end }}
{{- if $t.prometheus.enabled }}
{{- /* An OTLP endpoint, the values' or one the extraEnv sets, for every signal or metrics alone, exports there too. */}}
{{- /* A blank one in the extraEnv sets none, and one it blanks the values' with clears it. */}}
{{- $otlp := or (and (ne $endpoint "") (not (has "OTEL_EXPORTER_OTLP_ENDPOINT" $skip)))
      (has "OTEL_EXPORTER_OTLP_ENDPOINT" $endpoints) (has "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT" $endpoints) }}
{{- $env = append $env (dict "name" "OTEL_METRICS_EXPORTER"
      "value" (ternary "otlp,prometheus" "prometheus" $otlp)) }}
{{- /* Every address, which Vamos takes as a listener with no host, as the API's, IPv6's and IPv4's alike, or IPv4's
      alone on a node without IPv6, so that every cluster's scrapes reach it. */}}
{{- $env = append $env (dict "name" "OTEL_EXPORTER_PROMETHEUS_HOST" "value" "::") }}
{{- $env = append $env (dict "name" "OTEL_EXPORTER_PROMETHEUS_PORT" "value" ($t.prometheus.port | toString)) }}
{{- end }}
{{- end }}
{{- range $env }}
{{- if not (has .name $skip) }}
- {{ toYaml . | nindent 2 | trim }}
{{- end }}
{{- end }}
{{- end }}

{{/* An emptyDir's fields from medium and sizeLimit values, or {} for neither. */}}
{{- define "vamos.emptyDir" -}}
{{- $d := dict }}
{{- with .medium }}{{ $_ := set $d "medium" . }}{{ end }}
{{- with .sizeLimit }}{{ $_ := set $d "sizeLimit" . }}{{ end }}
{{- toYaml $d }}
{{- end }}
