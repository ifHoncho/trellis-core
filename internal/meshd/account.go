package meshd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/api"
)

// The operator account (internal/account): the Linux user trellis,
// set from the setup wizard and Settings → Access.

// accountSystem is the machine's account system. A lab's meshd
// (--no-host-config) shares the host's users: it changes none.
func (d *Daemon) accountSystem() (account.Service, error) {
	if d.opts.NoHostConfig {
		return nil, api.Errorf(http.StatusConflict, "operator account: not managed on this node (lab)")
	}
	if d.opts.Account != nil {
		return d.opts.Account, nil
	}
	bin, err := os.Executable()
	if err != nil {
		bin = ""
	}
	return &account.Remote{Binary: bin}, nil
}

// accountErr makes a refused change a 400; the rest stays as it is.
func accountErr(err error) error {
	var inv *account.InvalidError
	if errors.As(err, &inv) {
		return api.Invalid("%s", inv.Msg)
	}
	return err
}

// Account is the operator account as it is.
func (d *Daemon) Account(ctx context.Context) (*account.State, error) {
	s, err := d.accountSystem()
	if err != nil {
		return nil, err
	}
	return s.Get(ctx)
}

// SetAccount changes the operator account; a dry run only checks.
func (d *Daemon) SetAccount(ctx context.Context, in api.AccountInput, dryRun bool) (*account.State, error) {
	s, err := d.accountSystem()
	if err != nil {
		return nil, err
	}
	ch, err := accountChange(in)
	if err != nil {
		return nil, err
	}
	if err := s.Check(ctx, ch); err != nil {
		return nil, accountErr(err)
	}
	if !dryRun {
		if err := s.Apply(ctx, ch); err != nil {
			return nil, accountErr(err)
		}
		d.log.Info("operator account changed", "account", account.Name, "password", in.Password != "",
			"keys", in.Keys != nil, "key_only", in.KeyOnly != nil)
	}
	return s.Get(ctx)
}

// accountChange reads the API's input: each key must parse.
func accountChange(in api.AccountInput) (account.Change, error) {
	ch := account.Change{Password: in.Password, KeyOnly: in.KeyOnly}
	if in.Password != "" {
		if err := account.CheckPassword(in.Password); err != nil {
			return ch, api.Invalid("%v", err)
		}
	}
	if in.Keys != nil {
		keys := []account.Key{}
		for i, l := range *in.Keys {
			k, err := account.ParseKey(l)
			if err != nil {
				return ch, api.Invalid("key %d: %v", i+1, err)
			}
			keys = account.MergeKeys(keys, k)
		}
		ch.Keys = &keys
	}
	return ch, nil
}

// GitHubKeys fetches a GitHub user's public SSH keys over this node's
// uplink.
func (d *Daemon) GitHubKeys(ctx context.Context, user string) ([]account.Key, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	keys, err := account.GitHubKeys(ctx, &http.Client{Timeout: 15 * time.Second}, user)
	if err != nil {
		return nil, api.Invalid("%v", err)
	}
	return keys, nil
}
