package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

const testToken = "glpat-test-token"

// gitRecorder records the argv of every git process the downloader starts.
type gitRecorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *gitRecorder) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

// recordGit wraps newGitCmd so the test sees every git command line; run=false fails them all.
func recordGit(t *testing.T, run bool) *gitRecorder {
	t.Helper()
	rec := &gitRecorder{}
	saved := newGitCmd
	newGitCmd = func(ctx context.Context, args ...string) *exec.Cmd {
		rec.mu.Lock()
		rec.calls = append(rec.calls, args)
		rec.mu.Unlock()
		if !run {
			return exec.CommandContext(ctx, "false")
		}
		return saved(ctx, args...)
	}
	t.Cleanup(func() { newGitCmd = saved })
	return rec
}

// fakeDNS answers lookups from a fixed table and fails any other name.
func fakeDNS(t *testing.T, table map[string]string) {
	t.Helper()
	saved := lookupHost
	lookupHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		if addr, ok := table[host]; ok {
			return []netip.Addr{netip.MustParseAddr(addr)}, nil
		}
		return nil, errors.New("no such host " + host)
	}
	t.Cleanup(func() { lookupHost = saved })
}

// treatAllAddressesAsPublic lets loopback test servers stand in for public hosts.
func treatAllAddressesAsPublic(t *testing.T) {
	t.Helper()
	saved := isInternalAddress
	isInternalAddress = func(netip.Addr) bool { return false }
	t.Cleanup(func() { isInternalAddress = saved })
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	logrus.SetOutput(buf)
	t.Cleanup(func() { logrus.SetOutput(os.Stderr) })
	return buf
}

func gitlabIntegration(domain string) codeclarity.Integration {
	return codeclarity.Integration{Id: uuid.New(), AccessToken: testToken, ServiceDomain: domain}
}

func headAnalysis() codeclarity.Analysis {
	return codeclarity.Analysis{Id: uuid.New(), Branch: "main"}
}

func vcsProject(u string) codeclarity.Project {
	return codeclarity.Project{Id: uuid.New(), Url: u}
}

func TestProjectOnAnotherHostStartsNoGitAndUsesNoToken(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	rec := recordGit(t, false)
	fakeDNS(t, map[string]string{"gitlab.example.com": "203.0.113.10", "evil.example.org": "203.0.113.20"})

	err := Git(headAnalysis(), vcsProject("https://evil.example.org/group/repo"),
		gitlabIntegration("https://gitlab.example.com"), uuid.New())

	if !errors.Is(err, ErrRemoteNotAllowed) {
		t.Fatalf("want ErrRemoteNotAllowed, got %v", err)
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Errorf("want no git process, got %v", calls)
	}
}

func TestOnlyHTTPSURLsAreAccepted(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	rec := recordGit(t, false)
	fakeDNS(t, map[string]string{"gitlab.example.com": "203.0.113.10"})

	for _, u := range []string{
		"http://gitlab.example.com/group/repo",
		"file:///etc/passwd",
		"ssh://git@gitlab.example.com/group/repo",
		"ext::sh -c touch% /tmp/pwned",
		"git@gitlab.example.com:group/repo.git",
		"https://user:pass@gitlab.example.com/group/repo",
	} {
		err := Git(headAnalysis(), vcsProject(u), gitlabIntegration("https://gitlab.example.com"), uuid.New())
		if !errors.Is(err, ErrRemoteNotAllowed) {
			t.Errorf("%s: want ErrRemoteNotAllowed, got %v", u, err)
		}
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Errorf("want no git process, got %v", calls)
	}
}

func TestPrivateAddressIsRefusedUnlessTheOperatorAllowsTheHost(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	rec := recordGit(t, false)
	fakeDNS(t, map[string]string{"gitlab.internal": "10.0.0.5", "github.com": "127.0.0.1"})

	cases := []struct{ project, domain, allow string }{
		{"https://gitlab.internal/group/repo", "https://gitlab.internal", ""},
		{"https://gitlab.internal/group/repo", "https://gitlab.internal", "other.internal"},
		{"https://github.com/org/repo", "github.com", ""},
		{"https://169.254.169.254/latest", "https://169.254.169.254", ""},
		{"https://[::ffff:127.0.0.1]/x", "https://[::ffff:127.0.0.1]", ""},
	}
	for _, tc := range cases {
		t.Setenv(allowedPrivateHostsEnv, tc.allow)
		err := Git(headAnalysis(), vcsProject(tc.project), gitlabIntegration(tc.domain), uuid.New())
		if !errors.Is(err, ErrRemoteNotAllowed) {
			t.Errorf("%s (allow %q): want ErrRemoteNotAllowed, got %v", tc.project, tc.allow, err)
		}
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Errorf("want no git process, got %v", calls)
	}
}

