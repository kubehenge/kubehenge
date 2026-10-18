{{- /*
Rendered by Cluster API, not Helm, with the cluster's variables and $anonymous: the oidc variable
becomes the API server's structured authentication configuration, one JWT authenticator per
provider. Defaults are repeated here because a value set without them reaches the template as
given. With $anonymous, requests without credentials reach only the health checks and the
cluster-info ConfigMap, which kubeadm join reads before it has any. Without, they reach nothing:
RKE2 and k3s, which need none, switch anonymous requests off only while they are given no file.
*/ -}}
{{- $jwt := list }}
{{- range .oidc }}
{{- $issuer := dict "url" .issuerURL "audiences" (list .clientID) }}
{{- if .certificateAuthority }}
{{- $_ := set $issuer "certificateAuthority" .certificateAuthority }}
{{- end }}
{{- $userPrefix := "oidc:" }}
{{- if hasKey . "usernamePrefix" }}{{ $userPrefix = .usernamePrefix }}{{ end }}
{{- $groupPrefix := "oidc:" }}
{{- if hasKey . "groupsPrefix" }}{{ $groupPrefix = .groupsPrefix }}{{ end }}
{{- $claims := dict
  "username" (dict "claim" (default "email" .usernameClaim) "prefix" $userPrefix)
  "groups" (dict "claim" (default "groups" .groupsClaim) "prefix" $groupPrefix) }}
{{- $jwt = append $jwt (dict "issuer" $issuer "claimMappings" $claims) }}
{{- end }}
{{- $config := dict "apiVersion" "apiserver.config.k8s.io/v1" "kind" "AuthenticationConfiguration" "jwt" $jwt }}
{{- if $anonymous }}
{{- $paths := list "/livez" "/readyz" "/healthz" "/api/v1/namespaces/kube-public/configmaps/cluster-info" }}
{{- $conditions := list }}
{{- range $paths }}{{ $conditions = append $conditions (dict "path" .) }}{{ end }}
{{- $_ := set $config "anonymous" (dict "enabled" true "conditions" $conditions) }}
{{- else }}
{{- $_ := set $config "anonymous" (dict "enabled" false) }}
{{- end }}
owner: root:root
permissions: "0600"
content: {{ $config | toJson | toJson }}
