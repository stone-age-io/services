package natsconn

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nkeys"
)

// account is an operator-mode account with users signed by its key.
type account struct {
	t        *testing.T
	operator nkeys.KeyPair
	oc       *jwt.OperatorClaims
	kp       nkeys.KeyPair
	claims   *jwt.AccountClaims
}

func newAccount(t *testing.T) *account {
	okp, _ := nkeys.CreateOperator()
	opub, _ := okp.PublicKey()
	oc := jwt.NewOperatorClaims(opub)
	oc.Subject = opub
	akp, _ := nkeys.CreateAccount()
	apub, _ := akp.PublicKey()
	return &account{t: t, operator: okp, oc: oc, kp: akp, claims: jwt.NewAccountClaims(apub)}
}

// user returns a creds file for a new user, and the user's public key.
func (a *account) user() ([]byte, string) {
	ukp, _ := nkeys.CreateUser()
	upub, _ := ukp.PublicKey()
	seed, _ := ukp.Seed()
	ujwt, err := jwt.NewUserClaims(upub).Encode(a.kp)
	if err != nil {
		a.t.Fatal(err)
	}
	creds, err := jwt.FormatUserConfig(ujwt, seed)
	if err != nil {
		a.t.Fatal(err)
	}
	return creds, upub
}

// server starts a server on port that trusts the account as it is now.
func (a *account) server(port int) *server.Server {
	ajwt, err := a.claims.Encode(a.operator)
	if err != nil {
		a.t.Fatal(err)
	}
	r := &server.MemAccResolver{}
	if err := r.Store(a.claims.Subject, ajwt); err != nil {
		a.t.Fatal(err)
	}
	opts := natstest.DefaultTestOptions
	opts.Port = port
	opts.TrustedOperators = []*jwt.OperatorClaims{a.oc}
	opts.AccountResolver = r
	return natstest.RunServer(&opts)
}

// The Revoke flow of ADR 0007 §4: the server refuses the old key, the client
// keeps retrying through repeated auth errors, and it connects as soon as a
// new creds file is on disk.
func TestReconnectsWithNewCredsAfterRevoke(t *testing.T) {
	acc := newAccount(t)
	oldCreds, oldPub := acc.user()
	newCreds, newPub := acc.user()

	path := filepath.Join(t.TempDir(), "user.creds")
	if err := os.WriteFile(path, oldCreds, 0o600); err != nil {
		t.Fatal(err)
	}

	s := acc.server(-1)
	port := s.Addr().(*net.TCPAddr).Port
	nc, err := Connect(s.ClientURL(), path, "test", slog.New(slog.NewTextHandler(io.Discard, nil)), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)

	// Revoke the old key, and restart the server so the client reconnects
	// with the old file.
	acc.claims.Revoke(oldPub)
	s.Shutdown()
	s = acc.server(port)
	t.Cleanup(s.Shutdown)

	// More than two refused attempts: without IgnoreAuthErrorAbort the
	// client would have given up by now. ReconnectWait is 2s.
	time.Sleep(5 * time.Second)
	if nc.IsClosed() {
		t.Fatal("client gave up after auth errors")
	}
	if nc.IsConnected() {
		t.Fatal("client connected with a revoked key")
	}

	if err := os.WriteFile(path, newCreds, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !nc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("did not reconnect with the new creds file")
		}
		time.Sleep(50 * time.Millisecond)
	}

	cz, err := s.Connz(&server.ConnzOptions{Username: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cz.Conns) != 1 || cz.Conns[0].AuthorizedUser != newPub {
		t.Fatalf("want one connection as %s, got %+v", newPub, cz.Conns)
	}
}
