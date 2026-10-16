{{- /*
Rendered by Cluster API, not Helm, with the cluster's variables: the oidc variable becomes the API
server's structured authentication configuration, one JWT authenticator per provider. Defaults are
repeated here because a value set without them reaches the template as given.
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
owner: root:root
permissions: "0600"
content: {{ dict "apiVersion" "apiserver.config.k8s.io/v1" "kind" "AuthenticationConfiguration" "jwt" $jwt | toJson | toJson }}
