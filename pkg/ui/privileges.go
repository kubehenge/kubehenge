package ui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// privilegesFresh is how long a user's privileges are taken as current for the lists a form loads
// together. It is short, so a grant shows within seconds.
const privilegesFresh = 5 * time.Second

// userPrivileges is what one question for all of a user's privileges answered, and the answers to
// the questions asked about the paths it cannot tell.
type userPrivileges struct {
	all map[string]pve.Privileges
	err error

	mu    sync.Mutex
	asked map[string]*askedPath
}

type askedPath struct {
	once  sync.Once
	privs pve.Privileges
	err   error
}

// userPrivileges asks for the user's privileges on all the paths Proxmox VE lists, once for every
// list read within privilegesFresh.
func (s *Server) userPrivileges(ctx context.Context, u user) (*userPrivileges, error) {
	up, err := s.privileges.get(ctx, u.ticket.Username, privilegesFresh, s.now, func(ctx context.Context) *userPrivileges {
		all, err := s.Accounts.AllPrivileges(ctx, u.ticket)
		return &userPrivileges{all: all, err: err, asked: map[string]*askedPath{}}
	})
	if err != nil {
		return nil, err
	}
	if up.err != nil {
		// The next list asks again rather than taking the failure as fresh.
		s.privileges.forget(u.ticket.Username, func(kept *userPrivileges) bool { return kept == up })
		return nil, fmt.Errorf("reading the user's privileges: %w", up.err)
	}
	return up, nil
}

// privilegesOn gives the user's privileges on each path, in order. A path the one answer lists is
// read from it. One it does not list holds nothing, unless it inherits from a path above it that
// does (inheritedHere), and then Proxmox VE is asked for that path, once however many lists need it.
func (s *Server) privilegesOn(ctx context.Context, u user, paths []string) ([]pve.Privileges, error) {
	up, err := s.userPrivileges(ctx, u)
	if err != nil {
		return nil, err
	}
	privs := make([]pve.Privileges, len(paths))
	errs := make([]error, len(paths))
	sem := make(chan struct{}, privilegeAsks)
	var wg sync.WaitGroup
	for i, path := range paths {
		if listed, ok := up.all[path]; ok || !inheritedHere(up.all, path) {
			privs[i] = listed
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			privs[i], errs[i] = s.askedPrivileges(ctx, u, up, path)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return privs, nil
}

func (s *Server) askedPrivileges(ctx context.Context, u user, up *userPrivileges, path string) (pve.Privileges, error) {
	up.mu.Lock()
	a, ok := up.asked[path]
	if !ok {
		a = &askedPath{}
		up.asked[path] = a
	}
	up.mu.Unlock()
	a.once.Do(func() { a.privs, a.err = s.Accounts.Privileges(ctx, u.ticket, path) })
	if a.err != nil {
		// A failed question is asked again by the next list, not remembered.
		up.mu.Lock()
		if up.asked[path] == a {
			delete(up.asked, path)
		}
		up.mu.Unlock()
	}
	return a.privs, a.err
}
