module github.com/kubehenge/kubehenge

go 1.27.4

// npm's packages may carry Go code of their own, which is not Kubehenge's to build or lint.
ignore (
	./web/node_modules
	./website/node_modules
)

require (
	charm.land/bubbles/v2 v2.2.1
	charm.land/bubbletea/v2 v2.1.2
	charm.land/huh/v2 v2.0.3
	charm.land/lipgloss/v2 v2.1.1
	filippo.io/age v1.3.2
	github.com/Masterminds/sprig/v3 v3.3.0
	github.com/ProtonMail/go-crypto v1.5.2
	github.com/charmbracelet/x/ansi v0.11.9
	github.com/charmbracelet/x/exp/golden v0.1.0
	github.com/charmbracelet/x/exp/teatest/v2 v2.0.0-20261101151554-27ecf3e4d454
	github.com/charmbracelet/x/term v0.2.2
	github.com/cosi-project/runtime v1.17.1
	github.com/diskfs/go-diskfs v1.9.4
	github.com/evanphx/json-patch/v5 v5.9.11
	github.com/go-acme/lego/v5 v5.6.0
	github.com/go-logr/logr v1.4.4
	github.com/google/cel-go v0.31.0
	github.com/google/go-cmp v0.7.0
	github.com/letsencrypt/pebble/v2 v2.10.3
	github.com/luthermonson/go-proxmox v0.10.0
	github.com/mdlayher/netlink v1.11.2
	github.com/miekg/dns v1.1.73
	github.com/minio/minio-go/v7 v7.4.1
	github.com/prometheus/client_golang v1.25.0
	github.com/prometheus/client_model v0.6.3
	github.com/robfig/cron/v3 v3.0.1
	github.com/siderolabs/talos/pkg/machinery v1.14.6
	github.com/smallstep/pkcs7 v0.2.3
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.1.0
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78
	go.etcd.io/bbolt v1.5.0
	go.etcd.io/etcd/api/v3 v3.7.3
	go.etcd.io/etcd/client/v3 v3.7.3
	go.uber.org/zap v1.28.0
	golang.org/x/crypto v0.59.0
	golang.org/x/net v0.63.0
	golang.org/x/sys v0.50.0
	golang.org/x/text v0.44.0
	gomodules.xyz/jsonpatch/v2 v2.5.0
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/api v0.37.2
	k8s.io/apiextensions-apiserver v0.37.2
	k8s.io/apimachinery v0.37.2
	k8s.io/apiserver v0.37.2
	k8s.io/client-go v0.37.2
	k8s.io/kubectl v0.37.2
	k8s.io/pod-security-admission v0.37.2
	k8s.io/streaming v0.37.2
	k8s.io/utils v0.0.0-20260626114624-be93311217bd
	sigs.k8s.io/cluster-api v1.14.4
	sigs.k8s.io/cluster-api/api v1.14.4
	// release-0.25 with the fix that lets envtest build for Windows, which no v0.25 release carries yet.
	sigs.k8s.io/controller-runtime v0.25.3
	sigs.k8s.io/yaml v1.6.0
	software.sslmate.com/src/go-pkcs12 v0.7.4
)

