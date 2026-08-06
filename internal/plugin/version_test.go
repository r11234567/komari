package plugin

import (
	"testing"

	"github.com/komari-monitor/komari/utils"
)

func TestCheckKomariVersion(t *testing.T) {
	currentVersion := utils.CurrentVersion
	utils.CurrentVersion = "0.0.1"
	t.Cleanup(func() { utils.CurrentVersion = currentVersion })

	tests := []struct {
		constraint string
		wantErr    bool
	}{
		{"", false},
		{"0.0.1", false},
		{"v0.0.1", false},
		{">=0.0.1", false},
		{">0.0.1", true},
		{"<1.0.0", false},
		{"<=0.0.1", false},
		{">=99.0.0", true},
		{"0.1", true}, // 0.1.0 > 0.0.1
		{"1", true},   // 1.0.0 > 0.0.1
		{"1.2.3.4", true},
		{"abc", true},
		{">", true},
	}
	for _, tt := range tests {
		err := CheckKomariVersion(tt.constraint)
		if (err != nil) != tt.wantErr {
			t.Errorf("CheckKomariVersion(%q) error = %v, wantErr %v", tt.constraint, err, tt.wantErr)
		}
	}
}

func TestCheckKomariVersionAcceptsRunningVersionSuffix(t *testing.T) {
	currentVersion := utils.CurrentVersion
	utils.CurrentVersion = "1.2.5-LTS1"
	t.Cleanup(func() { utils.CurrentVersion = currentVersion })

	if err := CheckKomariVersion(">=1.2.5"); err != nil {
		t.Fatalf("LTS version suffix was rejected: %v", err)
	}
	if err := CheckKomariVersion(">1.2.5"); err == nil {
		t.Fatal("LTS version suffix changed the core version comparison")
	}
}
