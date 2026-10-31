package isureal

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Region         string
	DBHost         string
	DBPort         string
	DBUser         string
	DBPassword     string
	DBName         string
	ImageFileDir   string
	ImagePeers     []string
	ImageTokenFile string
	DBMaxOpenConns int
	LogLevel       string
}

func ConfigFromEnv() *Config {
	maxConns, err := strconv.Atoi(envOrDefault("DB_MAXOPENCONNS", "25"))
	if err != nil || maxConns <= 0 {
		panic(fmt.Sprintf("DB_MAXOPENCONNS must be a positive integer: %q", os.Getenv("DB_MAXOPENCONNS")))
	}
	return &Config{
		ImageFileDir:   envOrDefault("ISUREAL_IMAGE_FILE_DIR", "/home/isucon/image-files"),
		ImagePeers:     strings.FieldsFunc(os.Getenv("ISUREAL_IMAGE_PEERS"), func(r rune) bool { return r == ',' }),
		ImageTokenFile: envOrDefault("ISUREAL_IMAGE_TOKEN_FILE", "/home/isucon/.image-file-token"),
		DBMaxOpenConns: maxConns,
		Region:         envOrDefault("ISUREAL_REGION", "asia"),
		DBHost:         envOrDefault("ISUREAL_DB_HOST", "127.0.0.1"),
		DBPort:         envOrDefault("ISUREAL_DB_PORT", "5432"),
		DBUser:         envOrDefault("ISUREAL_DB_USER", "isureal"),
		DBPassword:     envOrDefault("ISUREAL_DB_PASSWORD", "isureal"),
		DBName:         envOrDefault("ISUREAL_DB_NAME", "isureal"),
		LogLevel:       envOrDefault("ISUREAL_LOG_LEVEL", "error"),
	}
}

func (c *Config) DatabaseDSN() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.DBUser, c.DBPassword),
		Host:   net.JoinHostPort(c.DBHost, c.DBPort),
		Path:   "/" + c.DBName,
	}
	query := u.Query()
	query.Set("default_query_exec_mode", "exec")
	u.RawQuery = query.Encode()
	return u.String()
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
