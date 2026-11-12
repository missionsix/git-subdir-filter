package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/libgit2/git2go/v34"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCheckCert(t *testing.T) {
	known, other := newHostKey(t), newHostKey(t)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	lines := knownhosts.Line([]string{"example.com"}, known) + "\n" +
		knownhosts.Line([]string{"[alt.example.com]:2222"}, known) + "\n"
	if err := os.WriteFile(kh, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := knownhosts.New(kh)
	if err != nil {
		t.Fatal(err)
	}
	hostkey := func(k ssh.PublicKey) *git.Certificate {
		return &git.Certificate{Kind: git.CertificateHostkey,
			Hostkey: git.HostkeyCertificate{Kind: git.HostkeyRaw, SSHPublicKey: k}}
	}

	tests := []struct {
		name     string
		cert     *git.Certificate
		valid    bool
		host     string
		port     int
		insecure bool
		wantErr  string
	}{
		{name: "known key", cert: hostkey(known), host: "example.com", port: 22},
		{name: "known key custom port", cert: hostkey(known), host: "alt.example.com", port: 2222},
		{name: "wrong port", cert: hostkey(known), host: "alt.example.com", port: 22, wantErr: "verification failed"},
		{name: "changed key", cert: hostkey(other), host: "example.com", port: 22, wantErr: "verification failed"},
		{name: "unknown host", cert: hostkey(known), host: "evil.example", port: 22, wantErr: "verification failed"},
		{name: "insecure skips", cert: hostkey(other), host: "evil.example", port: 22, insecure: true},
		{name: "tls valid", cert: &git.Certificate{Kind: git.CertificateX509}, valid: true, host: "h"},
		{name: "tls invalid", cert: &git.Certificate{Kind: git.CertificateX509}, host: "h", wantErr: "not trusted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCert(tt.cert, tt.valid, tt.host, tt.port, cb, tt.insecure)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCallbacksRequireKnownHostsForSSH(t *testing.T) {
	a := Auth{KnownHosts: []string{filepath.Join(t.TempDir(), "missing")}}
	if _, err := a.Callbacks("git@github.com:o/r.git"); err == nil || !strings.Contains(err.Error(), "no known_hosts") {
		t.Fatalf("ssh url: err = %v, want missing known_hosts error", err)
	}
	if _, err := a.Callbacks("file:///tmp/r"); err != nil {
		t.Fatalf("file url should not need known_hosts: %v", err)
	}
}

func TestCredentialsStopAfterEachMethod(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	key := filepath.Join(t.TempDir(), "id_ed25519")
	cb := Auth{KeyPath: key}.credentials()

	c, err := cb("u", "", git.CredentialTypeUsername|git.CredentialTypeSSHKey)
	if err != nil || c.Type() != git.CredentialTypeUsername {
		t.Fatalf("first call: %v %v, want username credential", c, err)
	}
	if c, err = cb("u", "git", git.CredentialTypeSSHKey); err != nil || c.Type() != git.CredentialTypeSSHKey {
		t.Fatalf("second call: %v %v, want ssh key credential", c, err)
	}
	if _, err = cb("u", "git", git.CredentialTypeSSHKey); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("third call: err = %v, want authentication failed", err)
	}
}

func TestURLHelpers(t *testing.T) {
	for u, want := range map[string]bool{
		"git@github.com:o/r.git": true, "ssh://git@h:2222/r": true, "https://h/r": false,
		"file:///r": false, "/abs/path": false,
	} {
		if got := isSSH(u); got != want {
			t.Errorf("isSSH(%q) = %v, want %v", u, got, want)
		}
	}
	for u, want := range map[string]int{"ssh://git@h:2222/r": 2222, "ssh://h/r": 22, "git@h:o/r": 22} {
		if got := sshPort(u); got != want {
			t.Errorf("sshPort(%q) = %d, want %d", u, got, want)
		}
	}
}

func TestDefaultKeyPath(t *testing.T) {
	home := t.TempDir()
	if got := DefaultKeyPath(home); got != "" {
		t.Fatalf("empty home: %q", got)
	}
	must(t, os.MkdirAll(filepath.Join(home, ".ssh"), 0o700))
	for _, n := range []string{"id_rsa", "id_ed25519"} {
		must(t, os.WriteFile(filepath.Join(home, ".ssh", n), nil, 0o600))
	}
	if got := DefaultKeyPath(home); filepath.Base(got) != "id_ed25519" {
		t.Fatalf("got %q, want id_ed25519 preferred", got)
	}
}

// TestFetchPushRoundTrip moves a commit src -> work -> dst over file:// URLs.
func TestFetchPushRoundTrip(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *git.Repository {
		r, err := git.InitRepository(filepath.Join(dir, name), true)
		must(t, err)
		t.Cleanup(r.Free)
		return r
	}
	src, work, dst := open("src"), open("work"), open("dst")

	tb, err := src.TreeBuilder()
	must(t, err)
	tree, err := tb.Write()
	must(t, err)
	sig := &git.Signature{Name: "t", Email: "t@t", When: time.Unix(1_600_000_000, 0)}
	id, err := src.CreateCommitFromIds("refs/heads/main", sig, sig, "c", tree)
	must(t, err)

	must(t, Fetch(work, "origin", "file://"+filepath.Join(dir, "src"), "+refs/heads/main:refs/remotes/origin/main", Auth{}))
	must(t, Push(work, "target", "file://"+filepath.Join(dir, "dst"), "+refs/remotes/origin/main:refs/heads/out", Auth{}))

	ref, err := dst.References.Lookup("refs/heads/out")
	must(t, err)
	if !ref.Target().Equal(id) {
		t.Fatalf("dst out = %s, want %s", ref.Target(), id)
	}
	// Changing the URL of an existing remote must take effect.
	err = Push(work, "target", "file://"+filepath.Join(dir, "nope"), "+refs/remotes/origin/main:refs/heads/out", Auth{})
	if err == nil {
		t.Fatal("push to missing repo succeeded; remote URL not updated")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
