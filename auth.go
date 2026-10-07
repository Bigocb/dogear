package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---- users ----

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"` // "admin" | "user"
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) ensureUsersSchema() error {
	return s.migrateUsers()
}

// migrateUsers creates the users/shelves tables and back-fills the first
// admin's shelf with every existing book (preserving current statuses).
func (s *Store) migrateUsers() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS dogear_config (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id             INTEGER PRIMARY KEY,
  username       TEXT UNIQUE NOT NULL,
  password_hash  TEXT NOT NULL,
  role           TEXT NOT NULL DEFAULT 'user',
  created_at     INTEGER NOT NULL
);

-- Per-user shelf: which books a user sees + their private state for them.
CREATE TABLE IF NOT EXISTS user_books (
  user_id   INTEGER NOT NULL,
  book_id   INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  status    TEXT NOT NULL DEFAULT 'imported',
  hidden    INTEGER NOT NULL DEFAULT 0,
  added_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, book_id)
);

-- Reading time tracking: one row per user/book/day with accumulated seconds.
CREATE TABLE IF NOT EXISTS reading_sessions (
  user_id       INTEGER NOT NULL,
  book_id       INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  day           TEXT NOT NULL,           -- YYYY-MM-DD (user-local approx UTC)
  seconds       INTEGER NOT NULL DEFAULT 0,
  pages         INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, book_id, day)
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_day ON reading_sessions(user_id, day);