require (
	cel.dev/expr v0.26.0 // indirect
	cloud.google.com/go/auth v0.24.1-0.20261001053825-dbc26066f70a // indirect
	cloud.google.com/go/auth/oauth2adapt v0.3.0 // indirect
	cloud.google.com/go/compute/metadata v0.10.0 // indirect
	dario.cat/mergo v1.0.2 // indirect
	filippo.io/hpke v0.4.0 // indirect
	github.com/AdamSLevy/jsonrpc2/v14 v14.1.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.23.3 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.15.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.13.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns v1.2.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns/v2 v2.0.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph v0.10.0 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20261013165250-51075c6ea6bc // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.10.1 // indirect
	github.com/MakeNowJust/heredoc v1.0.0 // indirect
	github.com/Masterminds/goutils v1.1.1 // indirect
	github.com/Masterminds/semver/v3 v3.5.0 // indirect
	github.com/Myra-Security-GmbH/myrasec-go/v2 v2.62.1 // indirect
	github.com/Myra-Security-GmbH/signature v1.1.0 // indirect
	github.com/ProtonMail/gopenpgp/v3 v3.5.2 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/akamai/AkamaiOPEN-edgegrid-golang/v14 v14.1.0 // indirect
	github.com/alexbrainman/sspi v0.0.0-20250919150558-7d374ff0d59e // indirect
	github.com/alibabacloud-go/alibabacloud-gateway-spi v0.0.5 // indirect
	github.com/alibabacloud-go/darabonba-openapi/v2 v2.2.4 // indirect
	github.com/alibabacloud-go/debug v1.0.1 // indirect
	github.com/alibabacloud-go/tea v1.5.3 // indirect
	github.com/alibabacloud-go/tea-utils/v2 v2.0.9 // indirect
	github.com/aliyun/credentials-go v1.4.7 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aws/aws-sdk-go-v2 v1.47.5 // indirect
	github.com/aws/aws-sdk-go-v2/config v1.33.12 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.12 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/lightsail v1.69.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/route53 v1.70.6 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.12.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.6 // indirect
	github.com/aws/smithy-go v1.28.7 // indirect
	github.com/aymanbagabas/go-udiff v0.4.2 // indirect
	github.com/aziontech/azionapi-go-sdk v0.148.0 // indirect
	github.com/baidubce/bce-sdk-go v0.9.277 // indirect
	github.com/benbjohnson/clock v1.3.5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/bodgit/gssapi v0.0.4 // indirect
	github.com/bodgit/tsig v1.3.1 // indirect
	github.com/boombuler/barcode v1.0.1 // indirect
	github.com/buger/goterm v1.0.4 // indirect
	github.com/catppuccin/go v0.3.0 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/chai2010/gettext-go v1.0.3 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/harmonica v0.2.0 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20261023060817-a45b7c86333f // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/exp/strings v0.1.0 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clbanning/mxj/v2 v2.7.0 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cloudflare/circl v1.6.5 // indirect
	github.com/containerd/go-cni v1.1.15 // indirect
	github.com/containernetworking/cni v1.3.1 // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/coreos/go-systemd/v22 v22.7.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/djherbis/times v1.6.0 // indirect
	github.com/dnsimple/dnsimple-go/v10 v10.0.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/emicklei/go-restful/v3 v3.14.0 // indirect
	github.com/evanphx/json-patch v5.9.11+incompatible // indirect
	github.com/exoscale/egoscale/v3 v3.1.61 // indirect
	github.com/exponent-io/jsonpath v0.0.0-20210407135951-1de76d718b3f // indirect
	github.com/fatih/structs v1.1.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/fxamacker/cbor/v2 v2.10.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.16 // indirect
	github.com/gertd/go-pluralize v0.2.1 // indirect
	github.com/ghodss/yaml v1.0.0 // indirect
	github.com/go-acme/alidns-20150109/v5 v5.6.1 // indirect
	github.com/go-acme/esa-20240910/v3 v3.14.0 // indirect
	github.com/go-acme/jdcloud-sdk-go v1.64.0 // indirect
	github.com/go-acme/tencentclouddnspod v1.3.131 // indirect
	github.com/go-acme/tencentedgdeone v1.3.186 // indirect
	github.com/go-errors/errors v1.5.1 // indirect
	github.com/go-jose/go-jose/v4 v4.1.8 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-logr/zapr v1.3.0 // indirect
	github.com/go-openapi/jsonpointer v1.0.3 // indirect
	github.com/go-openapi/jsonreference v1.0.3 // indirect
	github.com/go-openapi/swag v0.29.2 // indirect
	github.com/go-openapi/swag/cmdutils v0.29.2 // indirect
	github.com/go-openapi/swag/conv v0.29.2 // indirect
	github.com/go-openapi/swag/fileutils v0.29.2 // indirect
	github.com/go-openapi/swag/jsonutils v0.29.2 // indirect
	github.com/go-openapi/swag/loading v0.29.2 // indirect
	github.com/go-openapi/swag/mangling v0.29.2 // indirect
	github.com/go-openapi/swag/netutils v0.29.2 // indirect
	github.com/go-openapi/swag/pools v0.29.2 // indirect
	github.com/go-openapi/swag/stringutils v0.29.2 // indirect
	github.com/go-openapi/swag/typeutils v0.29.2 // indirect
	github.com/go-openapi/swag/yamlutils v0.29.2 // indirect
	github.com/go-ozzo/ozzo-validation/v4 v4.5.0 // indirect
	github.com/go-playground/locales v0.14.2 // indirect
	github.com/go-playground/universal-translator v0.18.2 // indirect
	github.com/go-playground/validator/v10 v10.30.5 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gobuffalo/flect v1.0.3 // indirect
	github.com/goccy/go-yaml v1.19.2 // indirect
	github.com/gofrs/flock v0.13.1 // indirect
	github.com/gofrs/uuid v4.4.0+incompatible // indirect
	github.com/gofrs/uuid/v5 v5.5.1 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/gnostic-models v0.7.1 // indirect
	github.com/google/go-containerregistry v0.22.1 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/google/s2a-go v0.1.11 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.24 // indirect
	github.com/googleapis/gax-go/v2 v2.26.2 // indirect
	github.com/gophercloud/gophercloud v1.14.1 // indirect
	github.com/gophercloud/gophercloud/v2 v2.15.0 // indirect
	github.com/gophercloud/utils/v2 v2.0.0-20260922174841-b4759ea0529b // indirect
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.34.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/hashicorp/go-uuid v1.0.4 // indirect
	github.com/huandu/xstrings v1.6.3 // indirect
	github.com/huaweicloud/huaweicloud-sdk-go-v3 v0.1.220 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/infobloxopen/infoblox-go-client/v2 v2.12.0 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/goidentity/v6 v6.0.1 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/jinzhu/copier v0.4.0 // indirect
	github.com/jsimonetti/rtnetlink/v2 v2.3.0 // indirect
	github.com/json-iterator/go v1.1.13-0.20220915233716-71ac16282d12 // indirect
	github.com/k0kubun/go-ansi v0.0.0-20180517002512-3bf9e2903213 // indirect
	github.com/klauspost/compress v1.20.4 // indirect
	github.com/klauspost/cpuid/v2 v2.4.2 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/kolo/xmlrpc v0.0.0-20220921171641-a4b6fa1dd06b // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/labbsr0x/bindman-dns-webhook v1.0.2 // indirect
	github.com/labbsr0x/goh v1.0.1 // indirect
	github.com/leodido/go-urn v1.5.0 // indirect
	github.com/letsencrypt/challtestsrv v1.4.2 // indirect
	github.com/liggitt/tabwriter v0.0.0-20181228230101-89fcab3d43de // indirect
	github.com/linode/linodego/v2 v2.10.0 // indirect
	github.com/liquidweb/liquidweb-cli v0.6.10 // indirect
	github.com/liquidweb/liquidweb-go v1.6.4 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/magefile/mage v1.17.2 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.31 // indirect
	github.com/mdlayher/ethtool v0.7.0 // indirect
	github.com/mdlayher/genetlink v1.4.0 // indirect
	github.com/mdlayher/socket v0.8.0 // indirect
	github.com/mimuret/golang-iij-dpf v0.9.1 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/mitchellh/hashstructure/v2 v2.0.2 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/moby/spdystream v0.5.1 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/monochromegane/go-gitignore v0.0.0-20200626010858-205db1a8cc00 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/namedotcom/go/v4 v4.0.2 // indirect
	github.com/nrdcg/auroradns v1.2.0 // indirect
	github.com/nrdcg/bunny-go v0.1.0 // indirect
	github.com/nrdcg/desec v0.11.3 // indirect
	github.com/nrdcg/freemyip v0.3.0 // indirect
	github.com/nrdcg/goacmedns v0.2.0 // indirect
	github.com/nrdcg/goinwx v0.12.0 // indirect
	github.com/nrdcg/mailinabox v0.3.0 // indirect
	github.com/nrdcg/namesilo v0.5.0 // indirect
	github.com/nrdcg/nodion v0.1.0 // indirect
	github.com/nrdcg/oci-go-sdk/common/v1065 v1065.128.0 // indirect
	github.com/nrdcg/oci-go-sdk/dns/v1065 v1065.128.0 // indirect
	github.com/nrdcg/porkbun v0.4.0 // indirect
	github.com/nrdcg/vegadns v0.3.0 // indirect
	github.com/nzdjb/go-metaname v1.0.0 // indirect
	github.com/onsi/gomega v1.44.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/runtime-spec v1.3.0 // indirect
	github.com/openshift/gssapi v0.0.0-20161010215902-5fb4217df13b // indirect
	github.com/ovh/go-ovh v1.9.0 // indirect
	github.com/pandatix/go-abnf v0.5.5 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/peterbourgon/diskv v2.0.1+incompatible // indirect
	github.com/peterhellberg/link v1.2.0 // indirect
	github.com/petermattis/goid v0.0.0-20261109124353-7cfa343ad0ab // indirect
	github.com/philhofer/fwd v1.2.1 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20260702190614-8ae5a48058df // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/pquerna/otp v1.5.0 // indirect
	github.com/prometheus/common v0.74.0 // indirect
	github.com/prometheus/procfs v0.23.0 // indirect
	github.com/regfish/regfish-dnsapi-go v0.2.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/ryanuber/go-glob v1.0.0 // indirect
	github.com/sacloud/sacloud-sdk-go v0.4.0 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sasha-s/go-deadlock v0.3.9 // indirect
	github.com/scaleway/scaleway-sdk-go v1.39.0 // indirect
	github.com/selectel/domains-go v1.1.0 // indirect
	github.com/selectel/go-selvpcclient/v5 v5.0.0 // indirect
	github.com/shopspring/decimal v1.6.0 // indirect
	github.com/siderolabs/crypto v0.6.6 // indirect
	github.com/siderolabs/gen v0.8.8 // indirect
	github.com/siderolabs/go-api-signature v0.4.0 // indirect
	github.com/siderolabs/go-pointer v1.0.1 // indirect
	github.com/siderolabs/net v0.4.0 // indirect
	github.com/siderolabs/protoenc v0.2.4 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/softlayer/softlayer-go v1.2.1 // indirect
	github.com/softlayer/xmlrpc v0.0.0-20200409220501-5f089df7cb7e // indirect
	github.com/sony/gobreaker/v2 v2.4.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/viper v1.21.0 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common v1.3.202 // indirect
	github.com/tinylib/msgp v1.6.5 // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	github.com/transip/gotransip/v6 v6.28.0 // indirect
	github.com/ucloud/ucloud-sdk-go v0.22.141 // indirect
	github.com/ultradns/ultradns-go-sdk v1.8.6-20261009044533-535b847 // indirect
	github.com/vinyldns/go-vinyldns v0.9.18 // indirect
	github.com/volcengine/volc-sdk-golang v1.0.258 // indirect
	github.com/vultr/govultr/v3 v3.33.2 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xlab/treeprint v1.2.0 // indirect
	github.com/xo/terminfo v1.3.1 // indirect
	github.com/yandex-cloud/go-genproto v0.133.0 // indirect
	github.com/yandex-cloud/go-sdk/services/dns v0.0.106 // indirect
	github.com/yandex-cloud/go-sdk/v2 v2.185.0 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.etcd.io/etcd/client/pkg/v3 v3.7.3 // indirect
	go.mongodb.org/mongo-driver v1.17.10 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.1 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/ratelimit v0.3.1 // indirect
	go.yaml.in/yaml/v2 v2.4.5 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	go.yaml.in/yaml/v4 v4.0.0 // indirect
	golang.org/x/exp v0.0.0-20261117202309-7a49493bfd90 // indirect
	golang.org/x/mod v0.43.0 // indirect
	golang.org/x/oauth2 v0.38.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/telemetry v0.0.0-20261116191925-e77a43154acb // indirect
	golang.org/x/term v0.48.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	golang.org/x/tools v0.53.0 // indirect
	golang.org/x/vuln v1.10.0 // indirect
	google.golang.org/api v0.309.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261116203732-fd2c7689a808 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261116203732-fd2c7689a808 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.13.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/ini.v1 v1.68.0 // indirect
	gopkg.in/ns1/ns1-go.v2 v2.18.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	k8s.io/cli-runtime v0.37.2 // indirect
	k8s.io/component-base v0.37.2 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	k8s.io/kube-openapi v0.0.0-20260721132016-d427ff9ee9ad // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.37.1 // indirect
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730 // indirect
	sigs.k8s.io/kustomize/api v0.21.1 // indirect
	sigs.k8s.io/kustomize/kyaml v0.21.1 // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
	sigs.k8s.io/structured-merge-diff/v6 v6.4.2 // indirect
)

tool golang.org/x/vuln/cmd/govulncheck
