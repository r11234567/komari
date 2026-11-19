package clients

import (
	"testing"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
)

func TestSaveClientInfoRejectsUnallowlistedFieldsAndEnforcesRemoteControlProtected(t *testing.T) {
	id, origToken, err := CreateClient()
	require.NoError(t, err)
	t.Cleanup(func() {
		dbcore.GetDBInstance().Where("client = ?", id).Delete(&models.ClientPrivilegedState{})
		dbcore.GetDBInstance().Where("client = ?", id).Delete(&models.ClientPrivilegedRevision{})
		dbcore.GetDBInstance().Where("uuid = ?", id).Delete(&models.Client{})
	})

	err = SaveClientInfo(map[string]interface{}{
		"uuid":                     id,
		"cpu_name":                 "Allowed CPU",
		"cpu_cores":                8,
		"token":                    "attacker-overwritten-token",
		"remote_control_protected": true,
		"name":                     "hijacked-name",
		"hidden":                   true,
		"price":                    9999,
	})
	require.NoError(t, err)

	client, err := GetClientByUUID(id)
	require.NoError(t, err)
	require.Equal(t, "Allowed CPU", client.CpuName)
	require.Equal(t, 8, client.CpuCores)
	require.Equal(t, origToken, client.Token)
	require.False(t, client.RemoteControlProtected)
	require.NotEqual(t, "hijacked-name", client.Name)
	require.False(t, client.Hidden)
	require.Equal(t, float64(0), client.Price)

	// Enable v2 privileged delivery for webssh/execution/root/rescue, then verify RemoteControlProtected blocks remote control.
	db := dbcore.GetDBInstance()
	cfg := `{"remote_control_enabled":true,"webssh_enabled":true,"execution_enabled":true,"root_execution_allowed":true,"rescue_helper_enabled":true}`
	require.NoError(t, db.Create(&models.ClientPrivilegedRevision{Client: id, Revision: 1, Config: cfg}).Error)
	require.NoError(t, db.Create(&models.ClientPrivilegedState{Client: id, ProtocolVersion: 2, AppliedRevision: 1, DesiredRevision: 1}).Error)

	allowed, err := PrivilegedCapabilityAllowedWithDB(db, id, "execution")
	require.NoError(t, err)
	require.True(t, allowed)

	require.NoError(t, db.Model(&models.Client{}).Where("uuid = ?", id).Update("remote_control_protected", true).Error)
	for _, feat := range []string{"remote", "webssh", "execution", "root"} {
		allowed, err = PrivilegedCapabilityAllowedWithDB(db, id, feat)
		require.NoError(t, err)
		require.False(t, allowed, "feature %s must be denied when remote_control_protected=true", feat)
	}
	rescueAllowed, err := PrivilegedCapabilityAllowedWithDB(db, id, "rescue")
	require.NoError(t, err)
	require.True(t, rescueAllowed)
}
