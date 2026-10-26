package renewal

import (
	"errors"
	"fmt"
	"time"

	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	messageevent "github.com/komari-monitor/komari/database/models/messageEvent"
	"github.com/komari-monitor/komari/pkg/expiry"
	"github.com/komari-monitor/komari/utils/messageSender"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
)

func CheckAndAutoRenewal(client models.Client) {
	if !client.AutoRenewal {
		return
	}
	if _, ok := agent_runtime.GetConnectedClients()[client.UUID]; !ok {
		return
	}
	if !expiry.IsFinite(client.ExpiredAt) {
		return
	}
	if client.BillingCycle <= 0 {
		return
	}

	now := time.Now().UTC()
	expiredAt := client.ExpiredAt.UTC()
	if !expiry.Due(now, expiredAt) {
		return
	}

	next, err := nextExpiry(client, now)
	if err != nil {
		auditlog.EventLog("renewal", fmt.Sprintf("Failed to renew %s: %v", client.Name, err))
		return
	}

	err = clients.SaveClientWithSource(map[string]interface{}{
		"uuid":              client.UUID,
		"expired_at":        next,
		"_match_expired_at": expiredAt,
	}, "renewal")
	if errors.Is(err, clients.ErrExpiryAlreadyChanged) {
		return
	}
	if err != nil {
		auditlog.EventLog("renewal", fmt.Sprintf("Failed to renew %s: %v", client.Name, err))
		return
	}

	until := expiry.FormatLocalDisplay(next, client.ExpiryTimezone)
	auditlog.EventLog("renewal", fmt.Sprintf("Auto-renewed %s until %s", client.Name, until))

	messageSender.SendEvent(models.EventMessage{
		Event:   messageevent.Renew,
		Clients: []models.Client{client},
		Time:    time.Now().UTC(),
		Emoji:   "🔄",
		Message: fmt.Sprintf("• %s until %s\n", client.Name, until),
	})
}

// nextExpiry returns the first expiry strictly after now. Overdue short
// cycles are caught up here so CheckAndAutoRenewal saves once.
func nextExpiry(client models.Client, now time.Time) (time.Time, error) {
	if client.ExpiredAt == nil {
		return time.Time{}, fmt.Errorf("expiry is not set")
	}
	return expiry.NextDue(*client.ExpiredAt, client.ExpiryTimezone, client.BillingCycle, now)
}
