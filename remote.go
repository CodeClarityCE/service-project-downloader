package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// ErrRemoteNotAllowed is returned when a repository or submodule address may not be fetched.
var ErrRemoteNotAllowed = errors.New("RemoteNotAllowed")

// allowedPrivateHostsEnv lists the GitLab hosts the operator allows on private addresses.
const allowedPrivateHostsEnv = "GITLAB_ALLOWED_PRIVATE_HOSTS"

// maxSubmoduleDepth bounds how deep nested submodules are followed.
const maxSubmoduleDepth = 5

// Loopback, private, link-local (cloud metadata), shared and reserved ranges, as the API's assertPublicUrl.
var internalPrefixes = func() []netip.Prefix {
	var prefixes []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		prefixes = append(prefixes, netip.MustParsePrefix(p))
	}
	return prefixes
}()

// isInternalAddress reports whether an address is in a refused range (IPv4-mapped IPv6 included).
var isInternalAddress = func(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range internalPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return !addr.IsValid()
}

// lookupHost resolves a host name to its addresses.
var lookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// remotePolicy decides which addresses the downloader may fetch for one integration.
type remotePolicy struct {
	host           string // the integration's host name
	token          string
	allowedPrivate map[string]bool
}

// remote is a vetted address: what git may fetch, and the git environment that enforces it.
type remote struct {
	url  string
	host string
	pins []string // http.curloptResolve entries
	auth []string // URL-scoped extraHeader entries, key then value
}

// hostOf returns the lower-case host name of a URL or bare host, without brackets.
func hostOf(entry string) string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ""
	}
	if !strings.Contains(entry, "://") {
		entry = "https://" + entry
	}
	u, err := url.Parse(entry)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// newRemotePolicy builds the policy from the integration and the operator allowlist.
func newRemotePolicy(serviceDomain, token string) remotePolicy {
	allowed := map[string]bool{}
	for entry := range strings.SplitSeq(os.Getenv(allowedPrivateHostsEnv), ",") {
		if h := hostOf(entry); h != "" {
			allowed[h] = true
		}
	}
	return remotePolicy{host: hostOf(serviceDomain), token: token, allowedPrivate: allowed}
}

// vet checks one address: https only, no credentials in the URL, the project on the
// integration's host, and no internal address unless the operator allows the integration's host.
func (p remotePolicy) vet(ctx context.Context, raw string, isProject bool) (remote, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.Opaque != "" {
		return remote{}, fmt.Errorf("%w: only https URLs without credentials are allowed", ErrRemoteNotAllowed)
	}
	host := strings.ToLower(u.Hostname())
	onIntegrationHost := p.host != "" && host == p.host
	if isProject && !onIntegrationHost {
		return remote{}, fmt.Errorf("%w: host %s is not the integration's host", ErrRemoteNotAllowed, host)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return remote{}, fmt.Errorf("%w: bad port %q", ErrRemoteNotAllowed, port)
	}

	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = lookupHost(ctx, host); err != nil {
		return remote{}, fmt.Errorf("%w: host %s does not resolve: %v", ErrRemoteNotAllowed, host, err)
	}
	if len(addrs) == 0 {
		return remote{}, fmt.Errorf("%w: host %s has no address", ErrRemoteNotAllowed, host)
	}
	privateAllowed := onIntegrationHost && p.allowedPrivate[host]
	pinned := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if isInternalAddress(a) && !privateAllowed {
			return remote{}, fmt.Errorf("%w: host %s resolves to an internal address", ErrRemoteNotAllowed, host)
		}
		if a.Unmap().Is4() {
			pinned = append(pinned, a.Unmap().String())
		} else {
			pinned = append(pinned, "["+a.String()+"]")
		}
	}

	// curl wants the name without brackets; IPv6 literals need no pin.
	r := remote{url: u.String(), host: host}
	if _, err := netip.ParseAddr(host); err != nil {
		r.pins = []string{host + ":" + port + ":" + strings.Join(pinned, ",")}
	}
	if onIntegrationHost && p.token != "" {
		// Scoped to this scheme, host and port, so no other host ever gets the header.
		r.auth = []string{"http." + u.Scheme + "://" + u.Host + "/.extraHeader", authHeader(host, p.token)}
	}
	return r, nil
}

