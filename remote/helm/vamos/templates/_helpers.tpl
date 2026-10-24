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

{{/* The vamos image. */}}
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

{{/* The registry settings volume and its mount, which every vamos process reads as $CONTAINERS_REGISTRIES_CONF. */}}
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
{{- end }}

{{/* An emptyDir's fields from medium and sizeLimit values, or {} for neither. */}}
{{- define "vamos.emptyDir" -}}
{{- $d := dict }}
{{- with .medium }}{{ $_ := set $d "medium" . }}{{ end }}
{{- with .sizeLimit }}{{ $_ := set $d "sizeLimit" . }}{{ end }}
{{- toYaml $d }}
{{- end }}
