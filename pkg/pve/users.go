package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// administratorRole is Proxmox VE's built-in role holding every privilege.
const administratorRole = "Administrator"

// CreateAdministratorToken makes a Proxmox VE user, userid in a realm of Proxmox VE's own, holding
// the Administrator role on / with an API token named name that shares its privileges, and returns
// the token as user@realm!name=secret. A user of that ID left from before is deleted first, with
// any token it had. Proxmox VE shows the secret once, here.
func (a *Accounts) CreateAdministratorToken(ctx context.Context, t *Ticket, userid, name, comment string) (string, error) {
	if err := a.DeleteUser(ctx, t, userid); err != nil {
		return "", err
	}
	if err := a.send(ctx, t, http.MethodPost, "/access/users", url.Values{"userid": {userid}, "comment": {comment}}, nil); err != nil {
		return "", fmt.Errorf("creating the user %s: %w", userid, err)
	}
	acl := url.Values{"path": {"/"}, "roles": {administratorRole}, "users": {userid}, "propagate": {"1"}}
	if err := a.send(ctx, t, http.MethodPut, "/access/acl", acl, nil); err != nil {
		return "", errors.Join(fmt.Errorf("granting %s the Administrator role: %w", userid, err), a.DeleteUser(ctx, t, userid))
	}
	var token struct {
		ID    string `json:"full-tokenid"`
		Value string `json:"value"`
	}
	path := "/access/users/" + url.PathEscape(userid) + "/token/" + url.PathEscape(name)
	// privsep=0: the token holds what its user holds, which exists for nothing else.
	err := a.send(ctx, t, http.MethodPost, path, url.Values{"privsep": {"0"}, "comment": {comment}}, &token)
	if err == nil && (token.ID == "" || token.Value == "") {
		err = errors.New("proxmox VE returned no token")
	}
	if err != nil {
		return "", errors.Join(fmt.Errorf("creating the API token of %s: %w", userid, err), a.DeleteUser(ctx, t, userid))
	}
	return token.ID + "=" + token.Value, nil
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
	return a.do(req, out)
}
