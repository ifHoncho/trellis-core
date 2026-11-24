package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// GitHub usernames: letters, digits and single hyphens, up to 39.
var githubUser = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// GitHubKeysURL is where GitHub publishes a user's public SSH keys. A
// variable for tests.
var GitHubKeysURL = "https://github.com/%s.keys"

// GitHubKeys fetches a GitHub user's public SSH keys. The node needs its
// own way to the internet (an uplink): a phone on the setup Wi-Fi has
// none.
func GitHubKeys(ctx context.Context, client *http.Client, user string) ([]Key, error) {
	if !githubUser.MatchString(user) {
		return nil, fmt.Errorf("GitHub username %q: letters, digits and hyphens, up to 39", user)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(GitHubKeysURL, user), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "trellis")
	resp, err := client.Do(req)
	if err != nil {
		var ue interface{ Timeout() bool }
		if errors.As(err, &ue) && ue.Timeout() {
			return nil, errors.New("GitHub not reachable: timed out (no internet on this node?). Paste the key instead")
		}
		return nil, fmt.Errorf("GitHub not reachable: %v. Paste the key instead", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("GitHub user %s: not found", user)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	keys, err := ParseKeys(string(body))
	if err != nil {
		return nil, fmt.Errorf("GitHub user %s: %v", user, err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("GitHub user %s: no public SSH keys", user)
	}
	for i := range keys {
		if keys[i].Comment == "" {
			keys[i].Comment = "github:" + user
		}
	}
	return keys, nil
}
