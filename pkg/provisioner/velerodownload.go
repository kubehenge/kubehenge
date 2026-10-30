package provisioner

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 takes an SSE-C key's MD5 as its checksum, never as a hash to trust
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VeleroDownload fetches a file Velero signed a link to in a cluster's backup target, such as a
// restore's results, from the folder of the cluster of: the cluster itself, or one whose backups it
// reads. The store encrypted the file with that cluster's SSE-C key, which a signed link does not
// carry and the request has to; it is sent only to the target's own endpoint.
func (ts *ClusterTargets) VeleroDownload(ctx context.Context, cluster *clusterv1.Cluster, of, link string) (io.ReadCloser, error) {
	b, _, err := optionalVariable[Backups](cluster, backupsVariable)
	if err != nil {
		return nil, err
	}
	target, found, err := classTarget(ctx, ts.Client, ts.ClassNamespace, cluster, b.Target)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("backup target %q is not one an administrator set", b.Target)
	}
	if !target.serves(link) {
		return nil, fmt.Errorf("velero's link does not lead to target %s", target.Name)
	}
	keys := &corev1.Secret{}
	if err = ts.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: BackupKeysSecretName(of)}, keys); err != nil {
		return nil, fmt.Errorf("reading the backup keys of %s: %w", of, err)
	}
	key := keys.Data[BackupKeysKeySSEC]
	if len(key) != sseCKeyBytes {
		return nil, fmt.Errorf("the Secret %s holds no SSE-C key", BackupKeysSecretName(of))
	}
	transport, err := targetTransport(&target)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("velero's link: %w", err)
	}
	sum := md5.Sum(key) //nolint:gosec // as above
	req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256")
	req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key", base64.StdEncoding.EncodeToString(key))
	req.Header.Set("X-Amz-Server-Side-Encryption-Customer-Key-Md5", base64.StdEncoding.EncodeToString(sum[:]))
	// A redirect would take the key along to wherever it leads.
	noRedirects := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := (&http.Client{Transport: transport, CheckRedirect: noRedirects}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching target %s: %w", target.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("target %s: %w", target.Name, ErrNoObject)
		}
		return nil, errors.New("target " + target.Name + " answered " + resp.Status)
	}
	return resp.Body, nil
}
