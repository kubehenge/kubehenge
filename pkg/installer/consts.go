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
	vlanMax         = 4094
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
