locals {
  # Only whether there is a proxy is public: the address may hold a password, so the values stay
  # sensitive and no plan prints them.
  proxy_enabled = nonsensitive(var.proxy.url != "")
  proxy_has_ca  = nonsensitive(var.proxy.ca_bundle != "")

  # The authorities every outbound connection of the management side trusts beside the public ones:
  # the site CA, which a site behind TLS inspection needs with or without a proxy, then the proxy's.
  outbound_ca        = join("\n", compact([var.site_ca, local.proxy_has_ca ? nonsensitive(var.proxy.ca_bundle) : ""]))
  outbound_has_ca    = local.outbound_ca != ""
  outbound_secret_on = local.proxy_enabled || local.outbound_has_ca

  # The same Secret in the operator's namespace (its own release) and in the controller's, and under
  # a name of its own in the Add-on Provider for Helm's, since the providers chart keys secrets by
  # name alone.
  proxy_secret_name       = "kubehenge-proxy"
  proxy_addon_secret_name = "kubehenge-proxy-addon"
  proxy_secrets = {
    (local.proxy_secret_name)       = local.kubehenge_namespace
    (local.proxy_addon_secret_name) = one([for p in local.providers : p.namespace if p.kind == "AddonProvider"])
  }
  proxy_ca_path = "/etc/kubehenge/proxy/ca.crt"

  # What the charts take for the Secret: its name while there is a proxy, whether it holds
  # authorities, and its name for them alone when there is no proxy.
  proxy_chart = { for k, v in {
    secret   = local.proxy_secret_name
    ca       = local.outbound_has_ca
    caSecret = local.proxy_secret_name
    } : k => v if k == "secret" ? local.proxy_enabled : k == "ca" ? local.outbound_secret_on : local.outbound_has_ca && !local.proxy_enabled
  }

  # The proxy the nodes of a workload cluster without one of its own take, as its add-ons do. An
  # address with credentials, or one the classes would not take, is left out, since node files are
  # plain text. The authority is the certificates alone, as the classes read them.
  cluster_proxy_url = local.proxy_enabled && nonsensitive(can(regex("^https?://([A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?|\\[[0-9A-Fa-f:]+\\])(:[0-9]{1,5})?/?$", var.proxy.url))) ? nonsensitive(var.proxy.url) : ""
  proxy_cluster_default = local.cluster_proxy_url == "" ? {} : {
    clusterDefault = {
      url      = local.cluster_proxy_url
      noProxy  = [for e in split(",", nonsensitive(var.proxy.no_proxy)) : trimspace(e) if can(regex("^[A-Za-z0-9._*:/\\[\\]-]+$", trimspace(e)))]
      caBundle = join("", [for c in regexall("-----BEGIN CERTIFICATE-----[A-Za-z0-9+/=\\s]+-----END CERTIFICATE-----", replace(nonsensitive(var.proxy.ca_bundle), "\r", "")) : "${c}\n"])
    }
  }

  # The Add-on Provider for Helm fetches every chart itself, through the proxy and from the chart
  # mirrors, and trusts the authorities of all from one file. It gets the Secret for a mirror alone too.
  addon_namespace = one([for p in local.providers : p.namespace if p.kind == "AddonProvider"])
  addon_ca        = join("\n", compact([local.outbound_ca, var.chart_repositories.ca_bundle]))
  addon_has_ca    = local.addon_ca != ""
  addon_secret_on = local.proxy_enabled || local.addon_has_ca
  addon_secret_plain = merge(
    local.proxy_enabled ? { NO_PROXY = nonsensitive(var.proxy.no_proxy), no_proxy = nonsensitive(var.proxy.no_proxy) } : {},
    local.addon_has_ca ? { "ca.crt" = local.addon_ca } : {},
  )

  # The program-readable spellings, as the Secret that pods take with envFrom holds them.
  proxy_secret_encoded = local.proxy_enabled ? {
    for k, v in {
      HTTP_PROXY  = var.proxy.url
      HTTPS_PROXY = var.proxy.url
      http_proxy  = var.proxy.url
      https_proxy = var.proxy.url
    } : k => base64encode(v)
  } : {}
  # Neither the exceptions nor the authorities are secret. The Secret holds the authorities alone
  # where there is no proxy.
  proxy_secret_plain = merge(
    local.proxy_enabled ? {
      NO_PROXY = nonsensitive(var.proxy.no_proxy)
      no_proxy = nonsensitive(var.proxy.no_proxy)
    } : {},
    local.outbound_has_ca ? { "ca.crt" = local.outbound_ca } : {},
  )

  # The annotation that rolls a Deployment whose program reads the authorities once, as it starts,
  # when they change.
  trusted_ca_annotation = "kubehenge.dev/trusted-ca"

  # The operator chart's values for the proxy and the authorities. A for expression, since the keys
  # have different types and a conditional between them and {} has no common type.
  capi_operator_proxy_values = { for k, v in {
    podAnnotations = { (local.trusted_ca_annotation) = sha256(local.outbound_ca) }
    env = { manager = concat(
      local.proxy_enabled ? [for name in ["HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"] : {
        name      = name
        valueFrom = { secretKeyRef = { name = local.proxy_secret_name, key = name } }
      }] : [],
      local.outbound_has_ca ? [{ name = "SSL_CERT_FILE", value = local.proxy_ca_path }] : [],
    ) }
    # The chart's own volume holds the webhook certificate, so it is listed again.
    volumes = concat(
      [{ name = "cert", secret = { defaultMode = 420, secretName = "capi-operator-webhook-service-cert" } }],
      local.outbound_has_ca ? [{ name = "proxy-ca", secret = { secretName = local.proxy_secret_name, items = [{ key = "ca.crt", path = "ca.crt" }] } }] : [],
    )
    volumeMounts = { manager = concat(
      [{ name = "cert", mountPath = "/tmp/k8s-webhook-server/serving-certs", readOnly = true }],
      local.outbound_has_ca ? [{ name = "proxy-ca", mountPath = "/etc/kubehenge/proxy", readOnly = true }] : [],
    ) }
  } : k => v if local.outbound_secret_on && (k != "podAnnotations" || local.outbound_has_ca) }

  # What Talos puts in its services' environment, containerd's image pulls and the kubelet's among
  # them. The values stay sensitive until they are in the machine configuration.
  proxy_talos_env = local.proxy_enabled ? {
    HTTP_PROXY  = var.proxy.url
    HTTPS_PROXY = var.proxy.url
    NO_PROXY    = var.proxy.no_proxy
  } : {}
}

# The Secret the Cluster API operator's own Deployment reads, in a namespace of its own chart, since
# the operator fetches every provider's files and must reach the proxy before any provider exists.
resource "helm_release" "proxy_operator" {
  count = local.outbound_secret_on ? 1 : 0

  name             = "kubehenge-proxy"
  namespace        = "capi-operator-system"
  create_namespace = true
  chart            = "${path.module}/charts/proxy"
  replace          = true
  wait             = true

  values = [yamlencode({
    chartDigest = local.chart_digests.proxy
    secret      = { name = local.proxy_secret_name }
    data        = local.proxy_secret_plain
  })]

  set_sensitive = [for k, v in local.proxy_secret_encoded : {
    name  = "encodedData.${k}"
    value = v
    type  = "string"
  }]

  depends_on = [data.talos_cluster_health.this]
}
