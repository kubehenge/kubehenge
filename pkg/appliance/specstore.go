package appliance

import (
	"bytes"
	"context"
	"fmt"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// StoreClusterSpec writes a cluster's exported spec into the target the management cluster's etcd
// is backed up to. Only the appliance holds that target's access key, which is why it writes the
// specs and the controller does not. It answers ErrNoEtcdBackups where there is no target.
func (a *Appliance) StoreClusterSpec(ctx context.Context, namespace, name string, spec []byte) error {
	b, err := a.backupSetup()
	if err != nil {
		return err
	}
	if _, err := b.store.Put(ctx, provisioner.ClusterSpecKey(b.target.Prefix, namespace, name), bytes.NewReader(spec), map[string]string{"content-type": "application/yaml"}); err != nil {
		return fmt.Errorf("storing the spec of %s/%s in target %s: %w", namespace, name, b.target.Name, err)
	}
	return nil
}