-- Imported reading-list items, keyed by the source's remote id, so a re-sync
-- is idempotent (no duplicates, only new/changed entries are added).
CREATE TABLE IF NOT EXISTS import_items (
  user_id     INTEGER NOT NULL,
  source      TEXT NOT NULL,           -- 'hardcover' | 'goodreads'
  remote_id   TEXT NOT NULL,           -- provider book id / CSV row key
  book_id     INTEGER,                 -- linked library book, if created
  status      TEXT NOT NULL,           -- mapped: wanted | reading | read
  imported_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, source, remote_id)
);
CREATE INDEX IF NOT EXISTS idx_import_user_source ON import_items(user_id, source);
`)
	if err != nil {
		return err
	}

	// --- add user_id columns where missing ---
	// NOTE: tables originally had (book_id)-style PRIMARY KEYs; multi-user needs
	// composite (user_id, book_id) keys, so instead of ADD COLUMN we rebuild
	// each table (SQLite cannot change a PK in place).
	rebuild := func(table, cols, extraIdx string) error {
		// does user_id exist already?
		var has int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name='user_id'`).Scan(&has)
		rows, err := s.db.Query(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table)
		if err != nil {
			return err
		}
		var oldSQL string
		if rows.Next() {
			_ = rows.Scan(&oldSQL)
		}
		rows.Close()
		if oldSQL == "" {
			return nil // table doesn't exist (fresh db: base migration runs first)
		}
		if has > 0 && strings.Contains(oldSQL, "PRIMARY KEY (user_id, book_id)") {
			return nil // already rebuilt
		}
		if err := func() error {
			// gather existing data
			var dataRows *sql.Rows
			selCols := "book_id, cfi, percent, device, updated_at"
			if has > 0 {
				selCols = "user_id, book_id, cfi, percent, device, updated_at"
			}
			dataRows, err = s.db.Query(`SELECT ` + selCols + ` FROM ` + table)
			if err != nil {
				return err
			}
			type row struct {
				uid, bid int64
				cfi      string
				pct      float64
				dev      string
				upd      int64
			}
			var data []row
			for dataRows.Next() {
				var r row
				if has > 0 {
					if err := dataRows.Scan(&r.uid, &r.bid, &r.cfi, &r.pct, &r.dev, &r.upd); err != nil {
						dataRows.Close()
						return err
					}
				} else {
					r.uid = 1
					if err := dataRows.Scan(&r.bid, &r.cfi, &r.pct, &r.dev, &r.upd); err != nil {
						dataRows.Close()
						return err
					}
				}
				data = append(data, r)
			}
			dataRows.Close()

			// rebuild
			if _, err := s.db.Exec(`DROP TABLE ` + table); err != nil {
				return err
			}
			if _, err := s.db.Exec(`CREATE TABLE ` + table + ` (
  user_id     INTEGER NOT NULL,
  book_id     INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  cfi         TEXT,
  percent     REAL,
  device      TEXT,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, book_id)
)`); err != nil {
				return err
			}
			for _, r := range data {
				if _, err := s.db.Exec(`INSERT INTO `+table+`(user_id, book_id, cfi, percent, device, updated_at) VALUES(?,?,?,?,?,?)`,
					r.uid, r.bid, r.cfi, r.pct, r.dev, r.upd); err != nil {
					return err
				}
			}
			if extraIdx != "" {
				if _, err := s.db.Exec(extraIdx); err != nil {
					return err
				}
			}
			return nil
		}(); err != nil {
			return fmt.Errorf("rebuild %s: %w", table, err)
		}
		return nil
	}

	if err := rebuild("progress", "", ""); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_progress_user ON progress(user_id, book_id)`); err != nil {
		return err
	}
	// highlights/bookmarks/grabs keep rowid tables; add user_id where missing
	for _, table := range []string{"highlights", "bookmarks", "grabs"} {
		var colcount int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name='user_id'`).Scan(&colcount)
		if colcount == 0 {
			if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN user_id INTEGER NOT NULL DEFAULT 1;`); err != nil {
				return err
			}
		}
	}

	// --- bootstrap admin user ---
	var usercount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&usercount); err != nil {
		return err
	}
	if usercount == 0 {
		pw := os.Getenv("DOGEAR_ADMIN_PASSWORD")
		if pw == "" {
			pw = "dogear" // homelab default; change it in the admin screen
		}
		hash, err := hashPassword(pw)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(`INSERT INTO users(username, password_hash, role, created_at) VALUES('dcloutier', ?, 'admin', strftime('%s','now'))`, hash); err != nil {
			return err
		}
	}

	// --- back-fill shelf: give the first admin every existing book ---
	var shelfcount int
	adminID := int64(0)
	if err := s.db.QueryRow(`SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&adminID); err != nil {
		return nil // no users yet, skip
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_books WHERE user_id=?`, adminID).Scan(&shelfcount); err != nil {
		return err
	}
	if shelfcount == 0 {
		_, err := s.db.Exec(`
INSERT INTO user_books(user_id, book_id, status, hidden, added_at)
SELECT ?, id, status, 0, added_at FROM books WHERE true
ON CONFLICT(user_id, book_id) DO NOTHING`, adminID)
		return err
	}
	return nil
}

func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// ---- sessions ----

// dogearSessionSecret is generated once per install and stored in DB config.
func (s *Store) sessionSecret() (string, error) {
	var secret string
	err := s.db.QueryRow(`SELECT value FROM dogear_config WHERE key='session_secret'`).Scan(&secret)
	if err == nil && secret != "" {
		return secret, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret = base64.RawURLEncoding.EncodeToString(buf)
	_, err = s.db.Exec(`INSERT INTO dogear_config(key, value) VALUES('session_secret', ?)`, secret)
	return secret, err
}

func signSession(secret, username string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifySession(secret, username, sig string) bool {
	want := signSession(secret, username)
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}

// userFromRequest resolves the signed cookie to a User (nil = anonymous).
func (a *apiServer) userFromRequest(r *http.Request) *User {
	c, err := r.Cookie("dogear_session")
	if err != nil || c.Value == "" {
		return nil
	}
	secret, err := a.store.sessionSecret()
	if err != nil {
		return nil
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 || !verifySession(secret, parts[0], parts[1]) {
		return nil
	}
	u, err := a.store.getUser(parts[0])
	if err != nil || u == nil || u.Role == "" {
		return nil
	}
	return &User{ID: u.ID, Username: u.Username, Role: u.Role, CreatedAt: u.CreatedAt}
}

// requireUser wraps handlers; responds 401 when anonymous.
func (a *apiServer) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := a.userFromRequest(r)
		if u == nil {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		ctx := context.WithValue(r.Context(), userKey{}, u)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin wraps handlers; responds 403 for non-admins.
func (a *apiServer) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return a.requireUser(func(w http.ResponseWriter, r *http.Request) {
		u := userFromCtx(r)
		if u == nil || u.Role != "admin" {
			writeErr(w, http.StatusForbidden, "admin required")
			return
		}
		next(w, r)
	})
}

type userKey struct{}

func userFromCtx(r *http.Request) *User {
	u, _ := r.Context().Value(userKey{}).(*User)
	return u
}

// userIDFromCtx returns the caller's id (0 if anonymous).
func userIDFromCtx(r *http.Request) int64 {
	if u := userFromCtx(r); u != nil {
		return u.ID
	}
	return 0
}

// ---- handlers ----

func (a *apiServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" {
		writeErr(w, http.StatusBadRequest, "username/password required")
		return
	}
	u, err := a.store.getUser(body.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	if u == nil || !checkPassword(orDefaultStr(u.PasswordHash), body.Password) {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	secret, _ := a.store.sessionSecret()
	http.SetCookie(w, &http.Cookie{
		Name:     "dogear_session",
		Value:    u.Username + "." + signSession(secret, u.Username),
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 30,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true, // always https in production (behind tunnel)
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]any{
		"username": u.Username, "role": u.Role,
	}})
}

func (a *apiServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "dogear_session", Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiServer) handleMe(w http.ResponseWriter, r *http.Request) {
	u := a.userFromRequest(r)
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "not logged in")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": u.Username, "role": u.Role})
}

// listUsers (admin)
func (a *apiServer) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := a.store.listUsers()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// never expose password hashes
		out := make([]User, len(users))
		copy(out, users)
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
			writeErr(w, http.StatusBadRequest, "username and password required")
			return
		}
		if body.Role == "" {
			body.Role = "user"
		}
		if body.Role != "user" && body.Role != "admin" {
			writeErr(w, http.StatusBadRequest, "invalid role")
			return
		}
		id, err := a.store.createUser(body.Username, body.Password, body.Role)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("create user: %v", err))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "username": body.Username, "role": body.Role})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *apiServer) handleUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		// guard: cannot delete the last admin or yourself
		caller := userFromCtx(r)
		if caller != nil && caller.ID == id {
			writeErr(w, http.StatusBadRequest, "cannot delete yourself")
			return
		}
		if err := a.store.deleteUser(id); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		var body struct {
			Password *string `json:"password"`
			Role     *string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := a.store.updateUser(id, body.Password, body.Role); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ---- store: user lookup & management ----

type userRow struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	CreatedAt    int64
}

func (s *Store) getUser(username string) (*userRow, error) {
	row := s.db.QueryRow(`SELECT id, username, password_hash, role, created_at FROM users WHERE username=?`, username)
	var u userRow
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) getUserByID(id int64) (*userRow, error) {
	row := s.db.QueryRow(`SELECT id, username, password_hash, role, created_at FROM users WHERE id=?`, id)
	var u userRow
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// listUsers requires admin at handler level.
func (s *Store) listUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, role, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) createUser(username, password, role string) (int64, error) {
	hash, err := hashPassword(password)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO users(username, password_hash, role, created_at) VALUES(?,?,?,strftime('%s','now'))`,
		strings.ToLower(strings.TrimSpace(username)), hash, role)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("user already exists")
		}
		return 0, err
	}
	id, _ := res.LastInsertId()

	// new user's shelf starts with nothing (Netflix model: own profiles)
	return id, nil
}

