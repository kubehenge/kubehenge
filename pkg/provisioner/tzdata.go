package provisioner

import (
	// The controller's image has no time zone database, and upgrade windows name zones.
	_ "time/tzdata"
)
