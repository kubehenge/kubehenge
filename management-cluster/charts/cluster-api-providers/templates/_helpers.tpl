{{- /*
The pod spec of a hook Job that runs `kubehenge wait`, from the root context and the subcommand's
arguments.
*/ -}}
{{- define "cluster-api-providers.waitPod" -}}
{{- $root := index . 0 -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
containers:
  - name: wait
    image: {{ $root.Values.image }}
    args:
      - wait
      {{- range index . 1 }}
      - {{ . }}
      {{- end }}
      - --timeout={{ int $root.Values.waitTimeoutSeconds }}s
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ALL]
{{- end -}}
