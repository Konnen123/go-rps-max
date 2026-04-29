package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

type healthResponse struct {
	Status string `json:"status"`
}
type locationResponse struct {
	Locations []Location `json:"locations"`
}

type Location struct {
	Id      string `json:"id"`
	City    string `json:"city"`
	Country string `json:"country"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func loadEnv() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
}

func getConnectionString() string {
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")

	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s", dbHost, dbPort, dbUser, dbPassword, dbName)
}

func openDbConnection(connectionString string) *sql.DB {
	db, err := sql.Open("postgres", connectionString)
	if err != nil {
		log.Fatalf("Error opening database connection: %v", err)
	}
	err = db.Ping()
	if err != nil {
		log.Fatalf("Error pinging database connection: %v", err)
	}

	log.Println("Successfully connected to database")
	return db
}

func main() {
	mux := http.NewServeMux()
	aMux := appMux{mux: mux}

	loadEnv()
	connectionString := getConnectionString()
	db := openDbConnection(connectionString)
	defer db.Close()

	aMux.HandleHttpFunc(http.MethodGet, "/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{Status: "Healthy"})
	})

	// Load all locations -> bad
	aMux.HandleHttpFunc(http.MethodGet, "/api/v1/locations", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query("SELECT id, city, country FROM locations;")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		}
		defer rows.Close()
		var locations []Location
		for rows.Next() {
			var location Location
			if err := rows.Scan(&location.Id, &location.City, &location.Country); err != nil {
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
				return
			}
			locations = append(locations, location)
		}
		writeJSON(w, http.StatusOK, locationResponse{Locations: locations})
	})

	// Offset and Pagination -> better, but has its issues
	aMux.HandleHttpFunc(http.MethodGet, "/api/v2/locations", func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		limit := r.URL.Query().Get("limit")

		rows, err := db.Query("SELECT id, city, country FROM locations OFFSET $1 LIMIT $2", offset, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		defer rows.Close()
		var locations []Location
		for rows.Next() {
			var location Location
			if err := rows.Scan(&location.Id, &location.City, &location.Country); err != nil {
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
				return
			}
			locations = append(locations, location)
		}
		writeJSON(w, http.StatusOK, locationResponse{Locations: locations})
	})

	log.Println("Starting server on :8080")
	err := http.ListenAndServe(":8080", mux)
	if err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
