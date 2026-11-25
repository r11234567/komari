package trafficledger

import (
	"context"
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/trafficreset"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func trafficPolicyVersions(db *gorm.DB, client models.Client, start, end time.Time) ([]models.TrafficPolicyVersion, error) {
	var active models.TrafficPolicyVersion
	if err := db.Where("client = ? AND effective <= ?", client.UUID, start.UnixNano()).Order("effective DESC").Take(&active).Error; err != nil {
		return nil, err
	}
	var newer []models.TrafficPolicyVersion
	if err := db.Where("client = ? AND effective > ? AND effective <= ?", client.UUID, start.UnixNano(), end.UnixNano()).
		Order("effective").Find(&newer).Error; err != nil {
		return nil, err
	}
	var next models.TrafficPolicyVersion
	err := db.Where("client = ? AND effective > ?", client.UUID, end.UnixNano()).Order("effective").Take(&next).Error
	if err == nil {
		newer = append(newer, next)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return append([]models.TrafficPolicyVersion{active}, newer...), nil
}

func (r policyRange) at(at time.Time) trafficreset.Schedule {
	s := r.legacy
	for i, row := range r.versions {
		if row.Effective != 0 && time.Unix(0, row.Effective).After(at) {
			s.EndsAt = time.Unix(0, row.Effective).UTC()
			break
		}
		s = trafficreset.FromFields(&row.Day, row.Clock, row.Timezone)
		if row.Effective != 0 || i != 0 {
			s.StartsAt = time.Unix(0, row.Effective).UTC()
		}
	}
	return s
}

type policyRange struct {
	legacy   trafficreset.Schedule
	versions []models.TrafficPolicyVersion
}

func DisplayTrafficSchedule(client models.Client) trafficreset.Schedule {
	schedule := clientSchedule(client)
	if client.TrafficPolicyStart != nil {
		schedule.StartsAt = *client.TrafficPolicyStart
	}
	if client.TrafficPolicyEnd != nil {
		schedule.EndsAt = *client.TrafficPolicyEnd
	}
	return schedule
}

func loadTrafficPolicies(db *gorm.DB, client models.Client, start, end time.Time) (policyRange, error) {
	r := policyRange{legacy: clientSchedule(client)}
	if !client.TrafficPolicyManaged {
		return r, nil
	}
	rows, err := trafficPolicyVersions(db, client, start, end)
	r.versions = rows
	return r, err
}

func TrafficScheduleAt(ctx context.Context, db *gorm.DB, client models.Client, at time.Time) (trafficreset.Schedule, error) {
	policies, err := loadTrafficPolicies(db.WithContext(ctx), client, at, at)
	return policies.at(at), err
}

func SaveTrafficPolicy(ctx context.Context, db *gorm.DB, id string, day int, clock, timezone string, now time.Time) error {
	telemetryTrafficMu.Lock()
	defer telemetryTrafficMu.Unlock()
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var client models.Client
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("uuid = ?", id).Take(&client).Error; err != nil {
			return err
		}
		policies, err := loadTrafficPolicies(tx, client, now, now)
		if err != nil {
			return err
		}
		active := policies.at(now)
		if !client.TrafficPolicyManaged {
			legacy := models.TrafficPolicyVersion{Client: id, Day: active.Day, Clock: trafficreset.FormatClock(active.Hour, active.Minute, active.Second), Timezone: active.Timezone}
			if err := tx.Create(&legacy).Error; err != nil {
				return err
			}
			if err := tx.Model(&client).Update("traffic_policy_managed", true).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("client = ? AND effective > ?", id, now.UnixNano()).Delete(&models.TrafficPolicyVersion{}).Error; err != nil {
			return err
		}
		if active.Day == day && trafficreset.FormatClock(active.Hour, active.Minute, active.Second) == clock && active.Timezone == timezone {
			return nil
		}
		effective := active.Next(now)
		if effective.IsZero() {
			effective = now.Add(30 * time.Second).Truncate(time.Minute).Add(time.Minute)
		} else if !effective.After(now.Add(30 * time.Second)) {
			// The ingest endpoint accepts up to 30 seconds of clock skew.
			// Keep the ownership of samples that might already have landed.
			effective = active.Next(effective)
		}
		row := models.TrafficPolicyVersion{Client: id, Effective: effective.UnixNano(), Day: day, Clock: clock, Timezone: timezone}
		return tx.Create(&row).Error
	})
	if err == nil {
		InvalidateCalibratedCycleCache()
	}
	return err
}

func PendingTrafficPolicy(ctx context.Context, db *gorm.DB, client models.Client, now time.Time) (*models.TrafficPolicyVersion, error) {
	if !client.TrafficPolicyManaged {
		return nil, nil
	}
	var row models.TrafficPolicyVersion
	err := db.WithContext(ctx).Where("client = ? AND effective > ?", client.UUID, now.UnixNano()).Order("effective").Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}
