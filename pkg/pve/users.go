package pve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Upgrader is the user automatic upgrades of the management cluster act as: userid in a realm of
// Proxmox VE's own and in Group, with the roles of Roles on Grants alone, and an API token named
// Token that shares its privileges.
type Upgrader struct {
	User, Token, Comment, Group string
	Roles                       map[string][]string
	Grants                      []Grant
}

// Grant is a role on a path.
type Grant struct {
	Path      string
	Role      string
	Propagate bool
}

// CreateUpgrader makes the upgrader's user and token and returns the token as
// user@realm!name=secret. A user of that ID left from before is deleted first, with any token it
// had. Proxmox VE shows the secret once, here.
func (a *Accounts) CreateUpgrader(ctx context.Context, t *Ticket, u *Upgrader) (string, error) {
	if err := a.DeleteUser(ctx, t, u.User); err != nil {
		return "", err
	}
	if err := a.send(ctx, t, http.MethodPost, "/access/users", url.Values{"userid": {u.User}, "comment": {u.Comment}, "groups": {u.Group}}, nil); err != nil {
		return "", fmt.Errorf("creating the user %s: %w", u.User, err)
	}
	if err := a.GrantUpgrader(ctx, t, u); err != nil {
		return "", errors.Join(err, a.DeleteUser(ctx, t, u.User))
	}
	var token struct {
		ID    string `json:"full-tokenid"`
		Value string `json:"value"`
	}
	path := "/access/users/" + url.PathEscape(u.User) + "/token/" + url.PathEscape(u.Token)
	// privsep=0: the token holds what its user holds, which exists for nothing else.
	err := a.send(ctx, t, http.MethodPost, path, url.Values{"privsep": {"0"}, "comment": {u.Comment}}, &token)
	if err == nil && (token.ID == "" || token.Value == "") {
		err = errors.New("proxmox VE returned no token")
	}
	if err != nil {
		return "", errors.Join(fmt.Errorf("creating the API token of %s: %w", u.User, err), a.DeleteUser(ctx, t, u.User))
	}
	return token.ID + "=" + token.Value, nil
}

// GrantUpgrader gives the upgrader's roles their privileges, puts its user in its group and leaves
// the user its grants alone, so that a user made with other grants, such as Administrator on /,
// holds these instead.
func (a *Accounts) GrantUpgrader(ctx context.Context, t *Ticket, u *Upgrader) error {
	if err := a.putRoles(ctx, t, u.Roles); err != nil {
		return err
	}
	if err := a.send(ctx, t, http.MethodPut, "/access/users/"+url.PathEscape(u.User), url.Values{"groups": {u.Group}}, nil); err != nil {
		return fmt.Errorf("putting %s in the group %s: %w", u.User, u.Group, err)
	}
	return a.grantAlone(ctx, t, u.User, u.Grants)
}

