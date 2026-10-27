package hostd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

var githubUserRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// GitHubKeys fetches the public SSH keys that GitHub publishes for an
// account at <base>/<user>.keys. base is https://github.com outside tests.
// Keys of a type that ParseKey refuses, such as DSA, are skipped.
func GitHubKeys(ctx context.Context, base, user string) ([]string, error) {
	if !githubUserRE.MatchString(user) {
		return nil, fmt.Errorf("github: %q is not a GitHub user name", user)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+user+".keys", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: %s.keys returned %d", user, resp.StatusCode)
	}
	var keys []string
	sc := bufio.NewScanner(io.LimitReader(resp.Body, MaxRequestSize))
	for sc.Scan() {
		if k, err := ParseKey(sc.Text()); err == nil {
			keys = append(keys, k+" github:"+user)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("github: %s has no usable SSH keys", user)
	}
	return keys, nil
}
