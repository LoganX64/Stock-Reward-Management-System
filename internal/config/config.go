package config

import (
	"log"
	"os"
)

type HTTPServer struct {
	Port string
}

type Database struct {
	Host     string
	DbPort   string
	User     string
	Password string
	DBName   string
	SSLMode  string
}

type Config struct {
	Env        string
	Database   Database
	HTTPServer HTTPServer
}

func LoadFromEnv() *Config {
	getEnv := func(key, defaultVal string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return defaultVal
	}

	cfg := &Config{
		Env: getEnv("ENV", "dev"),
		Database: Database{
			Host:     getEnv("DB_HOST", "localhost"),
			DbPort:   getEnv("DB_PORT", "5432"),
			User:     getEnv("POSTGRES_USER", "postgres"),
			Password: getEnv("POSTGRES_PASSWORD", "password"),
			DBName:   getEnv("POSTGRES_DB", "assignment"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		HTTPServer: HTTPServer{
			Port: ":" + getEnv("HTTP_PORT", "8080"),
		},
	}

	return cfg
}

func MustLoad() *Config {
	cfg := LoadFromEnv()
	if cfg == nil {
		log.Fatal("failed to load config from environment variables")
	}
	return cfg
}