// putRoles makes each role, or sets the privileges of one there already.
func (a *Accounts) putRoles(ctx context.Context, t *Ticket, roles map[string][]string) error {
	var listed []struct {
		ID string `json:"roleid"`
	}
	if err := a.send(ctx, t, http.MethodGet, "/access/roles", nil, &listed); err != nil {
		return fmt.Errorf("listing the roles: %w", err)
	}
	exists := map[string]bool{}
	for _, r := range listed {
		exists[r.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(roles)) {
		privs := strings.Join(roles[id], ",")
		var err error
		if exists[id] {
			err = a.send(ctx, t, http.MethodPut, "/access/roles/"+url.PathEscape(id), url.Values{"privs": {privs}}, nil)
		} else {
			err = a.send(ctx, t, http.MethodPost, "/access/roles", url.Values{"roleid": {id}, "privs": {privs}}, nil)
		}
		if err != nil {
			return fmt.Errorf("making the role %s: %w", id, err)
		}
	}
	return nil
}

// grantAlone leaves user the grants alone: the missing ones are added first, then the others are
// taken away, Administrator last, so that the user's own token, while it holds Administrator, can
// do all of it.
func (a *Accounts) grantAlone(ctx context.Context, t *Ticket, user string, grants []Grant) error {
	var acl []struct {
		Path      string `json:"path"`
		Role      string `json:"roleid"`
		Type      string `json:"type"`
		ID        string `json:"ugid"`
		Propagate int    `json:"propagate"`
	}
	if err := a.send(ctx, t, http.MethodGet, "/access/acl", nil, &acl); err != nil {
		return fmt.Errorf("reading the ACLs: %w", err)
	}
	held := map[Grant]bool{}
	var others []Grant
	for _, e := range acl {
		g := Grant{Path: e.Path, Role: e.Role, Propagate: e.Propagate != 0}
		switch {
		case e.Type != "user" || e.ID != user:
		case slices.Contains(grants, g):
			held[g] = true
		default:
			others = append(others, g)
		}
	}
	for _, g := range grants {
		if held[g] {
			continue
		}
		if err := a.send(ctx, t, http.MethodPut, "/access/acl", url.Values{"path": {g.Path}, "roles": {g.Role}, "users": {user}, "propagate": {g.propagate()}}, nil); err != nil {
			return fmt.Errorf("granting %s the role %s on %s: %w", user, g.Role, g.Path, err)
		}
	}
	slices.SortStableFunc(others, func(x, y Grant) int { return cmp.Compare(x.lastToGo(), y.lastToGo()) })
	for _, g := range others {
		if err := a.send(ctx, t, http.MethodPut, "/access/acl", url.Values{"path": {g.Path}, "roles": {g.Role}, "users": {user}, "delete": {"1"}}, nil); err != nil {
			return fmt.Errorf("taking %s on %s from %s: %w", g.Role, g.Path, user, err)
		}
	}
	return nil
}

// administratorRole is Proxmox VE's built-in role holding every privilege, which upgraders made
// before held on /.
const administratorRole = "Administrator"

// lastToGo orders the grants taken away: Administrator after every other.
func (g Grant) lastToGo() int {
	if g.Role == administratorRole {
		return 1
	}
	return 0
}

// propagate is the grant's propagate flag as Proxmox VE takes it.
func (g Grant) propagate() string {
	if g.Propagate {
		return "1"
	}
	return "0"
}

// RevokeToken deletes an API token, user@realm!name=secret, acting as the token itself. Proxmox VE
// allows that only with User.Modify on a group of the token's user.
func (a *Accounts) RevokeToken(ctx context.Context, token string) error {
	id, _, _ := strings.Cut(token, "=")
	user, name, ok := strings.Cut(id, "!")
	if !ok {
		return errors.New("the Proxmox VE API token must be user@realm!name=secret")
	}
	path := "/access/users/" + url.PathEscape(user) + "/token/" + url.PathEscape(name)
	err := a.send(ctx, &Ticket{Username: id, Token: token}, http.MethodDelete, path, nil, nil)
	if err != nil {
		return fmt.Errorf("revoking the API token %s: %w", id, err)
	}
	return nil
}

// CreateOwnToken signs in as userid with its password and creates, as that user, its API token
// named name that shares its privileges, and returns the token's secret. Proxmox VE lets another
// user create such a token only with Permissions.Modify on /. A user with a second factor is
// refused, since no one is there to answer it.
func (a *Accounts) CreateOwnToken(ctx context.Context, userid, password, name, comment string) (string, error) {
	t, challenge, err := a.SignIn(ctx, userid, password)
	if err != nil {
		return "", fmt.Errorf("signing in as %s: %w", userid, err)
	}
	if challenge != nil {
		return "", fmt.Errorf("signing in as %s: Proxmox VE asks for a second factor. Take the realm's or the user's TFA off", userid)
	}
	var token struct {
		Value string `json:"value"`
	}
	path := "/access/users/" + url.PathEscape(userid) + "/token/" + url.PathEscape(name)
	if err := a.send(ctx, t, http.MethodPost, path, url.Values{"privsep": {"0"}, "comment": {comment}}, &token); err != nil {
		return "", err
	}
	if token.Value == "" {
		return "", errors.New("proxmox VE returned no token secret")
	}
	return token.Value, nil
}

// DeleteUser deletes a Proxmox VE user, which takes its API tokens and ACL entries with it. A user
// that does not exist is no error.
func (a *Accounts) DeleteUser(ctx context.Context, t *Ticket, userid string) error {
	err := a.send(ctx, t, http.MethodDelete, "/access/users/"+url.PathEscape(userid), nil, nil)
	// Proxmox VE names a missing user its own way.
	if apiErr, ok := errors.AsType[*APIError](err); ok && strings.Contains(apiErr.Status, "no such user") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting the user %s: %w", userid, err)
	}
	return nil
}

// send makes a change Proxmox VE answers at once, without a task, with the form as its body. out,
// when set, takes the answer's data.
func (a *Accounts) send(ctx context.Context, t *Ticket, method, path string, form url.Values, out any) error {
	req, err := a.newRequest(ctx, method, path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	t.authorise(req)
	if out == nil {
		var ignored any
		out = &ignored
	}
	return t.refused(a.do(req, out))
}
