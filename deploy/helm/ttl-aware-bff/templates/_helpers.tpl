{{/*
Shared template helpers for the ttl-aware-bff chart.

Naming convention follows the Helm standard: `<release>-<chart>`, truncated to
63 characters (the Kubernetes label/name limit) and trimmed of any trailing
dash that truncation may leave behind.
*/}}

{{/*
Chart name, overridable with nameOverride.
*/}}
{{- define "ttl-aware-bff.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. Used for every resource name.
When the release is already named after the chart, the name is not doubled up.
*/}}
{{- define "ttl-aware-bff.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name and version, as used by app.kubernetes.io/managed-by tooling.
The `+` in a SemVer build identifier is not legal in a label value.
*/}}
{{- define "ttl-aware-bff.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Selector labels — the immutable subset. These land in
Deployment.spec.selector.matchLabels, which cannot be changed after creation,
so nothing volatile (version, chart) may appear here.
*/}}
{{- define "ttl-aware-bff.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ttl-aware-bff.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Full label set for metadata.
*/}}
{{- define "ttl-aware-bff.labels" -}}
helm.sh/chart: {{ include "ttl-aware-bff.chart" . }}
{{ include "ttl-aware-bff.selectorLabels" . }}
app.kubernetes.io/version: {{ include "ttl-aware-bff.imageTag" . | quote }}
app.kubernetes.io/component: bff
app.kubernetes.io/part-of: ttl-aware-bff
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "ttl-aware-bff.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ttl-aware-bff.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Resolved image tag: image.tag, falling back to the chart's appVersion.
*/}}
{{- define "ttl-aware-bff.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag -}}
{{- end -}}

{{/*
Fully resolved image reference. A digest wins over a tag, because a digest is
the only reference that is actually immutable.
*/}}
{{- define "ttl-aware-bff.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (include "ttl-aware-bff.imageTag" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the ConfigMap holding bff.yaml.
*/}}
{{- define "ttl-aware-bff.configMapName" -}}
{{- printf "%s-config" (include "ttl-aware-bff.fullname" .) -}}
{{- end -}}

{{/*
Name of the Secret holding credentials, or "" when none is in play.
An existing Secret (External Secrets, sealed-secrets) always wins over one the
chart would create itself.
*/}}
{{- define "ttl-aware-bff.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else if .Values.secrets.create -}}
{{- printf "%s-secrets" (include "ttl-aware-bff.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Redis address for cache.redis.addr.

  redis.enabled            -> the Bitnami subchart's master Service
  externalRedis.host set   -> that host:port
  neither                  -> whatever config.cache.redis.addr already says

The Bitnami chart names its master Service "<release>-redis-master" in the
standalone architecture; that name is reproduced here rather than guessed.
*/}}
{{- define "ttl-aware-bff.redisAddr" -}}
{{- if .Values.redis.enabled -}}
{{- printf "%s-redis-master:%d" .Release.Name (int 6379) -}}
{{- else if .Values.externalRedis.host -}}
{{- printf "%s:%d" .Values.externalRedis.host (int .Values.externalRedis.port) -}}
{{- else -}}
{{- .Values.config.cache.redis.addr -}}
{{- end -}}
{{- end -}}

{{/*
The effective bff.yaml: the `config` tree with the higher-level values merged
in, so that settings like the OTLP endpoint and the Redis address have exactly
one home in values.yaml rather than two that can disagree.

Merge order (later wins):
  1. .Values.config
  2. derived values (otel.*, redis/externalRedis, image tag)
  3. .Values.routingOverrides   -> config.routing
  4. .Values.tenantOverrides    -> config.tenants

mergeOverwrite is a deep merge, so an override may name a single leaf (one TTL)
without restating its siblings.
*/}}
{{- define "ttl-aware-bff.config" -}}
{{- $cfg := deepCopy .Values.config -}}

{{/* --- Cache address ---------------------------------------------------- */}}
{{- $_ := set $cfg.cache.redis "addr" (include "ttl-aware-bff.redisAddr" .) -}}
{{- if and (not .Values.redis.enabled) .Values.externalRedis.host -}}
{{- $_ := set $cfg.cache.redis "password_env" .Values.externalRedis.passwordEnv -}}
{{- end -}}

{{/* --- Observability ---------------------------------------------------- */}}
{{- if .Values.otel.enabled -}}
{{- $obs := $cfg.observability -}}
{{- $_ := set $obs "service_name" .Values.otel.serviceName -}}
{{- $_ := set $obs "environment" .Values.otel.environment -}}
{{- $_ := set $obs "trace_sample_ratio" .Values.otel.traceSampleRatio -}}
{{- $_ := set $obs "metrics_interval" .Values.otel.metricsInterval -}}
{{- $_ := set $obs "log" (dict "level" .Values.otel.log.level "format" .Values.otel.log.format) -}}
{{- $_ := set $obs "otlp" (dict "endpoint" .Values.otel.endpoint "insecure" .Values.otel.insecure "timeout" .Values.otel.timeout) -}}
{{- end -}}
{{/* service_version tracks the image so a metric or span can be attributed to
     a build without cross-referencing the Deployment. */}}
{{- $_ := set $cfg.observability "service_version" (include "ttl-aware-bff.imageTag" .) -}}

{{/* --- Routing and tenant overrides ------------------------------------- */}}
{{- with .Values.routingOverrides -}}
{{- $_ := set $cfg "routing" (mergeOverwrite (deepCopy $cfg.routing) (deepCopy .)) -}}
{{- end -}}
{{- with .Values.tenantOverrides -}}
{{- $_ := set $cfg "tenants" (mergeOverwrite (deepCopy (default (dict) $cfg.tenants)) (deepCopy .)) -}}
{{- end -}}

{{- toYaml $cfg -}}
{{- end -}}

{{/*
OTEL_RESOURCE_ATTRIBUTES: the standard pair plus anything in
otel.resourceAttributes, rendered as the comma-separated k=v list the SDK
expects.
*/}}
{{- define "ttl-aware-bff.otelResourceAttributes" -}}
{{- $parts := list (printf "service.namespace=%s" .Release.Namespace) (printf "deployment.environment=%s" .Values.otel.environment) -}}
{{- range $k, $v := .Values.otel.resourceAttributes -}}
{{- $parts = append $parts (printf "%s=%s" $k (toString $v)) -}}
{{- end -}}
{{- join "," $parts -}}
{{- end -}}
