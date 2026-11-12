// Package remote fetches from and pushes to git remotes with SSH key or agent
// authentication and known_hosts host-key verification.
package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	git "github.com/libgit2/git2go/v34"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Auth configures credentials and host verification for SSH remotes.
type Auth struct {
	// KeyPath is a private key; empty means the first of ~/.ssh/id_ed25519,
	// id_ecdsa, id_rsa that exists. "<KeyPath>.pub" is used when present.
	KeyPath    string
	Passphrase string
	// UseAgent tries ssh-agent (SSH_AUTH_SOCK) before the key file.
	UseAgent bool
	// KnownHosts lists known_hosts files; InsecureSkipHostCheck disables the check.
	KnownHosts            []string
	InsecureSkipHostCheck bool
}

// DefaultKeyPath returns the first conventional private key under home, or "".
func DefaultKeyPath(home string) string {
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Fetch fetches refspec from url. It uses an anonymous remote, so concurrent
// runs against different URLs in one work directory don't interfere.
// Cancelling ctx aborts the transfer.
func Fetch(ctx context.Context, repo *git.Repository, url, refspec string, auth Auth) error {
	r, err := repo.Remotes.CreateAnonymous(url)
	if err != nil {
		return err
	}
	defer r.Free()
	cbs, err := auth.Callbacks(ctx, url)
	if err != nil {
		return err
	}
	return r.Fetch([]string{refspec}, &git.FetchOptions{
		DownloadTags:    git.DownloadTagsNone,
		RemoteCallbacks: cbs,
	}, "")
}

// Push pushes refspec to url. A ref the server rejects (for example by a
// protected-branch hook) is returned as an error.
func Push(ctx context.Context, repo *git.Repository, url, refspec string, auth Auth) error {
	r, err := repo.Remotes.CreateAnonymous(url)
	if err != nil {
		return err
	}
	defer r.Free()
	cbs, err := auth.Callbacks(ctx, url)
	if err != nil {
		return err
	}
	cbs.PushUpdateReferenceCallback = rejectedRef
	return r.Push([]string{refspec}, &git.PushOptions{RemoteCallbacks: cbs})
}

// rejectedRef turns a per-ref push status into an error; "" means accepted.
func rejectedRef(refname, status string) error {
	if status != "" {
		return fmt.Errorf("remote rejected %s: %s", refname, status)
	}
	return nil
}

// Callbacks builds libgit2 callbacks for one remote URL.
func (a Auth) Callbacks(ctx context.Context, remoteURL string) (git.RemoteCallbacks, error) {
	var hostCB ssh.HostKeyCallback
	if !a.InsecureSkipHostCheck && isSSH(remoteURL) {
		var files []string
		for _, f := range a.KnownHosts {
			if _, err := os.Stat(f); err == nil {
				files = append(files, f)
			}
		}
		if len(files) == 0 {
			return git.RemoteCallbacks{}, fmt.Errorf("no known_hosts file found in %v; add the host with ssh-keyscan", a.KnownHosts)
		}
		cb, err := knownhosts.New(files...)
		if err != nil {
			return git.RemoteCallbacks{}, err
		}
		hostCB = cb
	}
	port := sshPort(remoteURL)

	return git.RemoteCallbacks{
		CredentialsCallback: a.credentials(),
		// libgit2 polls these during transfers; an error aborts it.
		TransferProgressCallback:     func(git.TransferProgress) error { return ctx.Err() },
		PushTransferProgressCallback: func(uint32, uint32, uint) error { return ctx.Err() },
		CertificateCheckCallback: func(cert *git.Certificate, valid bool, host string) error {
			return checkCert(cert, valid, host, port, hostCB, a.InsecureSkipHostCheck)
		},
	}, nil
}

func checkCert(cert *git.Certificate, valid bool, host string, port int, hostCB ssh.HostKeyCallback, insecure bool) error {
	if insecure {
		return nil
	}
	switch cert.Kind {
	case git.CertificateHostkey:
		if hostCB == nil {
			return errors.New("ssh host key presented but no known_hosts configured")
		}
		if cert.Hostkey.SSHPublicKey == nil {
			return fmt.Errorf("%s: libgit2 did not provide the raw host key", host)
		}
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		placeholder := &net.TCPAddr{IP: net.IPv4zero, Port: port} // knownhosts prefers addr
		if err := hostCB(addr, placeholder, cert.Hostkey.SSHPublicKey); err != nil {
			return fmt.Errorf("host key verification failed for %s: %w", addr, err)
		}
		return nil
	default: // X.509 (HTTPS): defer to libgit2's validation.
		if !valid {
			return fmt.Errorf("%s: TLS certificate not trusted", host)
		}
		return nil
	}
}

// credentials yields at most one credential per method; libgit2 calls again
// after each failure, so a bad key ends in an error instead of a loop.
func (a Auth) credentials() git.CredentialsCallback {
	tried := map[string]bool{}
	return func(_ string, user string, allowed git.CredentialType) (*git.Credential, error) {
		if user == "" {
			user = "git"
		}
		if allowed&git.CredentialTypeUsername != 0 && !tried["username"] {
			tried["username"] = true
			return git.NewCredentialUsername(user)
		}
		if allowed&git.CredentialTypeSSHKey == 0 {
			return nil, fmt.Errorf("remote requires unsupported credential type %v", allowed)
		}
		if a.UseAgent && os.Getenv("SSH_AUTH_SOCK") != "" && !tried["agent"] {
			tried["agent"] = true
			return git.NewCredentialSSHKeyFromAgent(user)
		}
		if a.KeyPath != "" && !tried["key"] {
			tried["key"] = true
			pub := a.KeyPath + ".pub"
			if _, err := os.Stat(pub); err != nil {
				pub = "" // libssh2 derives it from the private key
			}
			return git.NewCredentialSSHKey(user, pub, a.KeyPath, a.Passphrase)
		}
		return nil, errors.New("ssh authentication failed: no usable agent identity or key")
	}
}

// isSSH reports whether u uses SSH, including scp-style "git@host:path".
func isSSH(u string) bool {
	if strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git+ssh://") || strings.HasPrefix(u, "ssh+git://") {
		return true
	}
	if strings.Contains(u, "://") {
		return false
	}
	// As in git, scp-style needs a colon before any slash, so "./a:b" is local.
	colon := strings.Index(u, ":")
	slash := strings.Index(u, "/")
	return colon > 0 && (slash < 0 || colon < slash)
}

// sshPort returns the port from an ssh:// URL, or 22.
func sshPort(u string) int {
	if p, err := url.Parse(u); err == nil && p.Port() != "" {
		if n, err := strconv.Atoi(p.Port()); err == nil {
			return n
		}
	}
	return 22
}
