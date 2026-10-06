// Command vendor downloads the SecLists common-password list at a pinned
// commit, checks its SHA-256, keeps the lower-cased entries of at least
// domain.PasswordMinLengthFloor characters and writes them, sorted and
// gzip-compressed, next to the commonpw package, with the SecLists licence.
// It runs from `make vendor-passwords`; its output is committed.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/identity/domain"
)

// file is file (not imported: the package embeds the file this
// tool writes).
const file = "common-passwords.txt.gz"

const (
	base     = "https://raw.githubusercontent.com/danielmiessler/SecLists/"
	listPath = "/Passwords/Common-Credentials/xato-net-10-million-passwords-100000.txt"
)

func main() {
	commit := flag.String("commit", "", "SecLists commit")
	sum := flag.String("sha256", "", "expected SHA-256 of the list file")
	out := flag.String("out", "internal/identity/infra/commonpw", "output directory")
	flag.Parse()

	if err := run(*commit, *sum, *out); err != nil {
		fmt.Fprintln(os.Stderr, "vendor:", err)
		os.Exit(1)
	}
}

func run(commit, sum, out string) error {
	if commit == "" || sum == "" {
		return fmt.Errorf("-commit and -sha256 are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	list, err := fetch(ctx, base+commit+listPath)
	if err != nil {
		return err
	}

	if got := sha256.Sum256(list); hex.EncodeToString(got[:]) != sum {
		return fmt.Errorf("list SHA-256 is %x, want %s", got, sum)
	}

	license, err := fetch(ctx, base+commit+"/LICENSE")
	if err != nil {
		return err
	}

	var entries []string

	for line := range strings.Lines(string(list)) {
		line = strings.ToLower(strings.TrimRight(line, "\r\n"))
		if utf8.ValidString(line) && utf8.RuneCountInString(line) >= domain.PasswordMinLengthFloor {
			entries = append(entries, line)
		}
	}

	slices.Sort(entries)
	entries = slices.Compact(entries)

	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if _, err := io.WriteString(zw, e+"\n"); err != nil {
			return err
		}
	}

	if err := zw.Close(); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(out, file), buf.Bytes(), 0o644); err != nil {
		return err
	}

	header := "SecLists (https://github.com/danielmiessler/SecLists), commit " + commit + "\n" + listPath + "\n\n"
	if err := os.WriteFile(filepath.Join(out, "LICENSE.SecLists"), append([]byte(header), license...), 0o644); err != nil {
		return err
	}

	fmt.Printf("%d entries written\n", len(entries))

	return nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}

	return io.ReadAll(io.LimitReader(res.Body, 16<<20))
}
