package messageSender

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

func TestCredentialTransportMetadataAndLegacyChannelIsolation(t *testing.T) {
	db := dbcore.GetDBInstance()
	if err := db.AutoMigrate(&models.MessageSenderProvider{}); err != nil {
		t.Fatal(err)
	}
	var previous models.MessageSenderProvider
	found := db.Where("name = ?", "email").First(&previous).Error == nil
	if err := db.Where("name = ?", "email").Delete(&models.MessageSenderProvider{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.MessageSenderProvider{Name: "email", Addition: `{"host":"smtp.example.test","password":"synthetic-secret","receiver":"alerts@example.test"}`}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("name = ?", "email").Delete(&models.MessageSenderProvider{})
		if found {
			db.Create(&previous)
		}
	})
	items := ListCredentialTransports()
	var emailFound bool
	for _, item := range items {
		if item.ID == "email" {
			emailFound = true
			if len(item.Kinds) != 1 || item.Kinds[0] != "email" {
				t.Fatal("incorrect private transport kind")
			}
		}
	}
	if !emailFound {
		t.Fatal("saved email transport omitted")
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "alerts@example.test") {
		t.Fatal("configuration exposed by metadata")
	}
	if err := SendCredential(context.Background(), "empty", factory.CredentialDelivery{Kind: "email", Address: "private@example.test", Title: "Synthetic", Message: "Synthetic"}); err != factory.ErrCredentialDelivery {
		t.Fatal("legacy alert sender accepted for credentials")
	}
	if err := SendCredential(context.Background(), "Javascript", factory.CredentialDelivery{Kind: "email", Address: "private@example.test", Title: "Synthetic", Message: "Synthetic"}); err != factory.ErrCredentialDelivery {
		t.Fatal("legacy JS alert fallback accepted")
	}
	var current models.MessageSenderProvider
	if err := db.Where("name = ?", "email").First(&current).Error; err != nil || strings.Contains(current.Addition, "private@example.test") {
		t.Fatal("recipient configuration mutated")
	}
}
