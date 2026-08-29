package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GetAdmin returns an admin by username.
func (s *Store) GetAdmin(ctx context.Context, username string) (Admin, error) {
	var a Admin
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, password_hash, created_at FROM admins WHERE username=$1`, username).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	return a, err
}

// CountAdmins reports how many admin rows exist (for first-boot seeding).
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM admins`).Scan(&n)
	return n, err
}

// CreateAdmin inserts an admin with an already-hashed password.
func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO admins (username, password_hash) VALUES ($1,$2)
		 ON CONFLICT (username) DO NOTHING`, username, passwordHash)
	return err
}

// SetAdminPassword updates an admin's password hash.
func (s *Store) SetAdminPassword(ctx context.Context, username, passwordHash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE admins SET password_hash=$2 WHERE username=$1`, username, passwordHash)
	return err
}
