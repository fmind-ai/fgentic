package a2aclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"iter"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// newTestCA returns a self-signed CA certificate and its key for signing leaf certs in mTLS tests.
func newTestCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return cert, key
}

// issueLeaf issues a leaf certificate signed by ca for either server or client authentication.
func issueLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// mtlsFixture is a signed-AgentCard A2A server over TLS that requires a verified client certificate.
type mtlsFixture struct {
	server         *httptest.Server
	identity       CardIdentity
	serverRoots    *x509.CertPool // roots the bridge pins to trust the server cert
	clientCert     tls.Certificate
	wrongClient    tls.Certificate
	a2aRequests    int
	lastAuthHeader string
}

func newMTLSFixture(t *testing.T, cardSchemes a2a.NamedSecuritySchemes) *mtlsFixture {
	t.Helper()
	serverCA, serverCAKey := newTestCA(t, "server-ca")
	clientCA, clientCAKey := newTestCA(t, "client-ca")
	rogueCA, rogueCAKey := newTestCA(t, "rogue-ca")

	serverCert := issueLeaf(t, serverCA, serverCAKey, "partner-server", true)
	clientCert := issueLeaf(t, clientCA, clientCAKey, "fgentic-bridge", false)
	wrongClient := issueLeaf(t, rogueCA, rogueCAKey, "impostor", false)

	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(serverCA)
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(clientCA)

	key := newTestSigningKey(t)
	fixture := &mtlsFixture{
		identity:    testCardIdentity(t, key),
		serverRoots: serverRoots,
		clientCert:  clientCert,
		wrongClient: wrongClient,
	}

	executor := executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("mtls ack")), nil)
		}
	})
	handler := a2asrv.NewHandler(executor, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	endpoint := a2asrv.NewJSONRPCHandler(handler)

	mux := http.NewServeMux()
	var cardBody []byte
	mux.HandleFunc(remoteFixturePath+remoteAgentCardPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/a2a+json")
		_, _ = w.Write(cardBody)
	})
	mux.HandleFunc(remoteFixturePath, func(w http.ResponseWriter, req *http.Request) {
		fixture.a2aRequests++
		fixture.lastAuthHeader = req.Header.Get("Authorization")
		endpoint.ServeHTTP(w, req)
	})

	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientRoots,
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	fixture.server = server

	card := validRemoteCard(server.URL + remoteFixturePath)
	card.SecuritySchemes = cardSchemes
	cardBody = signValidAgentCard(t, card, key, fixture.identity.KeyID)
	return fixture
}

func (f *mtlsFixture) client(t *testing.T) *Client {
	t.Helper()
	return New("http://local.invalid", "local-gateway-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *mtlsFixture) target(t *testing.T, opts ...RemoteOption) Target {
	t.Helper()
	target, err := NewRemoteTarget(f.server.URL+remoteFixturePath, f.identity, 4096, nil, opts...)
	if err != nil {
		t.Fatalf("NewRemoteTarget: %v", err)
	}
	return target
}

func TestRemoteMTLSHandshakeSucceedsWithPinnedCert(t *testing.T) {
	f := newMTLSFixture(t, nil)
	client := f.client(t)
	target := f.target(t, WithClientTLS(f.clientCert, f.serverRoots, "cert-fp-1"))

	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("ResolveAgentCard over mTLS: %v", err)
	}
	if !client.IsReady(target) {
		t.Fatal("target not ready after verified mTLS card fetch")
	}
	result, err := client.Call(WithUser(t.Context(), "@alice:local.example"), target, "prompt", "", nil)
	if err != nil {
		t.Fatalf("Call over mTLS: %v", err)
	}
	if !result.Terminal || result.Text != "mtls ack" {
		t.Fatalf("Call result = %+v", result)
	}
}

func TestRemoteMTLSRefusesWrongCAClientCert(t *testing.T) {
	f := newMTLSFixture(t, nil)
	client := f.client(t)
	// A client cert signed by a CA the server does not trust: the handshake fails and the card
	// (and thus any A2A request) is never served.
	target := f.target(t, WithClientTLS(f.wrongClient, f.serverRoots, "cert-fp-rogue"))

	if _, err := client.ResolveAgentCard(t.Context(), target); err == nil {
		t.Fatal("ResolveAgentCard succeeded with an untrusted client certificate")
	}
	if client.IsReady(target) {
		t.Fatal("target became ready despite a failed mTLS handshake")
	}
	if f.a2aRequests != 0 {
		t.Fatalf("A2A endpoint received %d requests before a completed handshake", f.a2aRequests)
	}
}

func TestRemoteMTLSCardSchemeFailsClosedWithoutClientCert(t *testing.T) {
	// The card-scheme policy is transport-independent: a card that declares an mTLS security scheme
	// while its mapping configured no client certificate is refused before delegation, regardless of
	// the underlying transport. Proven over the plain-HTTP fixture so the handshake never masks it.
	fixture, client, target := newRemoteContractFixture(t, nil, "")
	card := cloneCardForTest(t, fixture.baseCard)
	card.SecuritySchemes = a2a.NamedSecuritySchemes{"mtls": a2a.MutualTLSSecurityScheme{}}
	fixture.setCard(signValidAgentCard(t, card, fixture.key, fixture.identity.KeyID), `"card-mtls"`)

	_, err := client.ResolveAgentCard(t.Context(), target)
	if !errors.Is(err, ErrRemoteMutualTLSRequired) {
		t.Fatalf("ResolveAgentCard error = %v, want ErrRemoteMutualTLSRequired", err)
	}
	if !errors.Is(err, ErrRemoteTargetUntrusted) {
		t.Fatal("mTLS-required refusal must quarantine as untrusted")
	}
	if client.IsReady(target) {
		t.Fatal("mTLS-required card left the target ready")
	}
}

func TestRemoteMTLSCallCarriesNoLocalCredential(t *testing.T) {
	f := newMTLSFixture(t, nil)
	client := f.client(t)
	target := f.target(t, WithClientTLS(f.clientCert, f.serverRoots, "cert-fp-1"))
	if _, err := client.ResolveAgentCard(t.Context(), target); err != nil {
		t.Fatalf("ResolveAgentCard: %v", err)
	}

	if _, err := client.Call(WithUser(t.Context(), "@alice:local.example"), target, "prompt", "", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if f.lastAuthHeader != "" {
		t.Fatalf("remote mTLS call carried Authorization %q, want none", f.lastAuthHeader)
	}
}
