package modules

import (
	"encoding/base64"
	"github.com/Real-kia/payesh/internal/trust"
)

// The package signing key stays outside source and ordinary CI. Only its
// public verification key is distributed with the core.
const ProcessSigningKeyID = "payesh-process-2026-09"
const ProcessSigningPublicKey = "31lzsQZNtdI2KXcHoc8LpM0iP7gM2BVep8RNmINmaBk"

func ProcessTrustAnchor() trust.Anchor {
	key, _ := base64.RawURLEncoding.DecodeString(ProcessSigningPublicKey)
	return trust.Anchor{KeyID: ProcessSigningKeyID, PublicKey: key}
}
