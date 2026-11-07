locals {
  # Only whether there is a proxy is public: the address may hold a password, so the values stay
  # sensitive and no plan prints them.
  proxy_enabled = nonsensitive(var.proxy.url != "")
  proxy_has_ca  = nonsensitive(var.proxy.ca_bundle != "")

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

  # The program-readable spellings, as the Secret that pods take with envFrom holds them.
  proxy_secret_encoded = local.proxy_enabled ? {
    for k, v in {
      HTTP_PROXY  = var.proxy.url
      HTTPS_PROXY = var.proxy.url
      http_proxy  = var.proxy.url
      https_proxy = var.proxy.url
    } : k => base64encode(v)
  } : {}
  # Neither the exceptions nor the authority is secret.
  proxy_secret_plain = local.proxy_enabled ? merge(
    {
      NO_PROXY = nonsensitive(var.proxy.no_proxy)
      no_proxy = nonsensitive(var.proxy.no_proxy)
    },
    local.proxy_has_ca ? { "ca.crt" = nonsensitive(var.proxy.ca_bundle) } : {},
  ) : {}

  # The operator chart's values for the proxy. A for expression, since the three keys have different
  # types and a conditional between them and {} has no common type.
  capi_operator_proxy_values = { for k, v in {
    env = { manager = concat(
      [for name in ["HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"] : {
        name      = name
        valueFrom = { secretKeyRef = { name = local.proxy_secret_name, key = name } }
      }],
      local.proxy_has_ca ? [{ name = "SSL_CERT_FILE", value = local.proxy_ca_path }] : [],
    ) }
    # The chart's own volume holds the webhook certificate, so it is listed again.
    volumes = concat(
      [{ name = "cert", secret = { defaultMode = 420, secretName = "capi-operator-webhook-service-cert" } }],
      local.proxy_has_ca ? [{ name = "proxy-ca", secret = { secretName = local.proxy_secret_name, items = [{ key = "ca.crt", path = "ca.crt" }] } }] : [],
    )
    volumeMounts = { manager = concat(
      [{ name = "cert", mountPath = "/tmp/k8s-webhook-server/serving-certs", readOnly = true }],
      local.proxy_has_ca ? [{ name = "proxy-ca", mountPath = "/etc/kubehenge/proxy", readOnly = true }] : [],
    ) }
  } : k => v if local.proxy_enabled }

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
  count = local.proxy_enabled ? 1 : 0

  name             = "kubehenge-proxy"
  namespace        = "capi-operator-system"
  create_namespace = true
  chart            = "${path.module}/charts/proxy"
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
