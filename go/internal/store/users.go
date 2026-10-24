package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// User is one account. Email is stored lower-cased; a user created by an
// administrator before email login was configured has none.
type User struct {
	ID              string
	Email           string
	DisplayName     string
	Avatar          string
	Role            string
	PasswordHash    string
	Disabled        bool
	EmailVerifiedAt *int64
	CreatedAt       int64
	UpdatedAt       int64
}

const userColumns = `id, coalesce(email, ''), display_name, coalesce(avatar, ''),
	role, password_hash, disabled, email_verified_at, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var disabled int
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Avatar, &u.Role,
		&u.PasswordHash, &disabled, &u.EmailVerifiedAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Disabled = disabled != 0
	return u, nil
}

// NewID returns a sortable identifier with a type prefix, e.g. "usr_...".
func NewID(prefix string) string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// An ID collision is worse than a panic here: fall back to the clock.
		return prefix + "_" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:24]
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

// NormaliseEmail lower-cases and trims an address.
func NormaliseEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// CountUsers reports how many accounts exist; the first one becomes admin.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts an account and returns it as stored.
func (s *Store) CreateUser(ctx context.Context, u User) (User, error) {
	now := time.Now().UnixMilli()
	if u.ID == "" {
		u.ID = NewID("usr")
	}
	if u.Role == "" {
		u.Role = "member"
	}
	u.Email = NormaliseEmail(u.Email)
	u.CreatedAt, u.UpdatedAt = now, now

	var email any
	if u.Email != "" {
		email = u.Email
	}
	var avatar any
	if u.Avatar != "" {
		avatar = u.Avatar
	}
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO users (id, password_hash, role, disabled, email, email_verified_at,
			                   display_name, avatar, created_at, updated_at)
			VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
			u.ID, u.PasswordHash, u.Role, email, u.EmailVerifiedAt, u.DisplayName, avatar,
			u.CreatedAt, u.UpdatedAt)
		return err
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// UserByID returns one account.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	row := s.Read().QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// UserByEmail returns the account with that address, case-insensitively.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	row := s.Read().QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = ?`, NormaliseEmail(email))
	return scanUser(row)
}

// UpdateUserProfile writes the editable profile fields.
func (s *Store) UpdateUserProfile(ctx context.Context, id, displayName, avatar, email string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var emailValue any
		if NormaliseEmail(email) != "" {
			emailValue = NormaliseEmail(email)
		}
		var avatarValue any
		if avatar != "" {
			avatarValue = avatar
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE users SET display_name = ?, avatar = ?, email = ?, updated_at = ?
			WHERE id = ?`,
			displayName, avatarValue, emailValue, time.Now().UnixMilli(), id)
		return err
	})
}

// SetUserPassword replaces the stored hash.
func (s *Store) SetUserPassword(ctx context.Context, id, hash string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
			hash, time.Now().UnixMilli(), id)
		return err
	})
}
