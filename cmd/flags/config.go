package flags

import (
	"os"
	"strings"
)

const (
	DatabaseTypeSQLite = "sqlite"
)

var (
	// 数据库配置
	DatabaseType string // 数据库类型：sqlite
	DatabaseFile string // SQLite数据库文件路径

	Listen string
)

func NormalizeDatabaseType(databaseType string) string {
	databaseType = strings.ToLower(strings.TrimSpace(databaseType))
	if databaseType == "" {
		return DatabaseTypeSQLite
	}
	return databaseType
}

func ApplyDatabaseTypeNormalization() string {
	DatabaseType = NormalizeDatabaseType(DatabaseType)
	return DatabaseType
}

func IsSQLite() bool {
	return NormalizeDatabaseType(DatabaseType) == DatabaseTypeSQLite
}

func SupportedDatabaseTypes() string {
	return DatabaseTypeSQLite
}

// NativePasskeyConfigured selects the native store even if RP settings are incomplete.
func NativePasskeyConfigured() bool {
	return os.Getenv("KOMARI_PASSKEY_ORIGIN") != "" || os.Getenv("KOMARI_PASSKEY_RP_ID") != ""
}
