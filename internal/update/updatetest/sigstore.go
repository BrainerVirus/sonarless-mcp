// Package updatetest provides Sigstore bundles signed by sigstore-go's
// virtual Sigstore and a fake GitHub release server for tests. Test-only.
//
// Ported from ytrack (github.com/BrainerVirus/youtrack-cli,
// internal/upgrade/upgradetest).
package updatetest

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"

	"github.com/BrainerVirus/sonarless-mcp/internal/update"
)

// ReleaseSubject is the certificate identity of the real release workflow.
const ReleaseSubject = "https://github.com/BrainerVirus/sonarless-mcp/.github/workflows/release.yml@refs/heads/main"

// GitHubIssuer is the GitHub Actions OIDC issuer.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// Sigstore is sigstore-go's in-memory Fulcio CA, Rekor log and timestamp
// authority.
type Sigstore struct {
	t  *testing.T
	VS *ca.VirtualSigstore
}

// NewSigstore returns a fresh virtual Sigstore.
func NewSigstore(t *testing.T) *Sigstore {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	return &Sigstore{t: t, VS: vs}
}

// Trust is what update.VerifyBundle should check against for this
// Sigstore: its trusted material and the release identity. Its certificates
// carry no SCT, so the SCT check is off.
func (s *Sigstore) Trust() update.Trust {
	return update.Trust{Material: s.VS, Identity: update.ReleaseIdentity, NoSCT: true}
}

// Sign returns a Sigstore bundle (v0.3 JSON, as cosign sign-blob --bundle
// writes it) signing artifact as subject, issued by issuer, with a Rekor
// entry and an RFC 3161 timestamp.
func (s *Sigstore) Sign(artifact []byte, subject, issuer string) []byte {
	return s.sign(artifact, subject, issuer, true)
}

// SignTlogOnly is Sign without the RFC 3161 timestamp: the Rekor v1 signed
// entry timestamp is the only trusted time.
func (s *Sigstore) SignTlogOnly(artifact []byte, subject, issuer string) []byte {
	return s.sign(artifact, subject, issuer, false)
}

func (s *Sigstore) sign(artifact []byte, subject, issuer string, withTSA bool) []byte {
	s.t.Helper()
	e, err := s.VS.Sign(subject, issuer, artifact)
	if err != nil {
		s.t.Fatal(err)
	}
	vc, err := e.VerificationContent()
	if err != nil {
		s.t.Fatal(err)
	}
	sc, err := e.SignatureContent()
	if err != nil {
		s.t.Fatal(err)
	}
	entries, err := e.TlogEntries()
	if err != nil {
		s.t.Fatal(err)
	}
	stamps, err := e.Timestamps()
	if err != nil {
		s.t.Fatal(err)
	}
	cert := vc.(*bundle.Certificate).Certificate()
	ms := sc.(*bundle.MessageSignature)

	// The virtual Sigstore keeps the entry kind and the Rekor v1 inclusion
	// promise (SET) outside its proto and makes no inclusion proof for
	// message signatures. A bundle carries all three, so add them: the proof
	// and its checkpoint from the virtual log (a one-entry tree, index 0) and
	// a SET over that index, signed by the same virtual Rekor key.
	tles := make([]*protorekor.TransparencyLogEntry, 0, len(entries))
	for _, en := range entries {
		tle := en.TransparencyLogEntry()
		proof, err := s.VS.GetInclusionProof(tle.CanonicalizedBody)
		if err != nil {
			s.t.Fatal(err)
		}
		rootHash, err := hex.DecodeString(*proof.RootHash)
		if err != nil {
			s.t.Fatal(err)
		}
		hashes := make([][]byte, 0, len(proof.Hashes))
		for _, h := range proof.Hashes {
			b, err := hex.DecodeString(h)
			if err != nil {
				s.t.Fatal(err)
			}
			hashes = append(hashes, b)
		}
		tle.LogIndex = *proof.LogIndex
		tle.InclusionProof = &protorekor.InclusionProof{
			LogIndex:   *proof.LogIndex,
			RootHash:   rootHash,
			TreeSize:   *proof.TreeSize,
			Hashes:     hashes,
			Checkpoint: &protorekor.Checkpoint{Envelope: *proof.Checkpoint},
		}
		set, err := s.VS.RekorSignPayload(tlog.RekorPayload{
			Body:           base64.StdEncoding.EncodeToString(tle.CanonicalizedBody),
			IntegratedTime: tle.IntegratedTime,
			LogIndex:       tle.LogIndex,
			LogID:          hex.EncodeToString(tle.LogId.KeyId),
		})
		if err != nil {
			s.t.Fatal(err)
		}
		tle.InclusionPromise = &protorekor.InclusionPromise{SignedEntryTimestamp: set}
		tle.KindVersion = &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}
		tles = append(tles, tle)
	}
	tvd := &protobundle.TimestampVerificationData{}
	if withTSA {
		for _, ts := range stamps {
			tvd.Rfc3161Timestamps = append(tvd.Rfc3161Timestamps, &protocommon.RFC3161SignedTimestamp{SignedTimestamp: ts})
		}
	}
	pb := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content:                   &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: cert.Raw}},
			TlogEntries:               tles,
			TimestampVerificationData: tvd,
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: ms.Digest()},
			Signature:     ms.Signature(),
		}},
	}
	b, err := bundle.NewBundle(pb)
	if err != nil {
		s.t.Fatal(err)
	}
	out, err := b.MarshalJSON()
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}
