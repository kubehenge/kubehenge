package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// HAWriter writes Proxmox VE HA resources and rules, as *proxmox.Client does.
type HAWriter interface {
	Post(ctx context.Context, path string, params, out any) error
	Put(ctx context.Context, path string, params, out any) error
	Delete(ctx context.Context, path string, out any) error
}

// haJobTimeout bounds one HA write: Proxmox VE answers it at once.
const haJobTimeout = time.Minute

// HAJobs writes through a token Job each: the HA token holds Sys.Console on /, a shell on every
// node, so it stays out of the controller and is used only for the writes HARequest allows.
type HAJobs struct {
	Jobs TokenRunner
}

var _ HAWriter = &HAJobs{}

// HARequest is one HA write.
type HARequest struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Params map[string]any `json:"params,omitempty"`
}

// Post implements HAWriter.
func (h *HAJobs) Post(ctx context.Context, path string, params, _ any) error {
	return h.write(ctx, http.MethodPost, path, params)
}

// Put implements HAWriter.
func (h *HAJobs) Put(ctx context.Context, path string, params, _ any) error {
	return h.write(ctx, http.MethodPut, path, params)
}

// Delete implements HAWriter.
func (h *HAJobs) Delete(ctx context.Context, path string, _ any) error {
	return h.write(ctx, http.MethodDelete, path, nil)
}

func (h *HAJobs) write(ctx context.Context, method, path string, params any) error {
	req := HARequest{Method: method, Path: path}
	if params != nil {
		m, ok := params.(map[string]any)
		if !ok {
			return fmt.Errorf("HA write %s %s: parameters of type %T", method, path, params)
		}
		req.Params = m
	}
	_, err := h.Jobs.Run(ctx, req, haJobTimeout)
	return err //nolint:wrapcheck // Proxmox VE's answer as the HA reconciler reads it, which names the write
}

var (
	haResourceID = regexp.MustCompile(`^vm:[1-9]\d*$`)
	haRuleName   = regexp.MustCompile(`^kubehenge-[0-9a-f]{16}-(` + apartRule + `|` + nodesRule + `)$`)
)

// errNotKubehenges refuses a write to an HA object of someone else's.
var errNotKubehenges = errors.New("not one of Kubehenge's HA objects")

// ServeHARequest makes one HA write with the HA token, and only to Kubehenge's own HA resources and
// rules, those whose comment HAComment wrote: the token could do much else.
func ServeHARequest(ctx context.Context, ha *proxmox.Client, raw []byte) (string, error) {
	var req HARequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("the request: %w", err)
	}
	if err := checkHARequest(ctx, ha, req); err != nil {
		return "", fmt.Errorf("%s %s: %w", req.Method, req.Path, err)
	}
	var err error
	switch req.Method {
	case http.MethodPost:
		err = ha.Post(ctx, req.Path, req.Params, nil)
	case http.MethodPut:
		err = ha.Put(ctx, req.Path, req.Params, nil)
	default:
		err = ha.Delete(ctx, req.Path, nil)
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", req.Method, req.Path, err)
	}
	return "", nil
}

const (
	haResourcesPath = "/cluster/ha/resources"
	haRulesPath     = "/cluster/ha/rules"
)

// checkHARequest allows creating, changing and removing what carries Kubehenge's comment: HA
// resources of VMs, and rules over such resources.
func checkHARequest(ctx context.Context, ha *proxmox.Client, req HARequest) error {
	switch {
	case req.Method == http.MethodPost && req.Path == haResourcesPath:
		if !haResourceID.MatchString(param(req.Params, "sid")) || !kubehengeComment(req.Params) || !onlyParams(req.Params, "sid", "state", "comment") {
			return errNotKubehenges
		}
		return nil
	case req.Method == http.MethodDelete && strings.HasPrefix(req.Path, haResourcesPath+"/"):
		sid, err := url.PathUnescape(strings.TrimPrefix(req.Path, haResourcesPath+"/"))
		if err != nil {
			return errNotKubehenges
		}
		return ownResources(ctx, ha, sid)
	case req.Method == http.MethodPost && req.Path == haRulesPath:
		if !haRuleName.MatchString(param(req.Params, "rule")) || !kubehengeComment(req.Params) ||
			!onlyParams(req.Params, "rule", "comment", "type", "resources", "nodes", "strict", "affinity") {
			return errNotKubehenges
		}
		return ownResources(ctx, ha, param(req.Params, "resources"))
	case (req.Method == http.MethodPut || req.Method == http.MethodDelete) && strings.HasPrefix(req.Path, haRulesPath+"/"):
		return checkRuleChange(ctx, ha, req)
	}
	return errNotKubehenges
}

// checkRuleChange allows changing or removing one of Kubehenge's rules, and changing it only to
// one over Kubehenge's resources.
func checkRuleChange(ctx context.Context, ha *proxmox.Client, req HARequest) error {
	if !haRuleName.MatchString(strings.TrimPrefix(req.Path, haRulesPath+"/")) {
		return errNotKubehenges
	}
	var have struct {
		Comment string `json:"comment"`
	}
	if err := ha.Get(ctx, req.Path, &have); err != nil {
		return err //nolint:wrapcheck // its caller names the request
	}
	if !strings.HasPrefix(have.Comment, haCommentPrefix) {
		return errNotKubehenges
	}
	if req.Method == http.MethodDelete {
		return nil
	}
	if !onlyParams(req.Params, "type", "resources", "nodes") {
		return errNotKubehenges
	}
	return ownResources(ctx, ha, param(req.Params, "resources"))
}

// param is a parameter's text, or empty when it has none.
func param(params map[string]any, key string) string {
	s, ok := params[key].(string)
	if !ok {
		return ""
	}
	return s
}

func kubehengeComment(params map[string]any) bool {
	return strings.HasPrefix(param(params, "comment"), haCommentPrefix)
}

func onlyParams(params map[string]any, allowed ...string) bool {
	for k := range params {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

// ownResources checks that the HA resources listed are all Kubehenge's, so that a rule moves no one
// else's VMs and a removal takes out none of theirs.
func ownResources(ctx context.Context, ha *proxmox.Client, listed string) error {
	for _, sid := range splitList(listed) {
		if !haResourceID.MatchString(sid) {
			return fmt.Errorf("%w: resource %s", errNotKubehenges, sid)
		}
		var have haResource
		if err := ha.Get(ctx, haResourcesPath+"/"+url.PathEscape(sid), &have); err != nil {
			return fmt.Errorf("reading HA resource %s: %w", sid, err)
		}
		if !strings.HasPrefix(have.Comment, haCommentPrefix) {
			return fmt.Errorf("%w: resource %s", errNotKubehenges, sid)
		}
	}
	return nil
}
