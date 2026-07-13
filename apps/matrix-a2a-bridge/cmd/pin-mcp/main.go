// Command pin-mcp updates and verifies versioned MCP server-surface pins.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fmind/matrix-a2a-bridge/internal/mcppin"
)

const maxPinBytes = 64 << 20

func main() {
	os.Exit(execute(os.Args[1:], os.Stderr))
}

func execute(args []string, stderr io.Writer) int {
	if err := run(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected check, update, or verify subcommand")
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:])
	case "update":
		return runUpdate(args[1:])
	case "verify":
		return runVerify(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; expected check, update, or verify", args[0])
	}
}

func runCheck(args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	pinPath := flags.String("pin", "", "committed MCP pin path")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse check flags: %w", err)
	}
	if flags.NArg() != 0 || *pinPath == "" || *pinPath == "-" {
		return fmt.Errorf("check requires --pin with no positional arguments")
	}
	_, err := readPin(*pinPath)
	return err
}

func runUpdate(args []string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "stable MCP backend name")
	endpoint := flags.String("endpoint", "", "MCP endpoint to collect (never serialized)")
	outputPath := flags.String("output", "", "pin file to update atomically")
	image := flags.String("image", "", "immutable backend image reference")
	discoveryURL := flags.String("discovery-url", "", "controller discovery URL")
	discoveryProtocol := flags.String("discovery-protocol", "", "controller discovery transport")
	backendHost := flags.String("backend-host", "", "governed backend host")
	backendPort := flags.Int("backend-port", 0, "governed backend port")
	backendPath := flags.String("backend-path", "", "governed backend path")
	backendProtocol := flags.String("backend-protocol", "", "governed backend transport")
	var command stringList
	var arguments stringList
	flags.Var(&command, "command", "ordered container command element; repeat for each element")
	flags.Var(&arguments, "argument", "ordered container argument; repeat for each argument")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse update flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("update does not accept positional arguments")
	}
	if *name == "" || *endpoint == "" || *outputPath == "" || *outputPath == "-" ||
		*image == "" || len(command) == 0 || *discoveryURL == "" || *discoveryProtocol == "" ||
		*backendHost == "" || *backendPort == 0 || *backendPath == "" || *backendProtocol == "" {
		return fmt.Errorf("update requires --name, --endpoint, --output, complete execution provenance, and complete routing provenance")
	}

	file, err := readOptionalPin(*outputPath)
	if err != nil {
		return err
	}
	surface, err := mcppin.Collect(context.Background(), *endpoint)
	if err != nil {
		return fmt.Errorf("collect server %q: %w", *name, err)
	}
	server, err := mcppin.NewServer(*name, mcppin.Provenance{
		Image:     *image,
		Command:   slices.Clone(command),
		Arguments: slices.Clone(arguments),
		Discovery: mcppin.Discovery{URL: *discoveryURL, Protocol: *discoveryProtocol},
		Backend: mcppin.Backend{
			Host: *backendHost, Port: *backendPort, Path: *backendPath, Protocol: *backendProtocol,
		},
	}, surface)
	if err != nil {
		return fmt.Errorf("build server %q pin: %w", *name, err)
	}

	servers := slices.Clone(file.Servers)
	replaced := false
	for index := range servers {
		if servers[index].Name == *name {
			servers[index] = server
			replaced = true
			break
		}
	}
	if !replaced {
		servers = append(servers, server)
	}
	file, err = mcppin.NewFile(servers)
	if err != nil {
		return fmt.Errorf("build pin file: %w", err)
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pin file: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxPinBytes {
		return fmt.Errorf("encoded pin exceeds %d bytes", maxPinBytes)
	}
	return writeAtomic(*outputPath, encoded, 0o644)
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "stable MCP backend name")
	endpoint := flags.String("endpoint", "", "live MCP endpoint to compare")
	pinPath := flags.String("pin", "", "committed MCP pin path")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse verify flags: %w", err)
	}
	if flags.NArg() != 0 || *name == "" || *endpoint == "" || *pinPath == "" || *pinPath == "-" {
		return fmt.Errorf("verify requires --name, --endpoint, and --pin with no positional arguments")
	}

	pinnedFile, err := readPin(*pinPath)
	if err != nil {
		return err
	}
	pinned, found := serverByName(pinnedFile, *name)
	if !found {
		return fmt.Errorf("pin has no server %q", *name)
	}
	surface, err := mcppin.Collect(context.Background(), *endpoint)
	if err != nil {
		return fmt.Errorf("collect server %q: %w", *name, err)
	}
	observed, err := mcppin.NewServer(*name, pinned.Provenance, surface)
	if err != nil {
		return fmt.Errorf("build observed server %q pin: %w", *name, err)
	}
	expectedFile, err := mcppin.NewFile([]mcppin.Server{pinned})
	if err != nil {
		return fmt.Errorf("build expected comparison: %w", err)
	}
	observedFile, err := mcppin.NewFile([]mcppin.Server{observed})
	if err != nil {
		return fmt.Errorf("build observed comparison: %w", err)
	}
	changes, err := mcppin.Compare(expectedFile, observedFile)
	if err != nil {
		return fmt.Errorf("compare MCP surface: %w", err)
	}
	if len(changes) == 0 {
		return nil
	}
	var report strings.Builder
	report.WriteString("MCP surface drift")
	for _, change := range changes {
		report.WriteString("\n- ")
		report.WriteString(change.String())
	}
	return errors.New(report.String())
}

func readOptionalPin(path string) (mcppin.File, error) {
	file, err := readPin(path)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return mcppin.File{}, err
	}
	return mcppin.NewFile(nil)
}

func readPin(path string) (mcppin.File, error) {
	content, err := readBoundedFile(path, maxPinBytes)
	if err != nil {
		return mcppin.File{}, err
	}
	file, err := mcppin.Parse(content)
	if err != nil {
		return mcppin.File{}, fmt.Errorf("parse pin %q: %w", path, err)
	}
	return file, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open pin %q: %w", path, err)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read pin %q: %w", path, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close pin %q: %w", path, closeErr)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("pin %q exceeds %d bytes", path, limit)
	}
	return content, nil
}

func serverByName(file mcppin.File, name string) (mcppin.Server, bool) {
	for _, server := range file.Servers {
		if server.Name == name {
			return server, true
		}
	}
	return mcppin.Server{}, false
}

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func writeAtomic(path string, content []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".pin-mcp-*")
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
	if err := temporary.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary output: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}
	return nil
}
