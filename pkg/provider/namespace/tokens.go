package namespace

import (
	"context"
	"errors"
	"time"

	"namespacelabs.dev/integrations/api"
	"namespacelabs.dev/integrations/auth"
)

// Both Connect requests and the SSH gateway use this source. Check every
// issued token, including a refresh after a login/credential-file change.
// Namespace authenticates the token; the local claim check prevents an
// otherwise valid credential for a different tenant from selecting resources.
type tenantTokens struct {
	tenantID string
	file     string
	source   api.TokenSource
}

func (t tenantTokens) IssueToken(ctx context.Context, minDuration time.Duration, force bool) (string, error) {
	src := t.source
	if src == nil {
		var err error
		if t.file != "" {
			src, err = auth.LoadLocalToken(t.file)
		} else {
			src, err = auth.LoadDefaults()
		}
		if err != nil {
			return "", errors.New("namespace: no credential available for configured tenant")
		}
	}
	token, err := src.IssueToken(ctx, minDuration, force)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("namespace: credential source could not issue tenant token")
	}
	claims, err := auth.ExtractClaims(token)
	if err != nil || claims.TenantID != t.tenantID {
		return "", errors.New("namespace: credential does not identify the configured tenant")
	}
	return token, nil
}
