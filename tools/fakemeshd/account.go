package main

import (
	"context"
	"errors"
	"fmt"

	"meridian/trellis/internal/account"
	"meridian/trellis/internal/api"
)

// The operator account, in memory (internal/account on a node).

// A made-up operator's key.
const demoKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILhNVh+5m5DDjNfMtEp20Yqp3LnpdpNB3trND13c2OoB op@laptop"

func newAccount(setup bool) account.State {
	st := account.State{Name: account.Name, Keys: []account.Key{}, SSH: account.SSHActive}
	if !setup {
		k, _ := account.ParseKey(demoKey)
		st.Exists, st.Password, st.Keys = true, true, []account.Key{k}
	}
	return st
}

func (f *fake) Account(ctx context.Context) (*account.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.account
	st.Keys = append([]account.Key{}, st.Keys...)
	return &st, nil
}

func (f *fake) SetAccount(ctx context.Context, in api.AccountInput, dryRun bool) (*account.State, error) {
	f.mu.Lock()
	err := f.setAccount(in, dryRun)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.Account(ctx)
}

// setAccount checks and, unless dryRun, applies; f.mu held.
func (f *fake) setAccount(in api.AccountInput, dryRun bool) error {
	st := f.account
	if in.Password != "" {
		if err := account.CheckPassword(in.Password); err != nil {
			return api.Invalid("%v", err)
		}
		st.Password = true
	}
	if in.Keys != nil {
		keys := []account.Key{}
		for i, l := range *in.Keys {
			k, err := account.ParseKey(l)
			if err != nil {
				return api.Invalid("key %d: %v", i+1, err)
			}
			keys = account.MergeKeys(keys, k)
		}
		st.Keys = keys
	}
	if in.KeyOnly != nil {
		st.KeyOnly = *in.KeyOnly
	}
	if st.KeyOnly && len(st.Keys) == 0 {
		return api.Invalid("password authentication: off requires an SSH key")
	}
	if !dryRun {
		st.Exists = true
		f.account = st
	}
	return nil
}

// GitHubKeys: any user has the demo key; "nobody" does not exist, and
// "offline" is what a node without internet answers.
func (f *fake) GitHubKeys(ctx context.Context, user string) ([]account.Key, error) {
	switch user {
	case "nobody":
		return nil, api.Invalid("GitHub user %s: not found", user)
	case "offline":
		return nil, api.Invalid("%v", errors.New("GitHub not reachable: timed out (no internet on this node?). Paste the key instead"))
	}
	k, err := account.ParseKey(demoKey)
	if err != nil {
		return nil, err
	}
	k.Comment = fmt.Sprintf("github:%s", user)
	return []account.Key{k}, nil
}
