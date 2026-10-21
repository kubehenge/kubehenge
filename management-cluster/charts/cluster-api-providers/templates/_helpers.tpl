{{- /*
The pod spec of a hook Job that runs a kubehenge subcommand, from the root context, the container's
name and the subcommand's arguments.
*/ -}}
{{- define "cluster-api-providers.hookPod" -}}
{{- $root := index . 0 -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
containers:
  - name: {{ index . 1 }}
    image: {{ $root.Values.image }}
    args:
      {{- range index . 2 }}
      - {{ . | quote }}
      {{- end }}
      - --timeout={{ int $root.Values.waitTimeoutSeconds }}s
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: [ALL]
{{- end -}}
