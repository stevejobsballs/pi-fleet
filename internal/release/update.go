package release

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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Manifest lists a release's artifacts (DESIGN.md §9.2). It is signed as
// manifest.json + manifest.json.minisig.
type Manifest struct {
	Version string `json:"version"`
	// MinUpgradeFrom is the oldest version that may upgrade straight to
	// this one (schema migrations assume it).
	MinUpgradeFrom string     `json:"min_upgrade_from,omitempty"`
	Date           string     `json:"date"`
	Security       bool       `json:"security"`
	Artifacts      []Artifact `json:"artifacts"`
}

// Artifact is one binary.
type Artifact struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

const (
	ManifestFile = "manifest.json"
	SigFile      = "manifest.json.minisig"
)

var versionRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// ParseVersion parses vMAJOR.MINOR.PATCH. Anything else (development
// builds) parses as v0.0.0, so any release is newer.
func ParseVersion(v string) [3]int {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out
}

// Compare returns -1, 0 or 1.
func Compare(a, b string) int {
	va, vb := ParseVersion(a), ParseVersion(b)
	for i := range va {
		switch {
		case va[i] < vb[i]:
			return -1
		case va[i] > vb[i]:
			return 1
		}
	}
	return 0
}

// Source fetches release files from a directory (a USB stick, for
// air-gapped installs) or a URL (central's mirror).
type Source struct {
	Base string
	HTTP *http.Client
}

func (s Source) fetch(ctx context.Context, name string, limit int64) ([]byte, error) {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return nil, fmt.Errorf("release: bad file name %q", name)
	}
	if !strings.HasPrefix(s.Base, "http://") && !strings.HasPrefix(s.Base, "https://") {
		f, err := os.Open(filepath.Join(s.Base, name))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, limit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.Base, "/")+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: GET %s: %s", name, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Errors from Update.
var (
	ErrNoKeys       = errors.New("release: this build has no release keys compiled in, so it cannot verify updates")
	ErrNotNewer     = errors.New("release: not newer than the installed version (use -allow-downgrade to override)")
	ErrTooOld       = errors.New("release: the installed version is too old to upgrade straight to this release")
	ErrNoArtifact   = errors.New("release: no artifact for this platform")
	ErrHealthFailed = errors.New("release: the new version failed its health check and was rolled back")
)

// Options configure an update.
type Options struct {
	Source  Source
	Trusted []PublicKey
	// Root holds releases/<version>/pi-fleet and the current symlink.
	Root string
	// DataDir is checked by the new binary's selfcheck.
	DataDir        string
	CurrentVersion string
	AllowDowngrade bool
	// Snapshot copies the database before the switch and returns a
	// function that restores it.
	Snapshot func(ctx context.Context) (restore func() error, err error)
	// HealthTimeout bounds the new binary's selfcheck (default 120s).
	HealthTimeout time.Duration
	OS, Arch      string // default runtime.GOOS/GOARCH
}

// Result describes a completed update.
type Result struct {
	Manifest Manifest
	Binary   string
	Previous string
}

