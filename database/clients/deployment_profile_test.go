package clients

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/komari-monitor/komari/database/models"
	v2 "github.com/komari-monitor/komari/protocol/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDeploymentProfileRuntimeConfigExcludesInstallationOnlyFields(t *testing.T) {
	profile := DeploymentProfile{
		Platform:                "linux",
		DisableWebSSH:           true,
		DisableAutoUpdate:       true,
		IgnoreUnsafeCert:        true,
		GetIPAddrFromNIC:        true,
		EnableGHProxy:           true,
		GHProxy:                 "https://example.com",
		EnableCustomDir:         true,
		Dir:                     "/opt/custom-agent",
		EnableCustomServiceName: true,
		ServiceName:             "custom-agent",
		EnableInterval:          true,
		Interval:                15,
		EnableGPU:               true,
		DetailedGPU:             true,
		RemoteControlEnabled:    true,
	}
	if err := normalizeDeploymentProfile(&profile); err != nil {
		t.Fatalf("normalizeDeploymentProfile() error = %v", err)
	}
	encoded, err := json.Marshal(profile.RuntimeConfig())
	if err != nil {
		t.Fatalf("marshal runtime config: %v", err)
	}
	payload := string(encoded)
	for _, forbidden := range []string{
		"disable_web_ssh",
		"disable_auto_update",
		"ignore_unsafe_cert",
		"get_ip_addr_from_nic",
		"ghproxy",
		"dir",
		"service_name",
		"enable_gpu",
		"remote_control_enabled",
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("runtime config contains installation-only field %q: %s", forbidden, payload)
		}
	}
	if !strings.Contains(payload, `"interval":15`) {
		t.Fatalf("runtime config is missing interval: %s", payload)
	}
	if !strings.Contains(payload, `"detailed_gpu":true`) {
		t.Fatalf("runtime config is missing detailed GPU monitoring: %s", payload)
	}
}

func TestNormalizeDeploymentProfileRejectsInvalidRuntimeValues(t *testing.T) {
	profile := DeploymentProfile{
		Platform:       "linux",
		EnableInterval: true,
		Interval:       0.5,
	}
	if err := normalizeDeploymentProfile(&profile); err == nil {
		t.Fatal("expected invalid interval to be rejected")
	}

	profile = DeploymentProfile{
		Platform:          "linux",
		EnableMonthRotate: true,
		MonthRotate:       32,
	}
	if err := normalizeDeploymentProfile(&profile); err == nil {
		t.Fatal("expected invalid month rotation day to be rejected")
	}
}

func TestLegacyDeploymentProfileDefaultsRemoteControlFromHistoricalBehavior(t *testing.T) {
	var profile DeploymentProfile
	if err := json.Unmarshal([]byte(`{"platform":"linux","disable_web_ssh":false}`), &profile); err != nil {
		t.Fatalf("unmarshal legacy profile: %v", err)
	}
	if !profile.RemoteControlEnabled {
		t.Fatal("legacy profile unexpectedly disabled remote control")
	}
	if err := json.Unmarshal([]byte(`{"platform":"linux","remote_control_enabled":false}`), &profile); err != nil {
		t.Fatalf("unmarshal managed profile: %v", err)
	}
	if profile.RemoteControlEnabled {
		t.Fatal("explicit remote-control disable was ignored")
	}
}

