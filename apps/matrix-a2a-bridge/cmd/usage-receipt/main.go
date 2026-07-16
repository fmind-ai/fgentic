// Command usage-receipt serves, publishes, and verifies the federation usage-receipt contract.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentcardjws"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/usagereceipt"
)

const (
	maxFileBytes = 64 << 10
	maxGRPCBytes = 128 << 10
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "error: %v\n", err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf(
			"expected serve, public-jwk, request-hash, verify, or archive-count subcommand",
		)
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "public-jwk":
		return runPublicJWK(args[1:], stdout)
	case "request-hash":
		return runRequestHash(args[1:], stdout)
	case "verify":
		return runVerify(args[1:])
	case "archive-count":
		return runArchiveCount(args[1:], stdout)
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runRequestHash(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("request-hash", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inputPath := flags.String("input", "", "accepted A2A JSON request path")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse request-hash flags: %w", err)
	}
	if flags.NArg() != 0 || *inputPath == "" {
		return fmt.Errorf("request-hash requires --input")
	}
	raw, err := readBoundedFile(*inputPath, "A2A request")
	if err != nil {
		return err
	}
	hash, err := usagereceipt.RequestHash(raw)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, hash); err != nil {
		return fmt.Errorf("write A2A request hash: %w", err)
	}
	return nil
}

func runArchiveCount(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("archive-count", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archivePath := flags.String("archive", "", "append-only receipt archive path")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse archive-count flags: %w", err)
	}
	if flags.NArg() != 0 || *archivePath == "" {
		return fmt.Errorf("archive-count requires --archive")
	}
	file, err := os.Open(*archivePath)
	if os.IsNotExist(err) {
		_, err = fmt.Fprintln(stdout, 0)
		return err
	}
	if err != nil {
		return fmt.Errorf("open usage receipt archive: %w", err)
	}
	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil {
		closeErr := file.Close()
		scanErr := fmt.Errorf("scan usage receipt archive: %w", err)
		if closeErr != nil {
			return errors.Join(scanErr, fmt.Errorf("close usage receipt archive: %w", closeErr))
		}
		return scanErr
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close usage receipt archive: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, count); err != nil {
		return fmt.Errorf("write usage receipt archive count: %w", err)
	}
	return nil
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listenAddress := flags.String("listen", ":4444", "gRPC listen address")
	privateKeyPath := flags.String("private-key", "", "P-256 private key PEM path")
	keyID := flags.String("key-id", "", "protected JWS key ID")
	azp := flags.String("azp", "", "consumer identity already authorized by agentgateway")
	archivePath := flags.String("archive", "", "append-only JSONL archive path")
	pendingDir := flags.String("pending-dir", "", "long-task reservation evidence directory")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse serve flags: %w", err)
	}
	if flags.NArg() != 0 || *privateKeyPath == "" || *keyID == "" || *azp == "" ||
		*archivePath == "" || *pendingDir == "" {
		return fmt.Errorf("serve requires --private-key, --key-id, --azp, --archive, and --pending-dir")
	}
	key, err := readPrivateKey(*privateKeyPath)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return fmt.Errorf("listen for usage receipt external processor: %w", err)
	}
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxGRPCBytes),
		grpc.MaxSendMsgSize(maxGRPCBytes),
	)
	extprocv3.RegisterExternalProcessorServer(server, &usagereceipt.ExternalProcessor{
		Processor: &usagereceipt.Processor{
			AZP: *azp, KeyID: *keyID, Key: key,
			Archive: &usagereceipt.Archive{Path: *archivePath},
			Pending: &usagereceipt.PendingStore{Dir: *pendingDir},
			Now:     time.Now,
		},
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	slog.Info("usage receipt external processor listening", "address", listener.Addr(), "azp", *azp)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serveErr:
		return fmt.Errorf("serve usage receipt external processor: %w", err)
	case <-ctx.Done():
	}
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		server.Stop()
		<-stopped
	}
	return nil
}

func runPublicJWK(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("public-jwk", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	privateKeyPath := flags.String("private-key", "", "P-256 private key PEM path")
	keyID := flags.String("key-id", "", "protected JWS key ID")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse public-jwk flags: %w", err)
	}
	if flags.NArg() != 0 || *privateKeyPath == "" || *keyID == "" {
		return fmt.Errorf("public-jwk requires --private-key and --key-id")
	}
	key, err := readPrivateKey(*privateKeyPath)
	if err != nil {
		return err
	}
	jwk, err := agentcardjws.EncodePublicJWK(&key.PublicKey, *keyID)
	if err != nil {
		return err
	}
	if _, err := stdout.Write(append(jwk, '\n')); err != nil {
		return fmt.Errorf("write public JWK: %w", err)
	}
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inputPath := flags.String("input", "", "signed receipt JSON path")
	publicKeyPath := flags.String("public-key", "", "public P-256 JWK JSON path")
	keyID := flags.String("key-id", "", "expected protected JWS key ID")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse verify flags: %w", err)
	}
	if flags.NArg() != 0 || *inputPath == "" || *publicKeyPath == "" || *keyID == "" {
		return fmt.Errorf("verify requires --input, --public-key, and --key-id")
	}
	raw, err := readBoundedFile(*inputPath, "signed usage receipt")
	if err != nil {
		return err
	}
	signed, err := usagereceipt.Parse(raw)
	if err != nil {
		return err
	}
	jwk, err := readBoundedFile(*publicKeyPath, "public JWK")
	if err != nil {
		return err
	}
	key, err := agentcardjws.ParsePublicJWK(jwk, *keyID, agentcardjws.RequirePublicJWKMetadata)
	if err != nil {
		return err
	}
	return usagereceipt.Verify(signed, key, *keyID)
}

func readPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := readBoundedFile(path, "private key")
	if err != nil {
		return nil, err
	}
	return agentcardjws.ParseP256PrivateKeyPEM(raw)
}

func readBoundedFile(path, label string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read %s: %w", label, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close %s: %w", label, closeErr)
	}
	if len(raw) > maxFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, maxFileBytes)
	}
	return raw, nil
}
