package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

const nodeCustomisationVariable = "nodeCustomisation"

// nodeRoles maps the role names of the flags to the ones the cluster variable holds.
var nodeRoles = map[string]string{"control-plane": "controlPlane", "worker": "worker"}

// nodeFileModes are the permissions the cluster variable accepts for a file.
var nodeFileModes = []string{"0400", "0440", "0444", "0600", "0640", "0644", "0700", "0750", "0755"}

// nodeCustomFlags are the repeatable flags that build the cluster variable nodeCustomisation.
type nodeCustomFlags struct {
	files, commands                          []string
	kubeletArgs, apiServerArgs               []string
	controllerManagerArgs, schedulerArgs     []string
	auditPolicyConfigMap, auditWebhookSecret string
}

func (n *nodeCustomFlags) register(f *pflag.FlagSet) {
	f.StringArrayVar(&n.files, "node-file", nil,
		"a file on every node as PATH=SOURCE[,mode=0640][,roles=control-plane|worker], the SOURCE a local file or secret:NAME/KEY, repeated for more")
	f.StringArrayVar(&n.commands, "node-command", nil,
		"a command run once on each node at first boot, optionally prefixed control-plane: or worker:, repeated for more")
	f.StringArrayVar(&n.kubeletArgs, "kubelet-arg", nil, "a kubelet flag as NAME=VALUE, such as event-qps=50, repeated for more")
	f.StringArrayVar(&n.apiServerArgs, "apiserver-arg", nil, "a kube-apiserver flag as NAME=VALUE, repeated for more")
	f.StringArrayVar(&n.controllerManagerArgs, "controller-manager-arg", nil, "a kube-controller-manager flag as NAME=VALUE, repeated for more")
	f.StringArrayVar(&n.schedulerArgs, "scheduler-arg", nil, "a kube-scheduler flag as NAME=VALUE, repeated for more")
	f.StringVar(&n.auditPolicyConfigMap, "audit-policy-configmap", "", "the ConfigMap in the cluster's namespace that holds the audit policy")
	f.StringVar(&n.auditWebhookSecret, "audit-webhook-secret", "", "the Secret NAME[/KEY] that holds the audit webhook's kubeconfig, the key kubeconfig if omitted")
}

func (n *nodeCustomFlags) any() bool {
	return len(n.files)+len(n.commands)+len(n.kubeletArgs)+len(n.apiServerArgs)+len(n.controllerManagerArgs)+len(n.schedulerArgs) > 0 ||
		n.auditPolicyConfigMap != "" || n.auditWebhookSecret != ""
}

// value is the variable's value the flags describe.
func (n *nodeCustomFlags) value() (map[string]any, error) {
	v := map[string]any{}
	if err := n.addLists(v); err != nil {
		return nil, err
	}
	if n.auditPolicyConfigMap != "" {
		v["auditPolicy"] = map[string]any{"configMap": n.auditPolicyConfigMap}
	}
	if n.auditWebhookSecret != "" {
		name, key, _ := strings.Cut(n.auditWebhookSecret, "/")
		if name == "" || strings.Contains(key, "/") {
			return nil, fmt.Errorf("--audit-webhook-secret %q: give NAME or NAME/KEY", n.auditWebhookSecret)
		}
		webhook := map[string]any{"secret": name}
		if key != "" {
			webhook["key"] = key
		}
		v["auditWebhook"] = webhook
	}
	return v, nil
}

func (n *nodeCustomFlags) addLists(v map[string]any) error {
	files := make([]any, 0, len(n.files))
	for _, f := range n.files {
		file, err := parseNodeFile(f)
		if err != nil {
			return err
		}
		files = append(files, file)
	}
	commands := make([]any, 0, len(n.commands))
	for _, c := range n.commands {
		command, err := parseNodeCommand(c)
		if err != nil {
			return err
		}
		commands = append(commands, command)
	}
	lists := []struct {
		flag, field string
		values      []string
	}{
		{"--kubelet-arg", "kubeletArgs", n.kubeletArgs},
		{"--apiserver-arg", "apiServerArgs", n.apiServerArgs},
		{"--controller-manager-arg", "controllerManagerArgs", n.controllerManagerArgs},
		{"--scheduler-arg", "schedulerArgs", n.schedulerArgs},
	}
	for _, l := range lists {
		args := make([]any, 0, len(l.values))
		for _, s := range l.values {
			name, value, ok := strings.Cut(s, "=")
			if !ok || name == "" {
				return fmt.Errorf("%s %q: give NAME=VALUE, such as event-qps=50", l.flag, s)
			}
			args = append(args, map[string]any{"name": name, "value": value})
		}
		if len(args) > 0 {
			v[l.field] = args
		}
	}
	if len(files) > 0 {
		v["files"] = files
	}
	if len(commands) > 0 {
		v["commands"] = commands
	}
	return nil
}

