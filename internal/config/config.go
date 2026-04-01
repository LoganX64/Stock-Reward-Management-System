package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
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
	Env           string
	Database      Database
	HTTPServer    HTTPServer
	MigrationPath string
}

func LoadFromEnv() *Config {
	// Load .env file if it exists
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env.local"
	}
	if err := godotenv.Load(envFile); err != nil {
		log.Printf("No %s file found, using system environment variables", envFile)
	}

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
		}, MigrationPath: getEnv("MIGRATION_PATH", "./internal/database/migrations")}

	return cfg
}

func MustLoad() *Config {
	cfg := LoadFromEnv()
	if cfg == nil {
		log.Fatal("failed to load config from environment variables")
	}
	return cfg
}