func TestDeploymentProfileAndBillingEditorShareTrafficResetDay(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:deployment-profile-reset-day?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.Client{}, &models.ClientDeploymentProfile{}); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if err := db.Create(&models.Client{UUID: "node-a", Token: "token-a"}).Error; err != nil {
		t.Fatalf("create client: %v", err)
	}

	profile := DeploymentProfile{
		Platform:          "linux",
		EnableMonthRotate: true,
		MonthRotate:       17,
	}
	if _, err := saveDeploymentProfile(db, "node-a", profile); err != nil {
		t.Fatalf("save deployment profile: %v", err)
	}
	var client models.Client
	if err := db.Select("uuid", "traffic_reset_day").First(&client, "uuid = ?", "node-a").Error; err != nil {
		t.Fatalf("read client: %v", err)
	}
	if client.TrafficResetDay == nil || *client.TrafficResetDay != 17 {
		t.Fatalf("billing reset day = %v, want 17", client.TrafficResetDay)
	}

	if err := saveClient(db, map[string]interface{}{
		"uuid":              "node-a",
		"traffic_reset_day": float64(9),
	}); err != nil {
		t.Fatalf("save billing reset day: %v", err)
	}
	loaded, saved, err := getDeploymentProfile(db, "node-a")
	if err != nil {
		t.Fatalf("read deployment profile: %v", err)
	}
	if !saved || !loaded.EnableMonthRotate || loaded.MonthRotate != 9 {
		t.Fatalf("deployment reset day = enabled:%v day:%d, want enabled:true day:9", loaded.EnableMonthRotate, loaded.MonthRotate)
	}
}

func TestAdoptDeploymentRuntimeConfigInitializesAndReconcilesDelivery(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:deployment-profile-agent-state?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.Client{}, &models.ClientDeploymentProfile{}); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if err := db.Create(&models.Client{UUID: "node-state", Token: "token-state"}).Error; err != nil {
		t.Fatalf("create client: %v", err)
	}

	day, interval := 8, 12.0
	include, exclude, mounts := "eth0", "lo", "/;/data"
	memoryCache, gpu := true, true
	adopted, err := adoptDeploymentRuntimeConfig(db, "node-state", "linux", v2.ConfigParams{
		MonthRotate:        &day,
		Interval:           &interval,
		IncludeNics:        &include,
		ExcludeNics:        &exclude,
		IncludeMountpoints: &mounts,
		MemoryIncludeCache: &memoryCache,
		EnableGPU:          &gpu,
	})
	if err != nil {
		t.Fatalf("adopt runtime config: %v", err)
	}
	if !adopted {
		t.Fatal("first runtime config was not adopted")
	}
	profile, saved, err := getDeploymentProfile(db, "node-state")
	if err != nil {
		t.Fatalf("load adopted profile: %v", err)
	}
	if !saved || !profile.EnableMonthRotate || profile.MonthRotate != day ||
		!profile.EnableInterval || profile.Interval != interval ||
		!profile.EnableIncludeNics || profile.IncludeNics != include ||
		!profile.EnableExcludeNics || profile.ExcludeNics != exclude ||
		!profile.EnableIncludeMountpoints || profile.IncludeMountpoints != mounts ||
		!profile.MemoryIncludeCache || !profile.EnableGPU {
		t.Fatalf("incomplete adopted profile: %+v", profile)
	}

	staleInterval := 5.0
	adopted, err = adoptDeploymentRuntimeConfig(db, "node-state", "windows", v2.ConfigParams{Interval: &staleInterval})
	if err != nil {
		t.Fatalf("submit stale runtime config: %v", err)
	}
	if adopted {
		t.Fatal("stale runtime config overwrote a managed profile")
	}
	profile, _, err = getDeploymentProfile(db, "node-state")
	if err != nil {
		t.Fatalf("reload managed profile: %v", err)
	}
	if profile.Platform != "linux" || profile.Interval != interval {
		t.Fatalf("managed profile changed after stale report: %+v", profile)
	}

	if err := db.Create(&models.Client{UUID: "node-legacy-match", Token: "token-legacy-match"}).Error; err != nil {
		t.Fatalf("create matching legacy client: %v", err)
	}
	legacyMatch := DeploymentProfile{Platform: "linux", EnableInterval: true, Interval: 10}
	encodedMatch, err := json.Marshal(legacyMatch)
	if err != nil {
		t.Fatalf("encode matching legacy profile: %v", err)
	}
	if err := db.Create(&models.ClientDeploymentProfile{Client: "node-legacy-match", Config: string(encodedMatch)}).Error; err != nil {
		t.Fatalf("create matching legacy profile: %v", err)
	}
	matchingInterval := 10.0
	adopted, err = adoptDeploymentRuntimeConfig(db, "node-legacy-match", "linux", v2.ConfigParams{Interval: &matchingInterval})
	if err != nil || adopted {
		t.Fatalf("reconcile matching legacy profile = %v, %v", adopted, err)
	}
	var matchingState models.ClientDeploymentProfile
	if err := db.First(&matchingState, "client = ?", "node-legacy-match").Error; err != nil {
		t.Fatalf("load matching legacy state: %v", err)
	}
	if matchingState.Revision != 1 || matchingState.DeliveryStatus != DeploymentDeliveryApplied || matchingState.FinishedAt == nil {
		t.Fatalf("matching legacy delivery state = %+v", matchingState)
	}

	if err := db.Create(&models.Client{UUID: "node-legacy-mismatch", Token: "token-legacy-mismatch"}).Error; err != nil {
		t.Fatalf("create mismatching legacy client: %v", err)
	}
	legacyMismatch := DeploymentProfile{Platform: "linux", EnableInterval: true, Interval: 15}
	encodedMismatch, err := json.Marshal(legacyMismatch)
	if err != nil {
		t.Fatalf("encode mismatching legacy profile: %v", err)
	}
	if err := db.Create(&models.ClientDeploymentProfile{Client: "node-legacy-mismatch", Config: string(encodedMismatch)}).Error; err != nil {
		t.Fatalf("create mismatching legacy profile: %v", err)
	}
	reportedInterval := 20.0
	adopted, err = adoptDeploymentRuntimeConfig(db, "node-legacy-mismatch", "linux", v2.ConfigParams{Interval: &reportedInterval})
	if err != nil || adopted {
		t.Fatalf("reconcile mismatching legacy profile = %v, %v", adopted, err)
	}
	var mismatchingState models.ClientDeploymentProfile
	if err := db.First(&mismatchingState, "client = ?", "node-legacy-mismatch").Error; err != nil {
		t.Fatalf("load mismatching legacy state: %v", err)
	}
	if mismatchingState.Revision != 1 || mismatchingState.DeliveryStatus != DeploymentDeliverySaved || mismatchingState.FinishedAt != nil {
		t.Fatalf("mismatching legacy delivery state = %+v", mismatchingState)
	}
}

