// Command sign-agent-card signs and verifies A2A v1.0 AgentCards without network access.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/fmind/matrix-a2a-bridge/internal/agentcardjws"
)

const (
	maxAgentCardBytes = 1 << 20
	maxKeyBytes       = 64 << 10
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdin, os.Stderr))
}

func execute(args []string, stdin io.Reader, stderr io.Writer) int {
	if err := run(args, stdin); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func run(args []string, stdin io.Reader) error {
	if len(args) == 0 {
		return fmt.Errorf("expected sign or verify subcommand")
	}
	switch args[0] {
	case "sign":
		return runSign(args[1:], stdin)
	case "verify":
		return runVerify(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; expected sign or verify", args[0])
	}
}

func runSign(args []string, stdin io.Reader) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inputPath := flags.String("input", "", "unsigned AgentCard JSON path")
	privateKeyPath := flags.String("private-key", "", "P-256 private key PEM path, or - for stdin")
	keyID := flags.String("key-id", "", "protected JWS key ID")
	outputPath := flags.String("output", "", "atomic signed-card bundle path")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse sign flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("sign does not accept positional arguments")
	}
	if *inputPath == "" || *privateKeyPath == "" || *keyID == "" || *outputPath == "" {
		return fmt.Errorf("sign requires --input, --private-key, --key-id, and --output")
	}
	if *inputPath == "-" {
		return fmt.Errorf("--input must be a file path")
	}
	if *outputPath == "-" {
		return fmt.Errorf("--output must be a file path for atomic publication")
	}
	if samePath(*outputPath, *inputPath) {
		return fmt.Errorf("--output must not replace --input")
	}
	if *privateKeyPath != "-" && samePath(*outputPath, *privateKeyPath) {
		return fmt.Errorf("--output must not replace --private-key")
	}

	card, err := readBoundedFile(*inputPath, maxAgentCardBytes, "AgentCard")
	if err != nil {
		return err
	}
	var privateKeyPEM []byte
	if *privateKeyPath == "-" {
		privateKeyPEM, err = readBounded(stdin, maxKeyBytes, "private key")
	} else {
		privateKeyPEM, err = readBoundedFile(*privateKeyPath, maxKeyBytes, "private key")
	}
	if err != nil {
		return err
	}
	key, err := agentcardjws.ParseP256PrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return err
	}
	bundle, err := agentcardjws.Sign(card, key, *keyID)
	if err != nil {
		return err
	}
	encoded, err := marshalBundle(bundle)
	if err != nil {
		return err
	}
	return writeAtomic(*outputPath, encoded, 0o644)
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inputPath := flags.String("input", "", "signed AgentCard JSON path")
	publicKeyPath := flags.String("public-key", "", "public P-256 JWK JSON path")
	keyID := flags.String("key-id", "", "expected protected JWS key ID")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse verify flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("verify does not accept positional arguments")
	}
	if *inputPath == "" || *publicKeyPath == "" || *keyID == "" {
		return fmt.Errorf("verify requires --input, --public-key, and --key-id")
	}
	if *inputPath == "-" || *publicKeyPath == "-" {
		return fmt.Errorf("verify inputs must be file paths")
	}

	card, err := readBoundedFile(*inputPath, maxAgentCardBytes, "AgentCard")
	if err != nil {
		return err
	}
	jwk, err := readBoundedFile(*publicKeyPath, maxKeyBytes, "public JWK")
	if err != nil {
		return err
	}
	key, err := agentcardjws.ParsePublicJWK(jwk, *keyID)
	if err != nil {
		return err
	}
	document, err := agentcardjws.Parse(card)
	if err != nil {
		return err
	}
	if _, err := document.Card(); err != nil {
		return err
	}
	return agentcardjws.Verify(document, key, *keyID)
}

func marshalBundle(bundle agentcardjws.Bundle) ([]byte, error) {
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode signed AgentCard bundle: %w", err)
	}
	return append(encoded, '\n'), nil
}

func readBoundedFile(path string, limit int64, label string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	content, readErr := readBounded(file, limit, label)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close %s: %w", label, closeErr)
	}
	return content, nil
}

func readBounded(reader io.Reader, limit int64, label string) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, limit)
	}
	return content, nil
}

func samePath(first, second string) bool {
	firstAbsolute, firstErr := filepath.Abs(first)
	secondAbsolute, secondErr := filepath.Abs(second)
	if firstErr == nil && secondErr == nil && filepath.Clean(firstAbsolute) == filepath.Clean(secondAbsolute) {
		return true
	}
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func writeAtomic(path string, content []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".sign-agent-card-*")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary output: %w", removeErr))
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary output mode: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write temporary output: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary output: %w", err)
	}
	closeErr := temporary.Close()
	closed = true
	if closeErr != nil {
		return fmt.Errorf("close temporary output: %w", closeErr)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}
	return nil
}
