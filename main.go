package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Note struct {
	ID      int    `json:"id"`
	OwnerID int    `json:"owner_id"`
	Body    string `json:"body"`
}

type server struct {
	db *pgxpool.Pool
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ctx := context.Background()

	pool, err := connectWithRetry(ctx, dsn, 10, 2*time.Second)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	if err := initSchema(ctx, pool); err != nil {
		log.Fatalf("failed to initialize schema: %v", err)
	}

	s := &server{db: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /notes", s.handleListNotes)
	mux.HandleFunc("GET /notes/{id}", s.handleGetNote)
	mux.HandleFunc("POST /notes", s.handleCreateNote)
	mux.HandleFunc("GET /admin/notes", s.handleAdminListNotes)

	log.Printf("listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func connectWithRetry(ctx context.Context, dsn string, attempts int, delay time.Duration) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 1; i <= attempts; i++ {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		log.Printf("postgres connection attempt %d/%d failed: %v", i, attempts, err)
		time.Sleep(delay)
	}
	return nil, lastErr
}

func initSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			id serial primary key,
			username text not null unique,
			email text not null unique,
			password_hash text not null,
			role text not null default 'member'
		)
	`)
	if err != nil {
		return err
	}

	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sessions (
			token text primary key,
			user_id int not null references users(id),
			created_at timestamptz not null default now()
		)
	`)
	if err != nil {
		return err
	}

	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS notes (
			id serial primary key,
			owner_id int not null references users(id),
			body text not null
		)
	`)
	if err != nil {
		return err
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	var adminID int
	err = pool.QueryRow(ctx,
		"INSERT INTO users (username, email, password_hash, role) VALUES ($1, $2, $3, 'admin') RETURNING id",
		"admin", "admin@support-notes.test", hashPassword("ChangeMe123!"),
	).Scan(&adminID)
	if err != nil {
		return err
	}

	_, err = pool.Exec(ctx,
		"INSERT INTO notes (owner_id, body) VALUES ($1, $2)",
		adminID, "Welcome to support notes.",
	)
	return err
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "support-notes-go"})
}

type registerRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if req.Username == "" || req.Email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username, email and password are required"})
		return
	}

	var userID int
	err := s.db.QueryRow(
		r.Context(),
		"INSERT INTO users (username, email, password_hash, role) VALUES ($1, $2, $3, 'member') RETURNING id",
		req.Username, req.Email, hashPassword(req.Password),
	).Scan(&userID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "username or email already taken"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"id": userID, "username": req.Username})
}

type loginRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if req.Password == "" || (req.Username == "" && req.Email == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username or email, and password are required"})
		return
	}

	query := "SELECT id, password_hash FROM users WHERE username = $1"
	arg := req.Username
	if req.Username == "" {
		query = "SELECT id, password_hash FROM users WHERE email = $1"
		arg = req.Email
	}

	var userID int
	var passwordHash string
	err := s.db.QueryRow(r.Context(), query, arg).Scan(&userID, &passwordHash)
	if err != nil || passwordHash != hashPassword(req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	token, err := newSessionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to start session"})
		return
	}
	if _, err := s.db.Exec(r.Context(), "INSERT INTO sessions (token, user_id) VALUES ($1, $2)", token, userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to start session"})
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type authedUser struct {
	ID   int
	Role string
}

var errNoSession = errors.New("no valid session")

func (s *server) authenticate(r *http.Request) (*authedUser, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, errNoSession
	}

	var u authedUser
	err = s.db.QueryRow(
		r.Context(),
		"SELECT users.id, users.role FROM sessions JOIN users ON users.id = sessions.user_id WHERE sessions.token = $1",
		cookie.Value,
	).Scan(&u.ID, &u.Role)
	if err != nil {
		return nil, errNoSession
	}
	return &u, nil
}

func (s *server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	rows, err := s.db.Query(r.Context(), "SELECT id, owner_id, body FROM notes WHERE owner_id = $1 ORDER BY id", user.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list notes"})
		return
	}
	defer rows.Close()

	notes := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.OwnerID, &n.Body); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan note"})
			return
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read notes"})
		return
	}

	writeJSON(w, http.StatusOK, notes)
}

func (s *server) handleGetNote(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var n Note
	err = s.db.QueryRow(r.Context(), "SELECT id, owner_id, body FROM notes WHERE id = $1", id).Scan(&n.ID, &n.OwnerID, &n.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "note not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get note"})
		return
	}
	if n.OwnerID != user.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "note not found"})
		return
	}

	writeJSON(w, http.StatusOK, n)
}

type createNoteRequest struct {
	Body string `json:"body"`
}

func (s *server) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req createNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	var n Note
	err = s.db.QueryRow(
		r.Context(),
		"INSERT INTO notes (owner_id, body) VALUES ($1, $2) RETURNING id, owner_id, body",
		user.ID, req.Body,
	).Scan(&n.ID, &n.OwnerID, &n.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create note"})
		return
	}

	writeJSON(w, http.StatusCreated, n)
}

func (s *server) handleAdminListNotes(w http.ResponseWriter, r *http.Request) {
	user, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if user.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}

	rows, err := s.db.Query(r.Context(), "SELECT id, owner_id, body FROM notes ORDER BY id")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list notes"})
		return
	}
	defer rows.Close()

	notes := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.OwnerID, &n.Body); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan note"})
			return
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read notes"})
		return
	}

	writeJSON(w, http.StatusOK, notes)
}

func hashPassword(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("failed to write json response: %v", err)
	}
}