func TestDeploymentConfigDeliveryTracksOnlyRuntimeChangesAndRejectsStaleResults(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:deployment-profile-delivery?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.Client{}, &models.ClientDeploymentProfile{}); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if err := db.Create(&models.Client{UUID: "node-delivery", Token: "token-delivery"}).Error; err != nil {
		t.Fatalf("create client: %v", err)
	}

	profile := DeploymentProfile{Platform: "linux", EnableInterval: true, Interval: 10}
	_, state, runtimeChanged, err := saveDeploymentProfileForDispatch(db, "node-delivery", profile, false)
	if err != nil {
		t.Fatalf("save initial profile: %v", err)
	}
	if !runtimeChanged || state.Revision != 1 || state.Status != DeploymentDeliverySaved {
		t.Fatalf("initial delivery state = %+v, runtimeChanged=%v", state, runtimeChanged)
	}
	if marked, err := markDeploymentConfigSent(db, "node-delivery", 1); err != nil || !marked {
		t.Fatalf("mark sent = %v, %v", marked, err)
	}
	if completed, err := completeDeploymentConfig(db, "node-delivery", v2.ConfigResultParams{
		Revision: 1,
		Status:   DeploymentDeliveryApplied,
	}); err != nil || !completed {
		t.Fatalf("complete revision 1 = %v, %v", completed, err)
	}

	profile.DisableWebSSH = true
	_, state, runtimeChanged, err = saveDeploymentProfileForDispatch(db, "node-delivery", profile, false)
	if err != nil {
		t.Fatalf("save installation-only change: %v", err)
	}
	if runtimeChanged || state.Revision != 1 || state.Status != DeploymentDeliveryApplied {
		t.Fatalf("installation-only state = %+v, runtimeChanged=%v", state, runtimeChanged)
	}

	profile.Interval = 20
	_, state, runtimeChanged, err = saveDeploymentProfileForDispatch(db, "node-delivery", profile, false)
	if err != nil {
		t.Fatalf("save runtime change: %v", err)
	}
	if !runtimeChanged || state.Revision != 2 || state.Status != DeploymentDeliverySaved {
		t.Fatalf("second delivery state = %+v, runtimeChanged=%v", state, runtimeChanged)
	}
	if completed, err := completeDeploymentConfig(db, "node-delivery", v2.ConfigResultParams{
		Revision: 1,
		Status:   DeploymentDeliveryFailed,
		Error:    "stale result",
	}); err != nil || completed {
		t.Fatalf("stale result completion = %v, %v", completed, err)
	}
	if completed, err := completeDeploymentConfig(db, "node-delivery", v2.ConfigResultParams{
		Revision: 2,
		Status:   DeploymentDeliveryFailed,
		Error:    " invalid interface\n ",
	}); err != nil || !completed {
		t.Fatalf("complete revision 2 = %v, %v", completed, err)
	}

	_, state, runtimeChanged, err = saveDeploymentProfileForDispatch(db, "node-delivery", profile, false)
	if err != nil {
		t.Fatalf("retry failed profile: %v", err)
	}
	if !runtimeChanged || state.Revision != 3 || state.Status != DeploymentDeliverySaved {
		t.Fatalf("retry delivery state = %+v, runtimeChanged=%v", state, runtimeChanged)
	}
}

