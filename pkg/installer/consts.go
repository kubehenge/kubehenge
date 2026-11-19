package installer

import "time"

// Option values that more than one place compares against.
const (
	optYes    = "yes"
	optNone   = "none"
	optDHCP   = "dhcp"
	optAuto   = "auto"
	optStatic = "static"
)

// Step names, as the runner and the screens refer to them.
const (
	stepStartName       = "start"
	stepAddressName     = "address"
	stepNetworkName     = "network"
	stepCertificateName = "certificate"
	stepInstallName     = "install"
	stepUpdatesName     = "updates"
	stepEntryName       = "entry"
)

// The settings the installer asks for or changes, which a failure names when it is the one at fault.
const (
	settingFamily    = "family"
	settingNetwork   = "network"
	settingVLAN      = "vlan"
	settingV4Mode    = "v4mode"
	settingV4Addr    = "v4addr"
	settingV4Gateway = "v4gateway"
	settingV6Mode    = "v6mode"
	settingV6Addr    = "v6addr"
	settingV6Gateway = "v6gateway"
	settingDNS       = "dns"
	settingMTU       = "mtu"
	settingCTID      = "ctid"
	settingHostname  = "hostname"
	settingName      = "name"
	settingCert      = "cert"
	settingStorage   = "storage"
	settingTemplate  = "template"
	settingAPI       = "api"
	settingProxies   = "proxies"
	settingEntry     = "entry"
	settingTrust     = "trust"
	settingAPIHost   = "apihost"
	// settingOutside is an address outside the network's known subnet, which the user may confirm.
	settingOutside = "outside"
)

// netTries is how many times the connection check retries, a pause apart.
const netTries = 15

// File modes.
const (
	permFile = 0o600
	permDir  = 0o700
	permExec = 0o755
)

// Timeouts, limits and sizes.
const (
	probeTimeout    = 15 * time.Second
	certMinValidity = 24 * time.Hour
	serialBits      = 126
	fetchRetries    = 8
	sha512Hex       = 128
	kibPerMiB       = 1024
	kibPerGiB       = 1 << 20
	maxSuggestBits  = 29
	ipv4Bits        = 32
	cleanupTimeout  = 2 * time.Minute
	commandStopWait = 30 * time.Second
	dialTimeout     = 5 * time.Second
	httpTimeout     = 10 * time.Second
	stepPause       = 2 * time.Second
	dfMinLines      = 2
	dfFields        = 4
	defaultWidth    = 80
	defaultHeight   = 24
	barWidth        = 24
	formMargin      = 4
	maxFormWidth    = 76
	listChrome      = 12 // the rows a list's header, title and footer take
	minListRows     = 4
	vlanMax         = 4094
	ctidMin         = 100
	ctidMax         = 999999999
	mtuMin          = 576
	mtuMax          = 65535
	eventBuffer     = 256
	firstLineParts  = 2
	upgradeFactor   = 3
	upgradeSlackKiB = 65536
	dfAvailable     = 3
	certPEMSize     = 1024
)

// Storage ranks: the lower is preferred.
const (
	rankLocal = 1
	rankDir   = 2
	rankNFS   = 3
	rankCIFS  = 4
	rankCeph  = 5
)
