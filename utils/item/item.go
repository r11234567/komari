package item

import (
	"reflect"
	"strings"
)

type Item struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Type     string `json:"type"`
	Options  string `json:"options"`
	Default  string `json:"default"`
	Help     string `json:"help"`
	Secret   bool   `json:"secret,omitempty"`
}

func Parse(v any) []Item {
	val := reflect.ValueOf(v)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}

	var allowTypes = []string{"option", "richtext", "password", "secret"}

	var items []Item
	for i := 0; i < val.NumField(); i++ {
		field := val.Type().Field(i)
		typ := field.Tag.Get("type")
		if !contains(allowTypes, typ) {
			typ = field.Type.Name()
		}
		jsonName := field.Tag.Get("json")
		isSecret := field.Tag.Get("secret") == "true" || typ == "password" || typ == "secret"
		if !isSecret {
			lower := strings.ToLower(jsonName)
			if lower == "password" || lower == "bot_token" || lower == "device_key" || lower == "secret" || lower == "client_secret" {
				isSecret = true
			}
		}

		if itemType := typ; itemType == "" || itemType == "string" {
			if isSecret {
				typ = "password"
			} else {
				typ = "string"
			}
		}

		item := Item{
			Name:     jsonName,
			Required: field.Tag.Get("required") == "true",
			Type:     typ,
			Options:  field.Tag.Get("options"),
			Default:  field.Tag.Get("default"),
			Help:     field.Tag.Get("help"),
			Secret:   isSecret,
		}
		items = append(items, item)
	}
	return items
}

// contains reports whether s is present in slice.
func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