func TestPreviousRuntimeSurvivesRedispatchAndPreservesInstallSettings(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:deployment-profile-rollback?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&models.Client{}, &models.ClientDeploymentProfile{}); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if err := db.Create(&models.Client{UUID: "node-rollback", Token: "token-rollback"}).Error; err != nil {
		t.Fatalf("create client: %v", err)
	}

	profile := DeploymentProfile{
		Platform:             "linux",
		RuntimeIdentity:      AgentRuntimeIdentityServiceAccount,
		RemoteControlEnabled: false,
		RescueEnabled:        true,
		EnableCustomDir:      true,
		Dir:                  "/opt/current-agent",
		EnableInterval:       true,
		Interval:             10,
		EnableMonthRotate:    true,
		MonthRotate:          7,
		MemoryIncludeCache:   true,
	}
	if _, state, _, err := saveDeploymentProfileForDispatch(db, "node-rollback", profile, false); err != nil || state.Revision != 1 {
		t.Fatalf("save initial profile: state=%+v error=%v", state, err)
	}

	profile.Interval = 20
	profile.MonthRotate = 15
	profile.MemoryIncludeCache = false
	if _, state, _, err := saveDeploymentProfileForDispatch(db, "node-rollback", profile, false); err != nil || state.Revision != 2 {
		t.Fatalf("save changed runtime: state=%+v error=%v", state, err)
	}
	if _, state, _, err := saveDeploymentProfileForDispatch(db, "node-rollback", profile, true); err != nil || state.Revision != 3 {
		t.Fatalf("redispatch unchanged runtime: state=%+v error=%v", state, err)
	}

	rollback, available, err := previousDeploymentRuntimeProfile(db, "node-rollback")
	if err != nil || !available {
		t.Fatalf("load previous runtime: available=%v error=%v", available, err)
	}
	if rollback.Interval != 10 || rollback.MonthRotate != 7 || !rollback.MemoryIncludeCache {
		t.Fatalf("rollback runtime = %+v, want interval=10 reset-day=7 memory-cache=true", rollback.RuntimeConfig())
	}
	if rollback.Dir != profile.Dir || rollback.RuntimeIdentity != profile.RuntimeIdentity ||
		rollback.RemoteControlEnabled != profile.RemoteControlEnabled || rollback.RescueEnabled != profile.RescueEnabled {
		t.Fatalf("rollback changed install-only settings: %+v", rollback)
	}
	if _, state, changed, err := saveDeploymentProfileForDispatch(db, "node-rollback", rollback, true); err != nil || !changed || state.Revision != 4 {
		t.Fatalf("save rollback: state=%+v changed=%v error=%v", state, changed, err)
	}
}