func TestInternalAddressRangesMatchTheAPI(t *testing.T) {
	for addr, internal := range map[string]bool{
		"0.0.0.0": true, "10.1.2.3": true, "100.64.0.1": true, "127.0.0.1": true,
		"169.254.169.254": true, "172.16.0.1": true, "172.31.255.255": true, "192.0.0.1": true,
		"192.168.1.1": true, "198.18.0.1": true, "224.0.0.1": true, "240.0.0.1": true,
		"::": true, "::1": true, "64:ff9b::1": true, "64:ff9b:1::1": true, "fc00::1": true,
		"fd12::1": true, "fe80::1": true, "fec0::1": true, "ff02::1": true, "::ffff:10.0.0.1": true,
		"8.8.8.8": false, "172.32.0.1": false, "140.82.112.3": false, "2606:4700::1111": false,
	} {
		if got := isInternalAddress(netip.MustParseAddr(addr)); got != internal {
			t.Errorf("%s: internal=%v, want %v", addr, got, internal)
		}
	}
}

// gitServer serves bare repositories under root over TLS with git http-backend,
// recording each request's path and Authorization header.
type gitServer struct {
	*httptest.Server
	root string
	mu   sync.Mutex
	seen []string
}

func (s *gitServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func newGitServer(t *testing.T, redirects map[string]string) *gitServer {
	t.Helper()
	backend, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	backendPath := filepath.Join(strings.TrimSpace(string(backend)), "git-http-backend")
	if _, err := os.Stat(backendPath); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	s := &gitServer{root: t.TempDir()}
	cgiHandler := &cgi.Handler{Path: backendPath, Env: []string{"GIT_PROJECT_ROOT=" + s.root, "GIT_HTTP_EXPORT_ALL=1"}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.URL.Path+" auth="+r.Header.Get("Authorization"))
		s.mu.Unlock()
		for from, to := range redirects {
			if strings.HasPrefix(r.URL.Path, from) {
				http.Redirect(w, r, to+strings.TrimPrefix(r.URL.Path, from)+"?"+r.URL.RawQuery, http.StatusFound)
				return
			}
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *gitServer) port(t *testing.T) string {
	t.Helper()
	u, _ := url.Parse(s.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	return port
}

// run runs git in dir for test setup.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// addRepo creates a bare repo name.git on the server with one commit on main holding files,
// and an optional submodule gitlink at "sub" pointing to subURL at subCommit.
func (s *gitServer) addRepo(t *testing.T, name string, files map[string]string, subURL, subCommit string) string {
	t.Helper()
	work := t.TempDir()
	run(t, work, "init", "-q", "-b", "main")
	for f, content := range files {
		if err := os.WriteFile(filepath.Join(work, f), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, work, "add", f)
	}
	if subURL != "" {
		gitmodules := "[submodule \"sub\"]\n\tpath = sub\n\turl = " + subURL + "\n"
		if err := os.WriteFile(filepath.Join(work, ".gitmodules"), []byte(gitmodules), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, work, "add", ".gitmodules")
		run(t, work, "update-index", "--add", "--cacheinfo", "160000,"+subCommit+",sub")
	}
	run(t, work, "commit", "-q", "-m", "init")
	run(t, "", "clone", "-q", "--bare", work, filepath.Join(s.root, name+".git"))
	return run(t, work, "rev-parse", "HEAD")
}

// trustServers makes git accept the test servers' certificates.
func trustServers(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestCloneFromAllowedPrivateHostSendsTheTokenOnlyAsAHeader(t *testing.T) {
	download := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", download)
	srv := newGitServer(t, nil)
	srv.addRepo(t, "repo", map[string]string{"package.json": "{}"}, "", "")
	trustServers(t)
	fakeDNS(t, map[string]string{"gitlab.internal": "127.0.0.1"})
	t.Setenv(allowedPrivateHostsEnv, " other.example , gitlab.internal")
	rec := recordGit(t, true)
	logs := captureLogs(t)

	host := "gitlab.internal:" + srv.port(t)
	org := uuid.New()
	project := vcsProject("https://" + host + "/repo.git")
	analysis := headAnalysis()
	if err := Git(analysis, project, gitlabIntegration("https://"+host), org); err != nil {
		t.Fatalf("clone: %v", err)
	}

	dest := projectSourcePath(download, org, project, analysis)
	if _, err := os.Stat(filepath.Join(dest, "package.json")); err != nil {
		t.Fatalf("want the repository checked out at %s: %v", dest, err)
	}
	want := "auth=" + basicAuth("oauth2", testToken)
	sawToken := false
	for _, r := range srv.requests() {
		sawToken = sawToken || strings.HasSuffix(r, want)
	}
	if !sawToken {
		t.Errorf("want the token sent as a header, got requests %v", srv.requests())
	}
	for _, args := range rec.all() {
		if strings.Contains(strings.Join(args, " "), testToken) {
			t.Errorf("token on a git command line: %v", args)
		}
	}
	for _, dir := range []string{dest, filepath.Join(download, org.String(), "cache")} {
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && filepath.Base(p) == "config" {
				if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testToken)) {
					t.Errorf("token in %s", p)
				}
			}
			return nil
		})
	}
	if strings.Contains(logs.String(), testToken) {
		t.Errorf("token in logs")
	}
}

