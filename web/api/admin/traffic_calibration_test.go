package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrafficCalibrationRequiresOneCompleteInputMode(t *testing.T) {
	for _, body := range []string{`{"target_total":0}`, `{"target_total":50720000000}`, `{"target_up":0,"target_down":0}`} {
		var request trafficCalibrationRequest
		require.NoError(t, json.Unmarshal([]byte(body), &request))
		require.NoError(t, validateCalibrationRequest(request), body)
	}
	for _, body := range []string{`{}`, `{"target_up":0}`, `{"target_down":0}`, `{"target_total":-1}`, `{"target_up":-1,"target_down":0}`, `{"target_total":0,"target_up":0,"target_down":0}`} {
		var request trafficCalibrationRequest
		require.NoError(t, json.Unmarshal([]byte(body), &request))
		require.Error(t, validateCalibrationRequest(request), body)
	}
}
