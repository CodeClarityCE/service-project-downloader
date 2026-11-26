package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: test server for git http-backend; Go is far past 1.6.3
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	return recordGitFailing(t, func([]string) bool { return !run })
}

// recordGitFailing records every git command line and fails the ones fail picks.
func recordGitFailing(t *testing.T, fail func(args []string) bool) *gitRecorder {
	t.Helper()
	rec := &gitRecorder{}
	saved := newGitCmd
	newGitCmd = func(ctx context.Context, args ...string) *exec.Cmd {
		rec.mu.Lock()
		rec.calls = append(rec.calls, args)
		rec.mu.Unlock()
		if fail(args) {
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

func TestOnlyHTTPSURLsAreAcceptedForAHostNotAllowlisted(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	rec := recordGit(t, false)
	fakeDNS(t, map[string]string{"gitlab.example.com": "203.0.113.10"})
	t.Setenv(allowedPrivateHostsEnv, "other.example.com")

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

func TestHTTPIsRefusedUnlessTheIntegrationHostIsAllowlisted(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	rec := recordGit(t, false)
	fakeDNS(t, map[string]string{"gitlab.example.com": "203.0.113.10", "gitlab.internal": "10.0.0.5"})

	cases := []struct{ project, domain, allow string }{
		{"http://gitlab.example.com/group/repo", "http://gitlab.example.com", ""},
		{"http://gitlab.example.com/group/repo", "https://gitlab.example.com", "gitlab.internal"},
		{"http://gitlab.internal/group/repo", "http://gitlab.internal", "other.internal"},
		// An allowlisted host that is not the integration's host gets nothing either.
		{"http://gitlab.internal/group/repo", "http://gitlab.example.com", "gitlab.internal"},
	}
	for _, tc := range cases {
		t.Setenv(allowedPrivateHostsEnv, tc.allow)
		err := Git(headAnalysis(), vcsProject(tc.project), gitlabIntegration(tc.domain), uuid.New())
		if !errors.Is(err, ErrRemoteNotAllowed) {
			t.Errorf("%s on %s (allow %q): want ErrRemoteNotAllowed, got %v", tc.project, tc.domain, tc.allow, err)
		}
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Errorf("want no git process, got %v", calls)
	}
}

func TestAnHTTPRemoteGetsHTTPPortAndHeaderInItsOwnEnv(t *testing.T) {
	fakeDNS(t, map[string]string{"gitlab.internal": "10.0.0.5"})
	t.Setenv(allowedPrivateHostsEnv, "gitlab.internal")
	p := newRemotePolicy("gitlab.internal", testToken)
	for raw, want := range map[string][3]string{
		"https://gitlab.internal/g/r.git": {"https", "gitlab.internal:443:10.0.0.5", "http.https://gitlab.internal/.extraHeader"},
		"http://gitlab.internal/g/r.git":  {"https:http", "gitlab.internal:80:10.0.0.5", "http.http://gitlab.internal/.extraHeader"},
	} {
		r, err := p.vet(context.Background(), raw, true)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		env := gitEnv(r)
		for _, kv := range []string{"GIT_ALLOW_PROTOCOL=" + want[0], "GIT_CONFIG_VALUE_2=" + want[1], "GIT_CONFIG_KEY_3=" + want[2]} {
			if !slices.Contains(env, kv) {
				t.Errorf("%s: want %s, got %v", raw, kv, env)
			}
		}
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
	// requireAuth, when set, answers 401 to any request without this Authorization header.
	requireAuth string
}

// setRequireAuth sets requireAuth under the lock the handler reads it with.
func (s *gitServer) setRequireAuth(header string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireAuth = header
}

func (s *gitServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func newGitServer(t *testing.T, redirects map[string]string) *gitServer {
	t.Helper()
	return startGitServer(t, redirects, httptest.NewTLSServer)
}

// newHTTPGitServer is newGitServer over plain http.
func newHTTPGitServer(t *testing.T) *gitServer {
	t.Helper()
	return startGitServer(t, nil, httptest.NewServer)
}

func startGitServer(t *testing.T, redirects map[string]string, start func(http.Handler) *httptest.Server) *gitServer {
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
	s.Server = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.URL.Path+" auth="+r.Header.Get("Authorization"))
		required := s.requireAuth
		s.mu.Unlock()
		if required != "" && r.Header.Get("Authorization") != required {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		for from, to := range redirects {
			if strings.HasPrefix(r.URL.Path, from) {
				http.Redirect(w, r, to+strings.TrimPrefix(r.URL.Path, from)+"?"+r.URL.RawQuery, http.StatusFound) //nolint:gosec // G710: test server redirecting on purpose
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
	cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed test setup commands
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
	bare := filepath.Join(s.root, name+".git")
	run(t, "", "clone", "-q", "--bare", work, bare)
	run(t, bare, "config", "uploadpack.allowFilter", "true")
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
				if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testToken)) { //nolint:gosec // G304: a file under the test's temp dir
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

func TestHTTPCloneFromAllowedPrivateHostSendsTheTokenOnlyAsAHeader(t *testing.T) {
	download := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", download)
	srv := newHTTPGitServer(t)
	srv.addRepo(t, "repo", map[string]string{"package.json": "{}"}, "", "")
	srv.setRequireAuth(basicAuth("oauth2", testToken))
	fakeDNS(t, map[string]string{"gitlab.internal": "127.0.0.1"})
	t.Setenv(allowedPrivateHostsEnv, "gitlab.internal")
	rec := recordGit(t, true)
	logs := captureLogs(t)

	host := "gitlab.internal:" + srv.port(t)
	org := uuid.New()
	project := vcsProject("http://" + host + "/repo.git")
	analysis := headAnalysis()
	if err := Git(analysis, project, gitlabIntegration("http://"+host), org); err != nil {
		t.Fatalf("clone: %v", err)
	}

	dest := projectSourcePath(download, org, project, analysis)
	if _, err := os.Stat(filepath.Join(dest, "package.json")); err != nil {
		t.Fatalf("want the repository checked out at %s: %v", dest, err)
	}
	for _, args := range rec.all() {
		if strings.Contains(strings.Join(args, " "), testToken) {
			t.Errorf("token on a git command line: %v", args)
		}
	}
	for _, dir := range []string{dest, filepath.Join(download, org.String(), "cache")} {
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && filepath.Base(p) == "config" {
				if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(testToken)) { //nolint:gosec // G304: a file under the test's temp dir
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

func TestCommitCloneWithoutTheCacheSendsTheTokenOnEveryFetch(t *testing.T) {
	download := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", download)
	srv := newGitServer(t, nil)
	commit := srv.addRepo(t, "repo", map[string]string{"package.json": "{}"}, "", "")
	srv.setRequireAuth(basicAuth("oauth2", testToken))
	trustServers(t)
	fakeDNS(t, map[string]string{"gitlab.internal": "127.0.0.1"})
	t.Setenv(allowedPrivateHostsEnv, "gitlab.internal")
	// The shallow fetch by SHA fails, so the blobless clone and its checkout run.
	rec := recordGitFailing(t, func(args []string) bool {
		return len(args) > 1 && args[0] == "fetch" && args[1] == "--depth"
	})

	host := "gitlab.internal:" + srv.port(t)
	org := uuid.New()
	project := vcsProject("https://" + host + "/repo.git")
	// A recent cache failure sends the download to the shallow and blobless tiers.
	cache := filepath.Join(download, org.String(), "cache", project.Id.String())
	if err := os.MkdirAll(filepath.Dir(cache), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache+".unavailable", []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	analysis := codeclarity.Analysis{Id: uuid.New(), Branch: "main", Commit: commit}

	if err := Git(analysis, project, gitlabIntegration("https://"+host), org); err != nil {
		t.Fatalf("clone at commit: %v", err)
	}

	dest := projectSourcePath(download, org, project, analysis)
	if _, err := os.Stat(filepath.Join(dest, "package.json")); err != nil {
		t.Fatalf("want the commit checked out: %v", err)
	}
	blobless := false
	for _, args := range rec.all() {
		blobless = blobless || slices.Contains(args, "--filter=blob:none")
	}
	if !blobless {
		t.Fatalf("want the blobless tier to run, got %v", rec.all())
	}
	if b, _ := os.ReadFile(filepath.Join(dest, ".git", "config")); bytes.Contains(b, []byte(testToken)) { //nolint:gosec // G304: a file under the test's temp dir
		t.Error("token in the cloned repo's config")
	}
	for _, args := range rec.all() {
		if strings.Contains(strings.Join(args, " "), testToken) {
			t.Errorf("token on a git command line: %v", args)
		}
	}
}

func TestNumericHostFormsAreRefused(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	rec := recordGit(t, false)
	fakeDNS(t, map[string]string{"2130706433": "203.0.113.1", "0x7f.1": "203.0.113.1", "127.1": "203.0.113.1"})

	for _, host := range []string{"2130706433", "0x7f.1", "127.1"} {
		err := Git(headAnalysis(), vcsProject("https://"+host+"/repo"), gitlabIntegration("https://"+host), uuid.New())
		if !errors.Is(err, ErrRemoteNotAllowed) {
			t.Errorf("%s: want ErrRemoteNotAllowed, got %v", host, err)
		}
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Errorf("want no git process, got %v", calls)
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

func TestHTTPSubmoduleOnAnAllowlistedIntegrationHostIsCloned(t *testing.T) {
	download := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", download)
	srv := newHTTPGitServer(t)
	subCommit := srv.addRepo(t, "lib", map[string]string{"lib.txt": "lib"}, "", "")
	srv.addRepo(t, "repo", map[string]string{"a": "a"}, "../lib.git", subCommit)
	srv.setRequireAuth(basicAuth("oauth2", testToken))
	fakeDNS(t, map[string]string{"gitlab.internal": "127.0.0.1"})
	t.Setenv(allowedPrivateHostsEnv, "gitlab.internal")
	host := "gitlab.internal:" + srv.port(t)

	org := uuid.New()
	project := vcsProject("http://" + host + "/repo.git")
	analysis := headAnalysis()
	if err := Git(analysis, project, gitlabIntegration("http://"+host), org); err != nil {
		t.Fatalf("clone: %v", err)
	}

	if _, err := os.Stat(filepath.Join(projectSourcePath(download, org, project, analysis), "sub", "lib.txt")); err != nil {
		t.Errorf("want the submodule checked out: %v", err)
	}
}

func TestHTTPSubmoduleIsRefusedOffAnAllowlistedIntegrationHost(t *testing.T) {
	main := newGitServer(t, nil)
	other := newHTTPGitServer(t)
	subCommit := other.addRepo(t, "lib", map[string]string{"lib.txt": "lib"}, "", "")
	trustServers(t)
	treatAllAddressesAsPublic(t)
	fakeDNS(t, map[string]string{"gitlab.test": "127.0.0.1", "other.test": "127.0.0.1"})
	host := "gitlab.test:" + main.port(t)

	for _, tc := range []struct{ name, subHost, allow string }{
		// A foreign host, even one on the allowlist.
		{"foreign", "other.test", "other.test,gitlab.test"},
		// The integration's host, not on the allowlist.
		{"not allowlisted", "gitlab.test", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOWNLOAD_PATH", t.TempDir())
			t.Setenv(allowedPrivateHostsEnv, tc.allow)
			repo := strings.ReplaceAll(tc.name, " ", "-")
			main.addRepo(t, repo, map[string]string{"a": "a"}, "http://"+tc.subHost+":"+other.port(t)+"/lib.git", subCommit)

			err := Git(headAnalysis(), vcsProject("https://"+host+"/"+repo+".git"), gitlabIntegration("https://"+host), uuid.New())

			if !errors.Is(err, ErrRemoteNotAllowed) {
				t.Fatalf("want ErrRemoteNotAllowed, got %v", err)
			}
			if r := other.requests(); len(r) != 0 {
				t.Errorf("want no request to the http submodule host, got %v", r)
			}
		})
	}
}

func TestRelativeSubmoduleURLResolvesAgainstTheProject(t *testing.T) {
	for _, tc := range []struct{ super, rel, want string }{
		{"https://h/g/p.git", "../lib.git", "https://h/g/lib.git"},
		{"https://h/g/p", "./lib", "https://h/g/p/lib"},
		{"https://h/g/sub/p.git", "../../x/lib.git", "https://h/g/x/lib.git"},
		{"https://h/g/p.git", "https://other/lib.git", "https://other/lib.git"},
		// Climbing past the host would change it, so it resolves to nothing.
		{"https://h/g/p.git", "../../../evil/x.git", ""},
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

func TestTwoSubmodulesWithOnePathAreRefusedBeforeAnyFetch(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	gitmodules := "[submodule \"a\"]\n\tpath = lib\n\turl = https://gitlab.test/a.git\n" +
		"[submodule \"b\"]\n\tpath = ./lib\n\turl = https://other.test/b.git\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte(gitmodules), 0o600); err != nil {
		t.Fatal(err)
	}
	treatAllAddressesAsPublic(t)
	fakeDNS(t, map[string]string{"gitlab.test": "127.0.0.1", "other.test": "127.0.0.1"})
	rec := recordGit(t, true)
	p := newRemotePolicy("https://gitlab.test", testToken)

	err := initSubmodules(context.Background(), dir, remote{url: "https://gitlab.test/repo.git"}, p, true, 0, logrus.NewEntry(logrus.StandardLogger()))

	if !errors.Is(err, ErrRemoteNotAllowed) {
		t.Fatalf("want ErrRemoteNotAllowed, got %v", err)
	}
	for _, args := range rec.all() {
		if slices.Contains(args, "submodule") {
			t.Errorf("want no submodule fetch, got %v", args)
		}
	}
}
