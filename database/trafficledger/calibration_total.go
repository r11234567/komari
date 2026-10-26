package trafficledger

import (
	"context"
	"fmt"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
	"math/big"
	"strings"
	"time"
)

// A provider total does not identify measured directional usage. Keep the
// measured counters raw and record how the accounting correction is allocated.
// Sum preserves the effective ratio; max/min preserve the uncharged direction
// unless bounding it is necessary to make the selected billable total exact.
func CalibrationTargetsForTotal(kind string, current Usage, target int64) (Usage, error) {
	if target < 0 || current.Up < 0 || current.Down < 0 {
		return Usage{}, fmt.Errorf("traffic totals cannot be negative")
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	result := current
	switch kind {
	case "sum":
		sum := new(big.Int).Add(big.NewInt(current.Up), big.NewInt(current.Down))
		if sum.Sign() == 0 {
			return Usage{Up: target}, nil
		}
		up := new(big.Int).Mul(big.NewInt(target), big.NewInt(current.Up))
		up.Quo(up, sum)
		result = Usage{Up: up.Int64(), Down: target - up.Int64()}
	case "up":
		result.Up = target
	case "down":
		result.Down = target
	case "max":
		if current.Up >= current.Down {
			result.Up = target
			result.Down = min(current.Down, target)
		} else {
			result.Down = target
			result.Up = min(current.Up, target)
		}
	case "min":
		if current.Up <= current.Down {
			result.Up = target
			result.Down = max(current.Down, target)
		} else {
			result.Down = target
			result.Up = max(current.Up, target)
		}
	default:
		return Usage{}, fmt.Errorf("unsupported traffic counting type")
	}
	return result, nil
}

func CalibrateBillableCycle(ctx context.Context, db *gorm.DB, client models.Client, target int64, operator string, now time.Time) (CalibrationSnapshot, error) {
	calibrationWriteMu.Lock()
	defer calibrationWriteMu.Unlock()
	snapshot, err := LoadCalibrationSnapshot(ctx, db, client, now)
	if err != nil {
		return CalibrationSnapshot{}, err
	}
	kind := NormalizedCountingType(client.TrafficLimitType)
	values, err := CalibrationTargetsForTotal(kind, snapshot.Effective, target)
	if err != nil {
		return CalibrationSnapshot{}, err
	}
	offset := target - BillableUsage(kind, snapshot.Raw.Up, snapshot.Raw.Down)
	return calibrateCycle(ctx, db, client, values, operator, now, "total:"+kind, &offset, &target)
}
