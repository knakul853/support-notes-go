package main

import (
	"context"
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
	ID    int    `json:"id"`
	Owner string `json:"owner"`
	Body  string `json:"body"`
	// Pinned is set only by staff moderation, never by the client.
	Pinned bool `json:"pinned"`
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
	mux.HandleFunc("GET /notes", s.handleListNotes)
	mux.HandleFunc("GET /notes/{id}", s.handleGetNote)
	mux.HandleFunc("POST /notes", s.handleCreateNote)

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
		CREATE TABLE IF NOT EXISTS notes (
			id serial primary key,
			owner text not null,
			body text not null,
			pinned boolean not null default false
		)
	`)
	if err != nil {
		return err
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM notes").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO notes (owner, body) VALUES
			('alice', 'Welcome to support notes.'),
			('bob', 'Second seed note for testing.')
	`)
	return err
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), "SELECT id, owner, body, pinned FROM notes ORDER BY id")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list notes"})
		return
	}
	defer rows.Close()

	notes := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Owner, &n.Body, &n.Pinned); err != nil {
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
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var n Note
	err = s.db.QueryRow(r.Context(), "SELECT id, owner, body, pinned FROM notes WHERE id = $1", id).Scan(&n.ID, &n.Owner, &n.Body, &n.Pinned)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "note not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get note"})
		return
	}

	writeJSON(w, http.StatusOK, n)
}

type createNoteRequest struct {
	Owner  string `json:"owner"`
	Body   string `json:"body"`
	Pinned bool   `json:"pinned"`
}

func (s *server) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	var req createNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if req.Owner == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "owner and body are required"})
		return
	}

	var n Note
	err := s.db.QueryRow(
		r.Context(),
		"INSERT INTO notes (owner, body, pinned) VALUES ($1, $2, $3) RETURNING id, owner, body, pinned",
		req.Owner, req.Body, req.Pinned,
	).Scan(&n.ID, &n.Owner, &n.Body, &n.Pinned)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create note"})
		return
	}

	writeJSON(w, http.StatusCreated, n)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("failed to write json response: %v", err)
	}
}
