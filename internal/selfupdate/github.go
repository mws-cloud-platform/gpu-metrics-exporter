package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxReleaseJSON  = 4 << 20  // the release object, assets included
	maxPackageSize  = 64 << 20 // a .deb is ~2 MiB; anything near this is not ours
	apiTimeout      = 30 * time.Second
	downloadTimeout = 10 * time.Minute
)

// release is the part of GitHub's release object the update reads.
type release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	Assets     []asset `json:"assets"`
}

type asset struct {
	Name  string `json:"name"`
	State string `json:"state"` // "uploaded" once complete
	Size  int64  `json:"size"`
	// "sha256:<hex>", computed by GitHub when the asset was uploaded.
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// newHTTPClient follows redirects (a release download redirects to GitHub's
// asset storage) but never off https.
func newHTTPClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, CheckRedirect: httpsOnly}
}

func httpsOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing to follow a redirect to %s: not https", req.URL.Redacted())
	}
	return nil
}

// latestRelease asks GitHub for the repository's latest release: the most
// recent one that is neither a draft nor a prerelease, unless a maintainer
// has marked another as latest.
func (u *Updater) latestRelease(ctx context.Context) (*release, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.apiBase+"/repos/"+u.repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", u.userAgent())
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query the latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, u.apiError(resp)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSON)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode the latest release: %w", err)
	}
	if rel.Draft || rel.Prerelease {
		return nil, fmt.Errorf("GitHub returned %q as the latest release, but it is a draft or a prerelease", rel.TagName)
	}
	return &rel, nil
}

func (u *Updater) apiError(resp *http.Response) error {
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0" {
		msg := "GitHub API rate limit exceeded (unauthenticated requests are limited per IP address)"
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			msg += "; it resets at " + time.Unix(reset, 0).UTC().Format(time.RFC3339)
		}
		return errors.New(msg)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("github.com/%s has no published release", u.repo)
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	if body.Message != "" {
		return fmt.Errorf("query the latest release: GitHub API: %s: %q", resp.Status, body.Message)
	}
	return fmt.Errorf("query the latest release: GitHub API: %s", resp.Status)
}

// download fetches a into dir as name and checks it against the size and
// sha256 GitHub recorded when the asset was uploaded, returning its path.
//
// That proves the bytes are the ones the release holds, not who put them
// there: anyone who can upload to the repository's releases can publish a
// matching digest. It is the trust of a manual download from the release
// page, no more.
func (u *Updater) download(ctx context.Context, a asset, dir, name string) (string, error) {
	want, err := assetSHA256(a)
	if err != nil {
		return "", err
	}
	if a.Size <= 0 || a.Size > maxPackageSize {
		return "", fmt.Errorf("%s: GitHub lists an implausible size of %d bytes", a.Name, a.Size)
	}
	src, err := url.Parse(a.BrowserDownloadURL)
	if err != nil || src.Scheme != "https" {
		return "", fmt.Errorf("%s: refusing to download from %q: not an https URL", a.Name, a.BrowserDownloadURL)
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", u.userAgent())
	resp, err := u.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", a.Name, resp.Status)
	}

	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	// One byte past the listed size, so a longer body shows up as a mismatch.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, a.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.Name, err)
	}
	if n != a.Size {
		return "", fmt.Errorf("download %s: got %d bytes, GitHub lists %d", a.Name, n, a.Size)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return "", fmt.Errorf("download %s: sha256 %x does not match the %x GitHub recorded for it", a.Name, got, want)
	}
	return path, nil
}

func assetSHA256(a asset) ([]byte, error) {
	digits, ok := strings.CutPrefix(a.Digest, "sha256:")
	sum, err := hex.DecodeString(digits)
	if !ok || err != nil || len(sum) != sha256.Size {
		return nil, fmt.Errorf("%s: GitHub lists no sha256 digest for it (%q); refusing to install a package that cannot be checked", a.Name, a.Digest)
	}
	return sum, nil
}
