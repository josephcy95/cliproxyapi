// Command gen_model_catalog rebuilds the embedded model catalog fallback from
// the public models.dev catalogs.
//
// The registry consumes models/model_catalog_fallback.json for model names that
// no static catalog knows about. It is also rebuilt at runtime on the periodic
// refresh, so this command only needs to be run to seed or inspect the
// committed snapshot:
//
//	go run ./cmd/gen_model_catalog
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	outputPath       = "internal/registry/models/model_catalog_fallback.json"
	canonicalURL     = "https://models.dev/models.json"
	providerURL      = "https://models.dev/api.json"
	maxCanonicalSize = 8 << 20
	maxProviderSize  = 32 << 20
)

func main() {
	out := flag.String("out", outputPath, "destination path for the generated catalog")
	flag.Parse()

	canonical, err := download(canonicalURL, maxCanonicalSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen_model_catalog: %v\n", err)
		os.Exit(1)
	}
	providers, err := download(providerURL, maxProviderSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen_model_catalog: %v\n", err)
		os.Exit(1)
	}

	generated := time.Now().UTC().Format(time.RFC3339)
	encoded, err := registry.BuildCatalogFallbackArtifact(canonical, providers, generated)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen_model_catalog: %v\n", err)
		os.Exit(1)
	}
	if err := registry.ValidateCatalogFallbackJSON(encoded); err != nil {
		fmt.Fprintf(os.Stderr, "gen_model_catalog: generated catalog is invalid: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen_model_catalog: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("gen_model_catalog: wrote %s (%d bytes, generated=%s)\n", *out, len(encoded), generated)
}

func download(url string, maxBytes int64) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			fmt.Fprintf(os.Stderr, "gen_model_catalog: close %s: %v\n", url, errClose)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: unexpected status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("fetch %s: response exceeded %d bytes", url, maxBytes)
	}
	return data, nil
}
