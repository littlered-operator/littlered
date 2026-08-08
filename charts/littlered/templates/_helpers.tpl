{{/*
Expand the name of the chart.
*/}}
{{- define "littlered.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "littlered.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "littlered.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "littlered.labels" -}}
helm.sh/chart: {{ include "littlered.chart" . }}
{{ include "littlered.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "littlered.selectorLabels" -}}
app.kubernetes.io/name: {{ include "littlered.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "littlered.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "littlered.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Namespace-scoping mode (ADR-014). Returns exactly "allow", "deny", or "none":
- allow: scope.watchNamespaces set        -> least-privilege namespaced RBAC
- deny:  scope.ignoreNamespaces set        -> cluster-wide RBAC (watch all-but)
- none:  neither set (default)             -> cluster-wide RBAC (watch all)
Both lists set is a fatal render error (mirrors the operator's fail-fast on
WATCH_NAMESPACE + IGNORE_NAMESPACE both set).
*/}}
{{- define "littlered.scopeMode" -}}
{{- $watch := .Values.scope.watchNamespaces | default list -}}
{{- $ignore := .Values.scope.ignoreNamespaces | default list -}}
{{- if and (gt (len $watch) 0) (gt (len $ignore) 0) -}}
{{- fail "scope.watchNamespaces and scope.ignoreNamespaces are mutually exclusive" -}}
{{- end -}}
{{- if gt (len $watch) 0 -}}allow{{- else if gt (len $ignore) 0 -}}deny{{- else -}}none{{- end -}}
{{- end }}

{{/*
Manager (reconcile) RBAC rules. Shared VERBATIM between the cluster-scoped
ClusterRole (default / deny-list modes) and the per-namespace Roles (allow-list
mode), so scoping never silently drops a permission. Every rule here is on a
namespaced resource — nothing genuinely cluster-scoped is reconciled (the CRD
itself is cluster-scoped but is installed separately from crds/, not granted
here). Emits the list body under `rules:`; callers set the indentation.
*/}}
{{- define "littlered.managerRules" -}}
- apiGroups:
  - ''
  resources:
  - configmaps
  - services
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - ''
  resources:
  - events
  verbs:
  - create
  - patch
- apiGroups:
  - ''
  resources:
  - pods
  verbs:
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - ''
  resources:
  - secrets
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - apps
  resources:
  - statefulsets
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - monitoring.coreos.com
  resources:
  - servicemonitors
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - policy
  resources:
  - poddisruptionbudgets
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - redis.chuck-chuck-chuck.net
  resources:
  - littlereds
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - redis.chuck-chuck-chuck.net
  resources:
  - littlereds/finalizers
  verbs:
  - update
- apiGroups:
  - redis.chuck-chuck-chuck.net
  resources:
  - littlereds/status
  verbs:
  - get
  - patch
  - update
{{- end }}
