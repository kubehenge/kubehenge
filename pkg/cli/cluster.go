package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// interfaceTimeout bounds each request to the interface.
const interfaceTimeout = 2 * time.Minute

// clusterOptions reach Kubehenge's interface as the Proxmox VE API token's user.
type clusterOptions struct {
	url, caFile, namespace, output string
	token                          string
}

func newClusterCommand(conn *pve.Config) *cobra.Command {
	o := &clusterOptions{token: conn.Token}
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "List, create, scale and delete clusters through Kubehenge's interface",
		Long: `List, create, scale and delete clusters through Kubehenge's interface.

They act as the user of the Proxmox VE API token in KUBEHENGE_PROXMOX_TOKEN (user@realm!name=secret),
with that token's privileges on the clusters' pools, as the interface does for a signed-in user.`,
	}
	flags := cmd.PersistentFlags()
	flags.StringVar(&o.url, "url", os.Getenv("KUBEHENGE_URL"), "Kubehenge's interface, such as https://kubehenge.example.org (env KUBEHENGE_URL)")
	flags.StringVar(&o.caFile, "ca", os.Getenv("KUBEHENGE_CA"),
		"PEM file with the CA of the interface's certificate; default the system trust store (env KUBEHENGE_CA)")
	flags.StringVarP(&o.namespace, "namespace", "n", "", "the cluster's namespace; default the only cluster of that name")

	list := &cobra.Command{
		Use:   "list",
		Short: "List the clusters the token's user may see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.list(cmd.Context(), cmd.OutOrStdout())
		},
	}
	list.Flags().StringVarP(&o.output, "output", "o", "", "json for the interface's whole answer")

	get := &cobra.Command{
		Use:   "get NAME",
		Short: "Print a cluster as the interface describes it, in JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.raw(cmd.Context(), cmd.OutOrStdout(), http.MethodGet, args[0], "")
		},
	}
	kubeconfig := &cobra.Command{
		Use:   "kubeconfig NAME",
		Short: "Print a cluster's admin kubeconfig, which needs Sys.Modify on its pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.raw(cmd.Context(), cmd.OutOrStdout(), http.MethodGet, args[0], "/kubeconfig")
		},
	}

	var file string
	create := &cobra.Command{
		Use:   "create -f FILE",
		Short: "Create a cluster from the interface's create request, in JSON or YAML",
		Long: `Create a cluster from the interface's create request, in JSON or YAML.

The request is what the interface's wizard sends: name, class, version, controlPlaneReplicas,
workerReplicas and variables. The API server checks it as it checks one from the interface.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.create(cmd.Context(), cmd.OutOrStdout(), file)
		},
	}
	create.Flags().StringVarP(&file, "filename", "f", "", "the create request; - reads standard input")
	_ = create.MarkFlagRequired("filename")

	var controlPlanes int32
	var workers []string
	scale := &cobra.Command{
		Use:   "scale NAME",
		Short: "Set a cluster's control-plane nodes or its pools' workers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var planes *int32
			if cmd.Flags().Changed("control-planes") {
				planes = &controlPlanes
			}
			req, err := scaleRequest(planes, workers)
			if err != nil {
				return err
			}
			return o.change(cmd.Context(), cmd.OutOrStdout(), args[0], req)
		},
	}
	scale.Flags().Int32Var(&controlPlanes, "control-planes", 0, "control-plane nodes: 1, 3, 5 or 7")
	scale.Flags().StringArrayVar(&workers, "workers", nil, "a pool's workers as POOL=N, such as md-0=3; repeat for more pools")

	var yes bool
	deleteCmd := &cobra.Command{
		Use:   "delete NAME --yes",
		Short: "Delete a cluster, its VMs and its persistent volumes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("deleting a cluster cannot be undone: add --yes")
			}
			return o.delete(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	deleteCmd.Flags().BoolVar(&yes, "yes", false, "delete without asking")

	cmd.AddCommand(list, get, kubeconfig, create, scale, deleteCmd, o.machineCommand())
	return cmd
}

// machineAction is one of what a machine's row offers in the interface.
type machineAction struct {
	use, short, method, tail, done string
	// undoable is false for an action that takes the machine's VM and disks, which asks for --yes.
	undoable bool
}

var machineActions = []machineAction{
	{"replace", "Delete the machine's VM and disks and have a new one made in its place", http.MethodPut, "/replacement", "replacing", false},
	{"keep", "Take back a replacement that has not begun", http.MethodDelete, "/replacement", "kept", true},
	{"delete", "Drain and delete a worker, its pool shrinking by one", http.MethodDelete, "", "deleting", false},
	{"cordon", "Keep new pods off the machine's node", http.MethodPut, "/cordon", "cordoned", true},
	{"uncordon", "Let pods onto the machine's node again", http.MethodDelete, "/cordon", "uncordoned", true},
	{"reboot", "Shut the machine's VM down and start it again, which needs VM.PowerMgmt", http.MethodPost, "/reboot", "rebooting", true},
}

func (o *clusterOptions) machineCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "machine", Short: "Act on one of a cluster's machines, by the name get lists it under"}
	for _, a := range machineActions {
		var yes bool
		sub := &cobra.Command{
			Use:   a.use + " CLUSTER MACHINE",
			Short: a.short,
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				if !a.undoable && !yes {
					return fmt.Errorf("%s takes the machine's VM and disks: add --yes", a.use)
				}
				path, err := o.clusterPath(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if _, err := o.call(cmd.Context(), a.method, path+"/machines/"+url.PathEscape(args[1])+a.tail, nil); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "machine %s %s\n", args[1], a.done)
				return nil
			},
		}
		if !a.undoable {
			sub.Flags().BoolVar(&yes, "yes", false, a.use+" without asking")
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

// listedCluster is what list shows of the interface's description of a cluster.
type listedCluster struct {
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Distribution string   `json:"distribution"`
	Version      string   `json:"version"`
	Phase        string   `json:"phase"`
	ControlPlane replicas `json:"controlPlane"`
	Workers      replicas `json:"workers"`
}

type replicas struct {
	Desired int32 `json:"desired"`
	Ready   int32 `json:"ready"`
}

func (o *clusterOptions) list(ctx context.Context, out io.Writer) error {
	body, err := o.call(ctx, http.MethodGet, "/api/clusters", nil)
	if err != nil {
		return err
	}
	if o.output == "json" {
		return writeIndented(out, body)
	}
	if o.output != "" {
		return fmt.Errorf("--output is json or nothing, not %q", o.output)
	}
	var clusters []listedCluster
	if err := json.Unmarshal(body, &clusters); err != nil {
		return fmt.Errorf("reading the clusters: %w", err)
	}
	w := tabwriter.NewWriter(out, 0, 0, columnGap, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAMESPACE\tNAME\tDISTRIBUTION\tVERSION\tSTATUS\tCONTROL PLANES\tWORKERS")
	for _, c := range clusters {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%d/%d\n", c.Namespace, c.Name, c.Distribution, c.Version, c.Phase,
			c.ControlPlane.Ready, c.ControlPlane.Desired, c.Workers.Ready, c.Workers.Desired)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing the clusters: %w", err)
	}
	return nil
}

// raw prints the interface's answer about the cluster named, JSON indented and anything else as it is.
func (o *clusterOptions) raw(ctx context.Context, out io.Writer, method, name, tail string) error {
	path, err := o.clusterPath(ctx, name)
	if err != nil {
		return err
	}
	body, err := o.call(ctx, method, path+tail, nil)
	if err != nil {
		return err
	}
	if json.Valid(body) {
		return writeIndented(out, body)
	}
	if _, err := out.Write(body); err != nil {
		return fmt.Errorf("writing the answer: %w", err)
	}
	return nil
}

func (o *clusterOptions) create(ctx context.Context, out io.Writer, file string) error {
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(file) //nolint:gosec // the file the user names
	}
	if err != nil {
		return fmt.Errorf("reading the create request: %w", err)
	}
	req, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return fmt.Errorf("the create request is neither JSON nor YAML: %w", err)
	}
	body, err := o.call(ctx, http.MethodPost, "/api/clusters", json.RawMessage(req))
	if err != nil {
		return err
	}
	var made listedCluster
	if err := json.Unmarshal(body, &made); err != nil || made.Name == "" {
		_, _ = fmt.Fprintln(out, "cluster created")
		return nil //nolint:nilerr // the cluster was created; only its description did not read.
	}
	_, _ = fmt.Fprintf(out, "cluster %s/%s created\n", made.Namespace, made.Name)
	return nil
}

// scaleRequest is the interface's change for scale's flags.
func scaleRequest(controlPlanes *int32, workers []string) (map[string]any, error) {
	req := map[string]any{}
	if controlPlanes != nil {
		req["controlPlaneReplicas"] = *controlPlanes
	}
	if len(workers) > 0 {
		pools := map[string]int32{}
		for _, w := range workers {
			pool, n, ok := strings.Cut(w, "=")
			count, err := strconv.ParseInt(n, 10, 32)
			if !ok || pool == "" || err != nil {
				return nil, fmt.Errorf("--workers %q: give POOL=N, such as md-0=3", w)
			}
			pools[pool] = int32(count)
		}
		req["workerPools"] = pools
	}
	if len(req) == 0 {
		return nil, errors.New("give --control-planes, --workers or both")
	}
	return req, nil
}

func (o *clusterOptions) change(ctx context.Context, out io.Writer, name string, req map[string]any) error {
	path, err := o.clusterPath(ctx, name)
	if err != nil {
		return err
	}
	if _, err := o.call(ctx, http.MethodPatch, path, req); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "cluster %s changed\n", name)
	return nil
}

func (o *clusterOptions) delete(ctx context.Context, out io.Writer, name string) error {
	path, err := o.clusterPath(ctx, name)
	if err != nil {
		return err
	}
	if _, err := o.call(ctx, http.MethodDelete, path, map[string]string{"confirm": name}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "cluster %s deleting\n", name)
	return nil
}

// clusterPath is the interface's path of the cluster named, in --namespace or, without it, the
// only cluster the user sees by that name.
func (o *clusterOptions) clusterPath(ctx context.Context, name string) (string, error) {
	namespace := o.namespace
	if namespace == "" {
		body, err := o.call(ctx, http.MethodGet, "/api/clusters", nil)
		if err != nil {
			return "", err
		}
		var clusters []listedCluster
		if err := json.Unmarshal(body, &clusters); err != nil {
			return "", fmt.Errorf("reading the clusters: %w", err)
		}
		var found []string
		for _, c := range clusters {
			if c.Name == name {
				found = append(found, c.Namespace)
			}
		}
		switch len(found) {
		case 0:
			return "", fmt.Errorf("no cluster %s", name)
		case 1:
			namespace = found[0]
		default:
			return "", fmt.Errorf("clusters named %s are in %s: give --namespace", name, strings.Join(found, ", "))
		}
	}
	return "/api/clusters/" + url.PathEscape(namespace) + "/" + url.PathEscape(name), nil
}

// call sends a request to the interface as the token's user and returns the answer's body, or the
// interface's error.
func (o *clusterOptions) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	if o.url == "" {
		return nil, errors.New("give the interface's address with --url or KUBEHENGE_URL")
	}
	if o.token == "" {
		return nil, errors.New("set KUBEHENGE_PROXMOX_TOKEN to a Proxmox VE API token, user@realm!name=secret")
	}
	client, err := o.httpClient()
	if err != nil {
		return nil, err
	}
	var payload io.Reader = http.NoBody
	if body != nil {
		raw, merr := json.Marshal(body)
		if merr != nil {
			return nil, fmt.Errorf("encoding the request: %w", merr)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(o.url, "/")+path, payload)
	if err != nil {
		return nil, fmt.Errorf("the interface's address: %w", err)
	}
	req.Header.Set("Authorization", "PVEAPIToken="+o.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the interface: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	answer, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the interface's answer: %w", err)
	}
	if res.StatusCode >= http.StatusBadRequest {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(answer, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s (%d)", e.Error, res.StatusCode)
		}
		return nil, fmt.Errorf("the interface answered %s", res.Status)
	}
	return answer, nil
}

func (o *clusterOptions) httpClient() (*http.Client, error) {
	defaults, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport has been replaced by one that cannot be cloned")
	}
	transport := defaults.Clone()
	if o.caFile != "" {
		pem, err := os.ReadFile(o.caFile)
		if err != nil {
			return nil, fmt.Errorf("--ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--ca: no certificate in %s", o.caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: interfaceTimeout}, nil
}

func writeIndented(out io.Writer, body []byte) error {
	var b bytes.Buffer
	if err := json.Indent(&b, body, "", "  "); err != nil {
		return fmt.Errorf("the interface's answer: %w", err)
	}
	b.WriteByte('\n')
	if _, err := b.WriteTo(out); err != nil {
		return fmt.Errorf("writing the answer: %w", err)
	}
	return nil
}