// Update verifies, installs, switches to and health-checks a release,
// rolling back the binary and the database if the check fails
// (DESIGN.md §9.3).
func Update(ctx context.Context, o Options) (Result, error) {
	var res Result
	if len(o.Trusted) == 0 {
		return res, ErrNoKeys
	}
	if o.OS == "" {
		o.OS, o.Arch = runtime.GOOS, runtime.GOARCH
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 120 * time.Second
	}
	body, err := o.Source.fetch(ctx, ManifestFile, 1<<20)
	if err != nil {
		return res, err
	}
	sig, err := o.Source.fetch(ctx, SigFile, 64<<10)
	if err != nil {
		return res, err
	}
	if _, err := Verify(o.Trusted, body, sig); err != nil {
		return res, err
	}
	m := &res.Manifest
	if err := json.Unmarshal(body, m); err != nil {
		return res, fmt.Errorf("release: manifest: %w", err)
	}
	if !versionRE.MatchString(m.Version) {
		return res, fmt.Errorf("release: bad version %q", m.Version)
	}
	if Compare(m.Version, o.CurrentVersion) <= 0 && !o.AllowDowngrade {
		return res, fmt.Errorf("%w: offered %s, installed %s", ErrNotNewer, m.Version, o.CurrentVersion)
	}
	if m.MinUpgradeFrom != "" && Compare(o.CurrentVersion, m.MinUpgradeFrom) < 0 && ParseVersion(o.CurrentVersion) != [3]int{} {
		return res, fmt.Errorf("%w: needs %s or later", ErrTooOld, m.MinUpgradeFrom)
	}
	var art *Artifact
	for i := range m.Artifacts {
		if m.Artifacts[i].OS == o.OS && m.Artifacts[i].Arch == o.Arch {
			art = &m.Artifacts[i]
		}
	}
	if art == nil {
		return res, fmt.Errorf("%w (%s/%s)", ErrNoArtifact, o.OS, o.Arch)
	}
	bin, err := o.Source.fetch(ctx, art.Name, 256<<20)
	if err != nil {
		return res, err
	}
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != art.SHA256 || int64(len(bin)) != art.Size {
		return res, fmt.Errorf("release: %s does not match the signed manifest", art.Name)
	}

	dir := filepath.Join(o.Root, "releases", m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	res.Binary = filepath.Join(dir, "pi-fleet")
	if err := writeExecutable(res.Binary, bin); err != nil {
		return res, err
	}

	restore := func() error { return nil }
	if o.Snapshot != nil {
		if restore, err = o.Snapshot(ctx); err != nil {
			return res, fmt.Errorf("release: pre-update snapshot: %w", err)
		}
	}
	current := filepath.Join(o.Root, "current")
	res.Previous, _ = os.Readlink(current)
	if err := switchLink(current, dir); err != nil {
		return res, err
	}
	if err := healthCheck(ctx, res.Binary, o.DataDir, o.HealthTimeout); err != nil {
		var errs []error
		if res.Previous != "" {
			errs = append(errs, switchLink(current, res.Previous))
		}
		errs = append(errs, restore())
		if rerr := errors.Join(errs...); rerr != nil {
			return res, fmt.Errorf("%w: %v; ROLLBACK ALSO FAILED: %v", ErrHealthFailed, err, rerr)
		}
		return res, fmt.Errorf("%w: %v", ErrHealthFailed, err)
	}
	return res, nil
}

func writeExecutable(path string, b []byte) error {
	part := path + ".part"
	if err := os.WriteFile(part, b, 0o755); err != nil {
		return err
	}
	f, err := os.Open(part)
	if err == nil {
		f.Sync()
		f.Close()
	}
	return os.Rename(part, path)
}

// switchLink atomically points link at target.
func switchLink(link, target string) error {
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// healthCheck runs the new binary's selfcheck, which applies migrations
// and verifies every chain.
func healthCheck(ctx context.Context, bin, dataDir string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "selfcheck", "-data", dataDir)
	cmd.Stdout, cmd.Stderr = &out, &out
	// Updates run as root; the check must run as the data directory's
	// owner so SQLite doesn't leave root-owned files the service can't open.
	if info, err := os.Stat(dataDir); err == nil && os.Geteuid() == 0 {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: st.Uid, Gid: st.Gid}}
		}
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// BuildManifest describes the artifacts in dir named
// pi-fleet_<version>_<os>_<arch>.
func BuildManifest(dir, version string, minFrom string, security bool, now time.Time) (Manifest, error) {
	if !versionRE.MatchString(version) {
		return Manifest{}, fmt.Errorf("release: version %q must look like v1.2.3", version)
	}
	m := Manifest{Version: version, MinUpgradeFrom: minFrom, Date: now.UTC().Format(time.DateOnly), Security: security}
	matches, err := filepath.Glob(filepath.Join(dir, "pi-fleet_"+version+"_*_*"))
	if err != nil {
		return m, err
	}
	for _, p := range matches {
		name := filepath.Base(p)
		parts := strings.Split(strings.TrimPrefix(name, "pi-fleet_"+version+"_"), "_")
		if len(parts) != 2 || strings.HasSuffix(name, ".minisig") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return m, err
		}
		sum := sha256.Sum256(b)
		m.Artifacts = append(m.Artifacts, Artifact{Name: name, OS: parts[0], Arch: parts[1], SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))})
	}
	if len(m.Artifacts) == 0 {
		return m, fmt.Errorf("release: no pi-fleet_%s_<os>_<arch> artifacts in %s", version, dir)
	}
	return m, nil
}

// SignManifest writes manifest.json and its signature into dir.
func SignManifest(dir string, m Manifest, sk SecretKey, now time.Time) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	sig, err := Sign(sk, body, timestampComment(now, ManifestFile))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), body, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, SigFile), []byte(sig), 0o644)
}