// authHeader is the basic-auth header GitHub (token as user) or GitLab (oauth2 user) expects.
func authHeader(host, token string) string {
	creds := "oauth2:" + token
	if host == "github.com" {
		creds = token + ":"
	}
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// gitEnv is the environment for network git commands fetching from the given remotes:
// https only, no redirects, no stored credentials, vetted addresses pinned, token only by header.
func gitEnv(remotes ...remote) []string {
	config := [][2]string{
		{"http.followRedirects", "false"},
		{"credential.helper", ""},
	}
	for _, r := range remotes {
		for _, pin := range r.pins {
			config = append(config, [2]string{"http.curloptResolve", pin})
		}
		if len(r.auth) == 2 {
			config = append(config, [2]string{r.auth[0], r.auth[1]})
		}
	}
	env := []string{"GIT_ALLOW_PROTOCOL=https", "GIT_CONFIG_COUNT=" + strconv.Itoa(len(config))}
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// resolveSubmoduleURL resolves a relative submodule URL against its superproject's URL, as git does.
func resolveSubmoduleURL(super, rel string) string {
	if !strings.HasPrefix(rel, "./") && !strings.HasPrefix(rel, "../") {
		return rel
	}
	base := strings.TrimSuffix(super, "/")
	for {
		switch {
		case strings.HasPrefix(rel, "./"):
			rel = rel[2:]
		case strings.HasPrefix(rel, "../"):
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[:i]
			}
			rel = rel[3:]
		default:
			return base + "/" + rel
		}
	}
}

// initSubmodules checks out the submodules of the work tree at dir one level at a time,
// vetting each submodule URL with the same policy before git fetches it.
func initSubmodules(ctx context.Context, dir string, super remote, p remotePolicy, shallow bool, depth int, log *logrus.Entry) error {
	if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); err != nil {
		return nil
	}
	out, err := gitOutput(ctx, dir, "config", "-f", ".gitmodules", "--get-regexp", `^submodule\..*\.url$`)
	if err != nil || out == "" {
		return nil
	}
	if depth >= maxSubmoduleDepth {
		return fmt.Errorf("%w: submodules nested deeper than %d", ErrRemoteNotAllowed, maxSubmoduleDepth)
	}
	for line := range strings.SplitSeq(out, "\n") {
		key, rawURL, _ := strings.Cut(line, " ")
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".url")
		path, err := gitOutput(ctx, dir, "config", "-f", ".gitmodules", "--get", "submodule."+name+".path")
		if err != nil || !filepath.IsLocal(path) {
			return fmt.Errorf("%w: submodule %q has no safe path", ErrRemoteNotAllowed, name)
		}
		sub, err := p.vet(ctx, resolveSubmoduleURL(super.url, rawURL), false)
		if err != nil {
			log.WithError(err).WithFields(logrus.Fields{"submodule": name, "host": hostOf(resolveSubmoduleURL(super.url, rawURL))}).Warn("Refused a submodule address")
			return err
		}
		if err := runGit(ctx, dir, "config", "submodule."+name+".url", sub.url); err != nil {
			return fmt.Errorf("git config submodule: %w", err)
		}
		args := []string{"submodule", "update", "--init"}
		if shallow {
			args = append(args, "--depth", "1")
		}
		if err := runGitEnv(ctx, dir, gitEnv(sub), append(args, "--", path)...); err != nil {
			return fmt.Errorf("git submodule update %s: %w", name, err)
		}
		if err := initSubmodules(ctx, filepath.Join(dir, path), sub, p, shallow, depth+1, log); err != nil {
			return err
		}
	}
	return nil
}
