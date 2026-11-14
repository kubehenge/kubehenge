package provisioner

import "testing"

func TestPoolLabelNeverReadsLikeAUsersPool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		pools []string
		want  string
	}{
		{DefaultPool, []string{DefaultPool, "gpu"}, DefaultPoolLabel},
		{"gpu", []string{DefaultPool, "gpu"}, "gpu"},
		// A user's pool named workers would read like the default pool's label, so both keep their names.
		{DefaultPool, []string{DefaultPool, "workers"}, DefaultPool},
		{"workers", []string{DefaultPool, "workers"}, "workers"},
	} {
		if got := PoolLabel(tc.name, tc.pools); got != tc.want {
			t.Errorf("PoolLabel(%q, %v) = %q, want %q", tc.name, tc.pools, got, tc.want)
		}
	}
}
