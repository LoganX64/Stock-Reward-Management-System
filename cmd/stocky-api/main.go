package main

import (
	"context"
	"database/sql"
	"fmt"

	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/sirupsen/logrus"

	_ "github.com/lib/pq"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/LoganX64/stocky-api/internal/config"
	routes "github.com/LoganX64/stocky-api/internal/handlers/stocky"
	"github.com/LoganX64/stocky-api/internal/jobs"
)

var db *sql.DB

func main() {

	cfg := config.MustLoad()

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		cfg.Database.Host,
		cfg.Database.DbPort,
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.DBName,
		cfg.Database.SSLMode)

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		logrus.Fatalf("failed to connect to db: %v", err)
	}

	if err := db.Ping(); err != nil {
		logrus.Fatalf("failed to ping db: %v", err)
	}
	logrus.Info("connected to Database successfully")
	routes.InitDB(db)

	dbURL := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=%s",
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.Host,
		cfg.Database.DbPort,
		cfg.Database.DBName,
		cfg.Database.SSLMode)

	if err := runMigrations(dbURL); err != nil {
		logrus.Fatalf("failed to run migrations: %v", err)
	}
	logrus.Info("Migrations done successfully")

	r := gin.Default()
	routes.Routes(r)

	go jobs.StartPriceUpdater(db)

	port := cfg.HTTPServer.Port
	if port == "" {
		port = ":8080"
	}

	srv := &http.Server{
		Addr:    port,
		Handler: r,
	}

	go func() {
		logrus.Infof("starting server on %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logrus.Fatalf("server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logrus.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logrus.Fatal("Server forced to shutdown: ", err)
	}
	logrus.Info("Server exited")

}
func runMigrations(dbURL string) error {
	m, err := migrate.New("file://internal/database/migrations", dbURL)
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	if err == migrate.ErrNoChange {
		logrus.Info("No new migrations to apply")
	}
	return nil
}