func (s *Store) updateUser(id int64, password, role *string) error {
	if password != nil && *password != "" {
		hash, err := hashPassword(*password)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, id); err != nil {
			return err
		}
	}
	if role != nil && *role != "" {
		if *role != "user" && *role != "admin" {
			return fmt.Errorf("invalid role")
		}
		// guard: don't demote the last admin
		if *role == "user" {
			var admins int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
				return err
			}
			if admins <= 1 {
				return fmt.Errorf("cannot demote the last admin")
			}
		}
		if _, err := s.db.Exec(`UPDATE users SET role=? WHERE id=?`, *role, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deleteUser(id int64) error {
	// last-admin guard
	var u *userRow
	var err error
	if u, err = s.getUserByID(id); err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("user not found")
	}
	if u.Role == "admin" {
		var admins int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return fmt.Errorf("cannot delete the last admin")
		}
	}
	if _, err := s.db.Exec(`DELETE FROM user_books WHERE user_id=?`, id); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

func orDefaultStr(s string) string { return s }

// ---- shelf queries (Netflix-profile model) ----

// shelfBooks lists a user's shelf with per-user status. hidden books excluded.
func (s *Store) shelfBooks(userID int64, query string) ([]Book, error) {
	sqlQ := `SELECT ` + bookColsP("b.") + `
		FROM user_books ub JOIN books b ON b.id=ub.book_id
		WHERE ub.user_id=? AND ub.hidden=0`
	args := []any{userID}
	if query != "" {
		sqlQ += ` AND (lower(b.title) LIKE ? OR lower(coalesce(b.author,'')) LIKE ?)`
		q := "%" + strings.ToLower(query) + "%"
		args = append(args, q, q)
	}
	sqlQ += ` ORDER BY ub.added_at DESC`
	rows, err := s.db.Query(sqlQ, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		var shelfStatus string
		_ = s.db.QueryRow(`SELECT status FROM user_books WHERE user_id=? AND book_id=?`, userID, b.ID).Scan(&shelfStatus)
		if shelfStatus != "" {
			b.Status = shelfStatus
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// shelfStatus returns (status, hidden) of a book on a user's shelf; ok=false when absent.
func (s *Store) shelfStatus(userID, bookID int64) (status string, hidden bool, ok bool) {
	row := s.db.QueryRow(`SELECT status, hidden FROM user_books WHERE user_id=? AND book_id=?`, userID, bookID)
	err := row.Scan(&status, &hidden)
	if err == sql.ErrNoRows {
		return "", false, false
	}
	return status, hidden, err == nil
}

// shelfAdd ensures the book is on the user's shelf (idempotent, keeps existing status).
func (s *Store) shelfAdd(userID, bookID int64, status string) error {
	if status == "" {
		status = "imported"
	}
	// Follow the authoritative status being written, so a wanted row becomes
	// imported once a grab lands -- but never DOWNGRADE a book the reader has
	// progressed: reading/read wins over an incoming wanted/grabbed/imported.
	_, err := s.db.Exec(`INSERT INTO user_books(user_id, book_id, status, hidden, added_at) VALUES(?,?,?,0,strftime('%s','now'))
		ON CONFLICT(user_id, book_id) DO UPDATE SET hidden=0, status=CASE
			WHEN user_books.status IN ('reading','read') AND excluded.status IN ('wanted','grabbed','imported')
			  THEN user_books.status
			ELSE excluded.status END`,
		userID, bookID, status)
	return err
}

// shelfRemove takes a book off the shelf (library-wide files are untouched).
func (s *Store) shelfRemove(userID, bookID int64) error {
	_, err := s.db.Exec(`DELETE FROM user_books WHERE user_id=? AND book_id=?`, userID, bookID)
	return err
}

// shelfSetStatus updates the per-user status of a shelved book.
func (s *Store) shelfSetStatus(userID, bookID int64, status string) error {
	res, err := s.db.Exec(`UPDATE user_books SET status=? WHERE user_id=? AND book_id=?`, status, userID, bookID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("book %d not on shelf %d", bookID, userID)
	}
	return nil
}

// unshelvedBooks lists library books NOT on a user's shelf (the "add to my shelf" browse).
func (s *Store) unshelvedBooks(userID int64, query string) ([]Book, error) {
	// Household browse: shared books plus the user's OWN private books, but
	// never anyone else's private books.
	sqlQ := `SELECT ` + bookColsP("b.") + `
		FROM books b
		WHERE b.id NOT IN (SELECT book_id FROM user_books WHERE user_id=?)
		  AND (b.private = 0 OR b.owner_id = ?)`
	args := []any{userID, userID}
	if query != "" {
		sqlQ += ` AND (lower(b.title) LIKE ? OR lower(coalesce(b.author,'')) LIKE ?)`
		q := "%" + strings.ToLower(query) + "%"
		args = append(args, q, q)
	}
	sqlQ += ` ORDER BY b.added_at DESC LIMIT 200`
	rows, err := s.db.Query(sqlQ, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		b.Status = "library" // marker for the household browse UI
		out = append(out, b)
	}
	return out, rows.Err()
}

// canAccessBook reports whether userID may see/open a book: it's on their
// shelf, or it's shared (not private), or they own it.
func (s *Store) canAccessBook(userID, bookID int64) bool {
	if _, _, ok := s.shelfStatus(userID, bookID); ok {
		return true
	}
	var priv int
	var owner *int64
	err := s.db.QueryRow(`SELECT private, owner_id FROM books WHERE id=?`, bookID).Scan(&priv, &owner)
	if err != nil {
		return false
	}
	if priv == 0 {
		return true
	}
	return owner != nil && *owner == userID
}

// setPrivate toggles a book's private flag. Only the owner (or an admin) may
// change it.
func (s *Store) setPrivate(bookID int64, private bool) error {
	priv := 0
	if private {
		priv = 1
	}
	_, err := s.db.Exec(`UPDATE books SET private=?, updated_at=strftime('%s','now') WHERE id=?`, priv, bookID)
	return err
}

// ---- per-user progress / highlights / bookmarks ----

// ProgressFor returns cfi+percent for (user, book); "" + 0 when none.
func (s *Store) getProgressForUser(userID, bookID int64) (string, float64, error) {
	row := s.db.QueryRow(`SELECT cfi, percent FROM progress WHERE user_id=? AND book_id=?`, userID, bookID)
	var cfi string
	var pct float64
	err := row.Scan(&cfi, &pct)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return cfi, pct, err
}

func (s *Store) setProgressForUser(userID, bookID int64, cfi string, pct float64, device string) error {
	_, err := s.db.Exec(`INSERT INTO progress(user_id, book_id, cfi, percent, device, updated_at) VALUES(?,?,?,?,?,strftime('%s','now'))
		ON CONFLICT(user_id, book_id) DO UPDATE SET cfi=excluded.cfi, percent=excluded.percent, device=excluded.device, updated_at=excluded.updated_at`,
		userID, bookID, cfi, pct, device)
	return err
}

func (s *Store) listHighlightsForUser(userID, bookID int64) ([]Highlight, error) {
	rows, err := s.db.Query(`SELECT id, book_id, cfi, text, note, color, created_at, updated_at FROM highlights WHERE user_id=? AND book_id=? ORDER BY created_at DESC`, userID, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Highlight
	for rows.Next() {
		var hl Highlight
		if err := rows.Scan(&hl.ID, &hl.BookID, &hl.CFI, &hl.Text, &hl.Note, &hl.Color, &hl.CreatedAt, &hl.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, hl)
	}
	return out, rows.Err()
}

func (s *Store) addHighlightForUser(userID, bookID int64, cfi, text, color string, note *string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO highlights(user_id, book_id, cfi, text, note, color, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, bookID, cfi, text, note, color, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) listBookmarksForUser(userID, bookID int64) ([]Bookmark, error) {
	rows, err := s.db.Query(`SELECT id, book_id, cfi, label, percent, created_at FROM bookmarks WHERE user_id=? AND book_id=? ORDER BY percent`, userID, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bookmark
	for rows.Next() {
		var bm Bookmark
		if err := rows.Scan(&bm.ID, &bm.BookID, &bm.CFI, &bm.Label, &bm.Percent, &bm.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, bm)
	}
	return out, rows.Err()
}

func (s *Store) addBookmarkForUser(userID, bookID int64, cfi string, label *string, pct float64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO bookmarks(user_id, book_id, cfi, label, percent, created_at) VALUES(?,?,?,?,?,strftime('%s','now'))`,
		userID, bookID, cfi, label, pct)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// addReadingTime accumulates reading time for (user, book) on a given day.
// day is computed server-side; pages counts page turns if provided.
func (s *Store) addReadingTime(userID, bookID int64, seconds, pages int) error {
	if seconds <= 0 && pages <= 0 {
		return nil
	}
	if seconds > 600 {
		seconds = 600 // sanity cap per heartbeat batch
	}
	_, err := s.db.Exec(`INSERT INTO reading_sessions(user_id, book_id, day, seconds, pages) VALUES(?,?,?,?,?)
		ON CONFLICT(user_id, book_id, day) DO UPDATE SET
		  seconds = seconds + excluded.seconds,
		  pages = pages + excluded.pages`,
		userID, bookID, time.Now().UTC().Format("2006-01-02"), seconds, pages)
	return err
}

// userStats computes the dashboard aggregates for a user.
type UserStats struct {
	TotalSeconds    int64              `json:"total_seconds_read"`
	TodaySeconds    int64              `json:"today_seconds_read"`
	DaysStreak      int                `json:"streak_days"`
	BooksReading    int                `json:"books_reading"`
	BooksFinished   int                `json:"books_finished"`
	DailyLast14Days []DailyReading     `json:"daily"`
	TopBooks        []BookReadingTotal `json:"top_books"`
}

type DailyReading struct {
	Day     string `json:"day"`
	Minutes int64  `json:"minutes"`
}

type BookReadingTotal struct {
	BookID  int64  `json:"book_id"`
	Title   string `json:"title"`
	Minutes int64  `json:"minutes"`
}

func (s *Store) userStats(userID int64) (*UserStats, error) {
	st := &UserStats{}
	var totalSec, todaySec int64
	today := time.Now().UTC().Format("2006-01-02")
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(seconds),0) FROM reading_sessions WHERE user_id=?`, userID).Scan(&totalSec); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(seconds),0) FROM reading_sessions WHERE user_id=? AND day=?`, userID, today).Scan(&todaySec); err != nil {
		return nil, err
	}
	st.TotalSeconds = totalSec
	st.TodaySeconds = todaySec

	// streak: count consecutive days with > 0 seconds ending today (or yesterday)
	var activeDays []string
	rows, err := s.db.Query(`SELECT DISTINCT day FROM reading_sessions WHERE user_id=? AND seconds>0 ORDER BY day DESC`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil {
			activeDays = append(activeDays, d)
		}
	}
	rows.Close()
	streak := 0
	dayCursor := time.Now().UTC()
	for _, d := range activeDays {
		want := dayCursor.Format("2006-01-02")
		if d == want {
			streak++
		} else {
			break
		}
		dayCursor = dayCursor.AddDate(0, 0, -1)
	}
	// if today has none, streak can still be alive from yesterday
	if len(activeDays) > 0 && activeDays[0] != today && streak == 0 {
		yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
		if activeDays[0] == yesterday {
			dayCursor = time.Now().UTC().AddDate(0, 0, -1)
			for _, d := range activeDays {
				want := dayCursor.Format("2006-01-02")
				if d == want {
					streak++
				} else {
					break
				}
				dayCursor = dayCursor.AddDate(0, 0, -1)
			}
		}
	}
	st.DaysStreak = streak

	// counts from shelf
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_books WHERE user_id=? AND status='reading' AND hidden=0`, userID).Scan(&st.BooksReading); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_books WHERE user_id=? AND status='read' AND hidden=0`, userID).Scan(&st.BooksFinished); err != nil {
		return nil, err
	}

	// daily last 14 days (fill zeros)
	rows, err = s.db.Query(`SELECT day, COALESCE(SUM(seconds),0) FROM reading_sessions WHERE user_id=? AND day >= date('now','-13 days') GROUP BY day`, userID)
	if err != nil {
		return nil, err
	}
	dayMap := map[string]int64{}
	for rows.Next() {
		var d string
		var sec int64
		if err := rows.Scan(&d, &sec); err == nil {
			dayMap[d] = sec
		}
	}
	rows.Close()
	for i := 13; i >= 0; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		st.DailyLast14Days = append(st.DailyLast14Days, DailyReading{Day: day, Minutes: dayMap[day] / 60})
	}

	// top books by minutes
	rows, err = s.db.Query(`SELECT book_id, SUM(seconds) sec FROM reading_sessions WHERE user_id=? GROUP BY book_id ORDER BY sec DESC LIMIT 5`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bid, sec int64
		if err := rows.Scan(&bid, &sec); err != nil {
			continue
		}
		title := ""
		_ = s.db.QueryRow(`SELECT title FROM books WHERE id=?`, bid).Scan(&title)
		st.TopBooks = append(st.TopBooks, BookReadingTotal{BookID: bid, Title: title, Minutes: sec / 60})
	}
	return st, nil
}

var _ = json.Marshal
