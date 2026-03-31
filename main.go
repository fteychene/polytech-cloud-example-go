package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var (
	appVersion = getenv("APP_VERSION", "1.0.0")
	appName    = getenv("APP_NAME", "my-app")
	databaseURL = os.Getenv("POSTGRESQL_ADDON_URI")
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// -------------------------------------------------------------------
// Storage — PostgreSQL si POSTGRESQL_ADDON_URI est défini, mémoire sinon
// -------------------------------------------------------------------

type Item struct {
	ID          int        `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
}

type storage interface {
	init() error
	healthCheck() error
	findAll() ([]Item, error)
	insert(name string, description *string) (Item, error)
}

// PostgreSQL

type pgStorage struct{ db *sql.DB }

func newPgStorage(url string) (*pgStorage, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	return &pgStorage{db: db}, nil
}

func (s *pgStorage) init() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS items (
			id          SERIAL PRIMARY KEY,
			name        TEXT NOT NULL,
			description TEXT,
			created_at  TIMESTAMPTZ DEFAULT NOW()
		)`)
	return err
}

func (s *pgStorage) healthCheck() error {
	return s.db.Ping()
}

func (s *pgStorage) findAll() ([]Item, error) {
	rows, err := s.db.Query("SELECT id, name, description, created_at FROM items ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *pgStorage) insert(name string, description *string) (Item, error) {
	var item Item
	err := s.db.QueryRow(
		"INSERT INTO items (name, description) VALUES ($1, $2) RETURNING id, name, description, created_at",
		name, description,
	).Scan(&item.ID, &item.Name, &item.Description, &item.CreatedAt)
	return item, err
}

// Mémoire

type memStorage struct {
	items  []Item
	nextID int
}

func newMemStorage() *memStorage { return &memStorage{nextID: 1} }

func (s *memStorage) init() error { return nil }
func (s *memStorage) healthCheck() error { return nil }

func (s *memStorage) findAll() ([]Item, error) {
	result := make([]Item, len(s.items))
	for i, item := range s.items {
		result[len(s.items)-1-i] = item
	}
	return result, nil
}

func (s *memStorage) insert(name string, description *string) (Item, error) {
	item := Item{ID: s.nextID, Name: name, Description: description, CreatedAt: time.Now()}
	s.items = append(s.items, item)
	s.nextID++
	return item, nil
}

// -------------------------------------------------------------------
// Logging middleware
// -------------------------------------------------------------------

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, wrapped.status, time.Since(start).Round(time.Millisecond))
	})
}

// -------------------------------------------------------------------
// Handlers
// -------------------------------------------------------------------

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func crashHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Provoque un arrêt brutal du processus (démo PaaS)
		jsonResponse(w, http.StatusOK, map[string]string{"message": "Crash imminent..."})
		go func() {
			time.Sleep(100 * time.Millisecond)
			os.Exit(1)
		}()
	}
}

func healthHandler(store storage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"status":  "ok",
			"name":    appName,
			"version": appVersion,
		}
		if databaseURL != "" {
			if err := store.healthCheck(); err != nil {
				jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
					"status": "error", "name": appName, "version": appVersion, "database": "unreachable",
				})
				return
			}
			payload["database"] = "connected"
		}
		jsonResponse(w, http.StatusOK, payload)
	}
}

func getItemsHandler(store storage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.findAll()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if items == nil {
			items = []Item{}
		}
		jsonResponse(w, http.StatusOK, items)
	}
}

func createItemHandler(store storage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name        string  `json:"name"`
			Description *string `json:"description"`
		}
		json.NewDecoder(r.Body).Decode(&body)

		if strings.TrimSpace(body.Name) == "" {
			jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "Le champ 'name' est obligatoire"})
			return
		}

		item, err := store.insert(strings.TrimSpace(body.Name), body.Description)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusCreated, item)
	}
}

// -------------------------------------------------------------------
// Démarrage
// -------------------------------------------------------------------

func main() {
	godotenv.Load()

	// Re-read env after dotenv load
	appVersion = getenv("APP_VERSION", "1.0.0")
	appName = getenv("APP_NAME", "my-app")
	databaseURL = os.Getenv("POSTGRESQL_ADDON_URI")

	port := getenv("PORT", "3000")

	var store storage
	if databaseURL != "" {
		pg, err := newPgStorage(databaseURL)
		if err != nil {
			log.Fatalf("Impossible de se connecter à PostgreSQL : %v", err)
		}
		if err := pg.init(); err != nil {
			log.Fatalf("Erreur d'initialisation de la base de données : %v", err)
		}
		store = pg
		fmt.Printf("Base de données : PostgreSQL\n")
	} else {
		log.Println("POSTGRESQL_ADDON_URI non défini — stockage en mémoire (données perdues au redémarrage)")
		store = newMemStorage()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(store))
	mux.HandleFunc("GET /items", getItemsHandler(store))
	mux.HandleFunc("POST /items", createItemHandler(store))
	mux.HandleFunc("GET /crash", crashHandler())

	fmt.Printf("App démarrée sur le port %s (version %s)\n", port, appVersion)
	log.Fatal(http.ListenAndServe(":"+port, loggingMiddleware(mux)))
}