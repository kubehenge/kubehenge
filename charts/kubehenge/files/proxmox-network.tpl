networkDevices:
  - name: net0
    model: virtio
    bridge: {{ .network.bridge | quote }}
    {{- if .network.vlan }}
    vlan: {{ .network.vlan }}
    {{- end }}
    {{- if .network.mtu }}
    mtu: {{ .network.mtu }}
    linkMtu: {{ .network.mtu }}
    {{- end }}
    {{- if .network.queues }}
    queues: {{ .network.queues }}
    {{- end }}
