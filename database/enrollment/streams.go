package enrollment

import (
	"context"

	"github.com/komari-monitor/komari/database/agentcredentials"
)

// TrackStream registers before checking the token. A revocation committed
// before registration is seen by the check, and a later one cancels the
// registered stream, including while the check is in progress.
func TrackStream(ctx context.Context, agentID, token string) (context.Context, context.CancelFunc, error) {
	ctx, stop := agentcredentials.TrackStream(ctx, agentID)
	id, _, err := LookupAccessTokenContext(ctx, token)
	if err == nil && id != agentID {
		err = ErrCredentialRejected
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		stop()
		return nil, nil, err
	}
	return ctx, stop, nil
}
