package config

import (
	"flag"
	"log"
	"os"

	"github.com/ilyakaznacheev/cleanenv"
)

type HTTPServer struct {
	Port string `yaml:"port" env:"PORT" env-default:":8080"`
}

type Database struct {
	Host     string `yaml:"host" env:"HOST" env-default:"localhost"`
	DbPort   string `yaml:"port" env:"DB_PORT" env-default:"5432"`
	User     string `yaml:"user" env:"USER" env-default:"postgres"`
	Password string `yaml:"password" env:"PASSWORD" env-default:"password"`
	DBName   string `yaml:"dbname" env:"DBNAME" env-default:"assignment"`
	SSLMode  string `yaml:"sslmode" env:"SSLMODE" env-default:"disable"`
}

type Config struct {
	Env        string `yaml:"env"`
	Database   `yaml:"db"`
	HTTPServer `yaml:"http_server"`
}

func MustLoad() *Config {
	var configPath string

	configPath = os.Getenv("CONFIG_PATH")

	if configPath == "" {
		configFlag := flag.String("config", "", "Path to config file")
		flag.Parse()

		if *configFlag != "" {
			configPath = *configFlag
		}
	}

	if configPath == "" {
		log.Fatal("config path not provided")
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Fatalf("config file not found: %s", configPath)
	}

	var cfg Config

	err := cleanenv.ReadConfig(configPath, &cfg)
	if err != nil {
		log.Fatalf("failed to read config: %v", err)
	}

	return &cfg
}
