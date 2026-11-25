// Ported from ytrack (github.com/BrainerVirus/youtrack-cli, internal/upgrade/sigstore.go).

package update

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Identity is the signer a bundle's Fulcio certificate must name.
type Identity struct {
	Issuer        string // OIDC issuer, matched exactly
	SubjectRegexp string // certificate SAN (for GitHub Actions, the workflow URI)
}

// ReleaseIdentity is this repository's release workflow on GitHub Actions.
// semantic-release cuts a version from a push to main (not from a tag push),
// so the workflow runs, and signs, as refs/heads/main.
var ReleaseIdentity = Identity{
	Issuer:        "https://token.actions.githubusercontent.com",
	SubjectRegexp: `^https://github\.com/BrainerVirus/sonarless-mcp/\.github/workflows/release\.yml@refs/heads/main$`,
}

// Trust is what a release signature must verify against.
type Trust struct {
	Material root.TrustedMaterial
	Identity Identity
	// NoSCT skips the certificate transparency (SCT) check. Only for test
	// Sigstores, whose certificates carry no SCT; never set in production.
	NoSCT bool
}

var errSignature = errors.New("signature verification failed")

// PublicGoodTrustedRoot returns Sigstore's public-good trusted root (Fulcio
// CAs, Rekor logs, CT logs, timestamp authorities), fetched and verified
// through Sigstore's TUF repository from the TUF root that sigstore-go
// embeds. The TUF metadata is cached in cacheDir/sigstore-tuf, so key
// rotations reach sonarless-mcp without a new release.
func PublicGoodTrustedRoot(cacheDir string) (root.TrustedMaterial, error) {
	opts := tuf.DefaultOptions()
	if cacheDir != "" {
		opts = opts.WithCachePath(filepath.Join(cacheDir, "sigstore-tuf"))
	}
	tr, err := root.FetchTrustedRootWithOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot fetch the Sigstore trusted root from its TUF repository (%s); "+
			"behind a proxy, set HTTPS_PROXY: %w", errSignature, opts.RepositoryBaseURL, err)
	}
	return tr, nil
}

// VerifyBundle checks with sigstore-go that bundleJSON (a Sigstore bundle, as
// `cosign sign-blob --bundle` writes it) is a keyless signature of artifact
// by t.Identity: the certificate chains to a trusted Fulcio CA at a time
// attested by a transparency log entry or a signed timestamp, carries a valid
// SCT, names the identity, and its key signed the artifact's SHA-256. Any
// failed check is an error.
func VerifyBundle(bundleJSON, artifact []byte, t Trust) error {
	if t.Material == nil || t.Identity.Issuer == "" || t.Identity.SubjectRegexp == "" {
		return fmt.Errorf("%w: verification is not configured", errSignature)
	}
	b := &bundle.Bundle{}
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("%w: invalid bundle: %w", errSignature, err)
	}
	opts := []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	if !t.NoSCT {
		opts = append(opts, verify.WithSignedCertificateTimestamps(1))
	}
	v, err := verify.NewVerifier(t.Material, opts...)
	if err != nil {
		return fmt.Errorf("%w: %w", errSignature, err)
	}
	id, err := verify.NewShortCertificateIdentity(t.Identity.Issuer, "", "", t.Identity.SubjectRegexp)
	if err != nil {
		return fmt.Errorf("%w: %w", errSignature, err)
	}
	digest := sha256.Sum256(artifact)
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest[:]), verify.WithCertificateIdentity(id))
	if _, err := v.Verify(b, policy); err != nil {
		return fmt.Errorf("%w: %w", errSignature, err)
	}
	return nil
}