// parseNodeRoles reads control-plane, worker or both joined by |.
func parseNodeRoles(s string) ([]any, error) {
	var roles []any
	for r := range strings.SplitSeq(s, "|") {
		role, ok := nodeRoles[r]
		if !ok {
			return nil, fmt.Errorf("the role %q is neither control-plane nor worker", r)
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// parseNodeFile reads PATH=SOURCE[,mode=MODE][,roles=ROLES].
func parseNodeFile(s string) (map[string]any, error) {
	const usage = "give PATH=SOURCE[,mode=0640][,roles=control-plane|worker]"
	path, rest, ok := strings.Cut(s, "=")
	if !ok || path == "" || rest == "" {
		return nil, fmt.Errorf("--node-file %q: %s", s, usage)
	}
	parts := strings.Split(rest, ",")
	file := map[string]any{"path": path}
	source := parts[0]
	if secret, found := strings.CutPrefix(source, "secret:"); found {
		name, key, valid := strings.Cut(secret, "/")
		if !valid || name == "" || key == "" || strings.Contains(key, "/") {
			return nil, fmt.Errorf("--node-file %q: a Secret source is secret:NAME/KEY", s)
		}
		file["secret"] = map[string]any{"name": name, "key": key}
	} else {
		content, err := os.ReadFile(source) //nolint:gosec // the file the user names
		if err != nil {
			return nil, fmt.Errorf("--node-file %q: reading the source: %w", s, err)
		}
		file["content"] = string(content)
	}
	for _, opt := range parts[1:] {
		name, value, found := strings.Cut(opt, "=")
		if !found {
			return nil, fmt.Errorf("--node-file %q: %s", s, usage)
		}
		switch name {
		case "mode":
			if !contains(nodeFileModes, value) {
				return nil, fmt.Errorf("--node-file %q: the mode %q is not one of %s", s, value, strings.Join(nodeFileModes, ", "))
			}
			file["permissions"] = value
		case "roles":
			roles, err := parseNodeRoles(value)
			if err != nil {
				return nil, fmt.Errorf("--node-file %q: %w", s, err)
			}
			file["roles"] = roles
		default:
			return nil, fmt.Errorf("--node-file %q: the option %q is neither mode nor roles", s, name)
		}
	}
	return file, nil
}

// parseNodeCommand reads CMD, optionally prefixed control-plane: or worker:.
func parseNodeCommand(s string) (map[string]any, error) {
	command := map[string]any{}
	run := s
	for flagRole, role := range nodeRoles {
		if rest, found := strings.CutPrefix(s, flagRole+":"); found {
			run = rest
			command["roles"] = []any{role}
		}
	}
	if strings.TrimSpace(run) == "" {
		return nil, errors.New("--node-command: give the command to run, such as \"augenrules --load\"")
	}
	command["run"] = run
	return command, nil
}

// mergeNodeCustomisation puts the flags' value into the create request's variables. Lists of the
// request grow by the flags' entries, and the request's audit policy and webhook give way to the flags'.
func mergeNodeCustomisation(request []byte, flagValue map[string]any) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(request, &req); err != nil || req == nil {
		return nil, errors.New("the create request must be an object to take --node-* flags")
	}
	variables := requestVariables(req)
	merged, ok := variables[nodeCustomisationVariable].(map[string]any)
	if !ok {
		merged = map[string]any{}
	}
	for field, v := range flagValue {
		list, isList := v.([]any)
		existing, wasList := merged[field].([]any)
		if isList && wasList {
			merged[field] = append(existing, list...)
			continue
		}
		merged[field] = v
	}
	variables[nodeCustomisationVariable] = merged
	out, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("writing the create request: %w", err)
	}
	return out, nil
}
