{{- /*
Rendered by Cluster API, not Helm, with the cluster's variables and $anonymous: the oidc variable
becomes the API server's structured authentication configuration, one JWT authenticator per
provider. Defaults are repeated here because a value set without them reaches the template as
given. $anonymous is where requests without credentials may go. "paths": only the health checks
and the cluster-info ConfigMap, which kubeadm join reads before it has any. "none": nowhere, for
RKE2, which needs none and switches anonymous requests off only while it is given no file. "flag":
the API server's --anonymous-auth decides, for k3s, whose provider always sets it, and the API
server refuses the flag beside an anonymous section.
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
{{- if eq $anonymous "paths" }}
{{- $paths := list "/livez" "/readyz" "/healthz" "/api/v1/namespaces/kube-public/configmaps/cluster-info" }}
{{- $conditions := list }}
{{- range $paths }}{{ $conditions = append $conditions (dict "path" .) }}{{ end }}
{{- $_ := set $config "anonymous" (dict "enabled" true "conditions" $conditions) }}
{{- else if eq $anonymous "none" }}
{{- $_ := set $config "anonymous" (dict "enabled" false) }}
{{- end }}
owner: root:root
permissions: "0600"
content: {{ $config | toJson | toJson }}
