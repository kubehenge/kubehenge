annotations:
  {{- if .securityGroups }}
  kubehenge.dev/firewall-groups: {{ join "," .securityGroups | quote }}
  {{- end }}
  {{- if and .firewall .firewall.enabled }}
  {{- $ports := list }}
  {{- if or (not (hasKey .firewall "servingPorts")) .firewall.servingPorts }}
  {{- $ports = list "tcp/6443" "tcp/80" "tcp/443" "tcp/30000:32767" "udp/30000:32767" "icmp/echo-request" "ipv6-icmp/echo-request" }}
  {{- $ports = concat $ports (list TALOS_PORTS) }}
  {{- end }}
  {{- range .firewall.ports }}
  {{- $ports = append $ports (printf "%s/%v%s" .protocol .port (ternary (printf ":%v" .endPort) "" (hasKey . "endPort"))) }}
  {{- end }}
  kubehenge.dev/firewall-ports: {{ join "," $ports | quote }}
  {{- end }}
