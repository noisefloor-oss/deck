package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

type principalKey struct{}

// The original single-user storage principal stays stable so existing
// settings and connection records remain attached to their owner.
const legacySingleUserPrincipal = "operator"

// Principal is an opaque authenticated identity for sensitive per-user
// capabilities. Empty means no authentication; a project or request header
// alone never establishes one. Password/OIDC have exactly one user.
func Principal(ctx context.Context) string { v, _ := ctx.Value(principalKey{}).(string); return v }
func withPrincipal(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, principalKey{}, principalHash(identity))
}

// ProxyPrincipal identifies the sole configured user of a Deck instance.
// Only Require may establish that identity on an incoming request.
func ProxyPrincipal(identity string) string { return principalHash("proxy:" + identity) }

func principalHash(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}