func TestGithubTokenIsSentAsTheUsername(t *testing.T) {
	if got := authHeader("github.com", "ghp_x"); got != "Authorization: "+basicAuth("ghp_x", "") {
		t.Errorf("got %q", got)
	}
	if got := authHeader("gitlab.example.com", "glpat_x"); got != "Authorization: "+basicAuth("oauth2", "glpat_x") {
		t.Errorf("got %q", got)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	srv := newGitServer(t, map[string]string{"/moved.git": "/repo.git"})
	srv.addRepo(t, "repo", map[string]string{"a": "a"}, "", "")
	trustServers(t)
	treatAllAddressesAsPublic(t)
	fakeDNS(t, map[string]string{"gitlab.test": "127.0.0.1"})
	host := "gitlab.test:" + srv.port(t)

	err := Git(headAnalysis(), vcsProject("https://"+host+"/moved.git"), gitlabIntegration("https://"+host), uuid.New())

	if err == nil {
		t.Fatal("want the clone to fail on a redirect")
	}
	for _, r := range srv.requests() {
		if strings.HasPrefix(r, "/repo.git") {
			t.Errorf("redirect was followed: %s", r)
		}
	}
}

func TestSubmoduleOnAForeignPublicHostIsClonedWithoutTheToken(t *testing.T) {
	download := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", download)
	main := newGitServer(t, nil)
	other := newGitServer(t, nil)
	subCommit := other.addRepo(t, "lib", map[string]string{"lib.txt": "lib"}, "", "")
	trustServers(t)
	treatAllAddressesAsPublic(t)
	fakeDNS(t, map[string]string{"gitlab.test": "127.0.0.1", "other.test": "127.0.0.1"})
	host := "gitlab.test:" + main.port(t)
	main.addRepo(t, "repo", map[string]string{"a": "a"}, "https://other.test:"+other.port(t)+"/lib.git", subCommit)

	org := uuid.New()
	project := vcsProject("https://" + host + "/repo.git")
	analysis := headAnalysis()
	if err := Git(analysis, project, gitlabIntegration("https://"+host), org); err != nil {
		t.Fatalf("clone: %v", err)
	}

	if _, err := os.Stat(filepath.Join(projectSourcePath(download, org, project, analysis), "sub", "lib.txt")); err != nil {
		t.Errorf("want the submodule checked out: %v", err)
	}
	if len(other.requests()) == 0 {
		t.Fatal("want the submodule fetched from the other host")
	}
	for _, r := range other.requests() {
		if !strings.HasSuffix(r, "auth=") {
			t.Errorf("token sent to the submodule host: %s", r)
		}
	}
}

func TestSubmoduleOnAnInternalHostIsRefused(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	main := newGitServer(t, nil)
	other := newGitServer(t, nil)
	subCommit := other.addRepo(t, "lib", map[string]string{"lib.txt": "lib"}, "", "")
	trustServers(t)
	fakeDNS(t, map[string]string{"gitlab.internal": "127.0.0.1", "other.internal": "127.0.0.1"})
	t.Setenv(allowedPrivateHostsEnv, "gitlab.internal")
	host := "gitlab.internal:" + main.port(t)
	main.addRepo(t, "repo", map[string]string{"a": "a"}, "https://other.internal:"+other.port(t)+"/lib.git", subCommit)

	err := Git(headAnalysis(), vcsProject("https://"+host+"/repo.git"), gitlabIntegration("https://"+host), uuid.New())

	if !errors.Is(err, ErrRemoteNotAllowed) {
		t.Fatalf("want ErrRemoteNotAllowed, got %v", err)
	}
	if r := other.requests(); len(r) != 0 {
		t.Errorf("want no request to the internal submodule host, got %v", r)
	}
}

func TestRelativeSubmoduleURLResolvesAgainstTheProject(t *testing.T) {
	for _, tc := range []struct{ super, rel, want string }{
		{"https://h/g/p.git", "../lib.git", "https://h/g/lib.git"},
		{"https://h/g/p", "./lib", "https://h/g/p/lib"},
		{"https://h/g/sub/p.git", "../../x/lib.git", "https://h/g/x/lib.git"},
		{"https://h/g/p.git", "https://other/lib.git", "https://other/lib.git"},
	} {
		if got := resolveSubmoduleURL(tc.super, tc.rel); got != tc.want {
			t.Errorf("%s + %s = %s, want %s", tc.super, tc.rel, got, tc.want)
		}
	}
}

func TestFailureReasonForARefusedAddress(t *testing.T) {
	got := downloadFailureReason(errors.Join(ErrRemoteNotAllowed, errors.New("host x")))
	if !strings.Contains(got, "not allowed") {
		t.Errorf("got %q", got)
	}
}
