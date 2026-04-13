package hostresource

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type ApplyResult struct {
	Changes []host.Change
	Changed bool
}

func singleChange(change *host.Change) ApplyResult {
	if change == nil {
		return ApplyResult{}
	}
	return ApplyResult{Changes: []host.Change{*change}, Changed: true}
}

func BoolPtr(value bool) *bool {
	return &value
}

func BytesSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
