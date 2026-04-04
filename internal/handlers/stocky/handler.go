package stocky

import (
	"database/sql"
)

// Handler holds all dependencies (mainly DB)
type Handler struct {
	DB *sql.DB
}

// NewHandler creates a new Handler instance with injected dependencies
func NewHandler(db *sql.DB) *Handler {
	return &Handler{
		DB: db,
	}
}
