package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	redis "github.com/redis/go-redis/v9"
)

type healthResponse struct {
	Status string `json:"status"`
}
type locationResponse struct {
	Locations []Location `json:"locations"`
}

type Location struct {
	Id        string `json:"id"`
	City      string `json:"city"`
	Country   string `json:"country"`
	CreatedAt string `json:"created_at,omitempty"`
}

type NextLocationCursor struct {
	Id        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type redisConfig struct {
	addr     string
	password string
	db       int
}

type redisClient struct {
	config redisConfig
	client *redis.Client
}

const redisLocationsZKey = "locations:z"

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

func getEnvOrDefault(key string, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}

	return value
}

func getRedisConfig() redisConfig {
	redisDB, err := strconv.Atoi(getEnvOrDefault("REDIS_DB", "0"))
	if err != nil {
		log.Fatalf("Error parsing REDIS_DB: %v", err)
	}

	return redisConfig{
		addr:     getEnvOrDefault("REDIS_ADDR", "localhost:6379"),
		password: os.Getenv("REDIS_PASSWORD"),
		db:       redisDB,
	}
}

func newRedisClient(cfg redisConfig) (*redisClient, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.addr,
		Password: cfg.password,
		DB:       cfg.db,
	})

	defer func() { _ = rdb.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}

	return &redisClient{config: cfg, client: rdb}, nil
}

// loadLocationsIntoRedis loads all locations from the Postgres DB into Redis.
// It creates a sorted set (redisLocationsZKey) where the score is an index
// representing recency (higher == newer). The member is the location id.
// It also stores each location as a Redis hash at key "location:{id}".
func loadLocationsIntoRedis(db *sql.DB, rc *redisClient) (int, error) {
	ctx := context.Background()

	locations, err := queryLocationsFromDB(db)
	if err != nil {
		return 0, err
	}

	// Remove existing sorted set and rebuild
	if err := rc.client.Del(ctx, redisLocationsZKey).Err(); err != nil {
		return 0, err
	}

	total := len(locations)
	if total == 0 {
		return 0, nil
	}

	// Use a pipeline for performance
	pipe := rc.client.Pipeline()
	// We'll assign score = total - i so that the most recent entry (i=0)
	// gets the highest score = total. We will use ZREVRANGE/ZREVRANK for cursor pagination.
	for i, loc := range locations {
		score := float64(total - i)
		pipe.ZAdd(ctx, redisLocationsZKey, redis.Z{Score: score, Member: loc.Id})
		// store hash for location
		locKey := "location:" + loc.Id
		pipe.HSet(ctx, locKey, map[string]interface{}{
			"id":         loc.Id,
			"city":       loc.City,
			"country":    loc.Country,
			"created_at": loc.CreatedAt,
		})
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}

	log.Printf("Locations loaded into Redis cache %d", total)

	return total, nil
}

func queryLocationsFromDB(db *sql.DB) ([]Location, error) {
	rows, err := db.Query("SELECT id, city, country, created_at FROM locations ORDER BY created_at DESC, id DESC;")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var locations []Location
	for rows.Next() {
		var location Location
		if err := rows.Scan(&location.Id, &location.City, &location.Country, &location.CreatedAt); err != nil {
			return nil, err
		}
		locations = append(locations, location)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return locations, nil
}

func queryIntParam(r *http.Request, key string, defaultValue int) (int, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return defaultValue, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid %s: %q", key, value)
	}

	return parsed, nil
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

	// Initialize Redis client
	rcfg := getRedisConfig()
	redisCache, err := newRedisClient(rcfg)
	if err != nil {
		log.Fatalf("Error connecting to Redis: %v", err)
	}

	_, err = loadLocationsIntoRedis(db, redisCache)
	if err != nil {
		log.Printf("Warning: Error loading locations into Redis: %v", err)
	}

	defer func() { _ = db.Close() }()

	aMux.HandleHttpFunc(http.MethodGet, "/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{Status: "Healthy"})
	})

	// Load all locations -> bad
	aMux.HandleHttpFunc(http.MethodGet, "/api/v1/locations", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query("SELECT id, city, country FROM locations;")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		defer func() { _ = rows.Close() }()
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
		offset, err := queryIntParam(r, "offset", 0)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}

		limit, err := queryIntParam(r, "limit", 100)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}

		rows, err := db.Query("SELECT id, city, country FROM locations LIMIT $2 OFFSET $1", offset, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		defer func() { _ = rows.Close() }()
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

	// Keyset Pagination -> good, but it can be better
	aMux.HandleHttpFunc(http.MethodGet, "/api/v3/locations", func(w http.ResponseWriter, r *http.Request) {
		lastId := r.URL.Query().Get("last_id")
		lastCreatedAt := r.URL.Query().Get("last_created_at")
		limit, err := queryIntParam(r, "limit", 100)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}

		query := "SELECT id, city, country, created_at FROM locations ORDER BY created_at DESC, id DESC LIMIT $1"
		args := []any{limit}
		if lastCreatedAt != "" {
			query = "SELECT id, city, country, created_at FROM locations WHERE (id, created_at) < ($1, $2) ORDER BY created_at DESC, id DESC LIMIT $3"
			args = []any{lastId, lastCreatedAt, limit}
		}

		rows, err := db.Query(query, args...)

		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		defer func() { _ = rows.Close() }()

		var locations []Location
		for rows.Next() {
			var location Location
			if err := rows.Scan(&location.Id, &location.City, &location.Country, &lastCreatedAt); err != nil {
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
				return
			}
			locations = append(locations, location)
		}
		if len(locations) == 0 {
			writeJSON(w, http.StatusOK, struct {
				Locations    []Location         `json:"locations"`
				NextLocation NextLocationCursor `json:"next_location_cursor"`
			}{
				Locations:    locations,
				NextLocation: NextLocationCursor{},
			})
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Locations    []Location         `json:"locations"`
			NextLocation NextLocationCursor `json:"next_location_cursor"`
		}{
			Locations: locations,
			NextLocation: NextLocationCursor{
				Id:        locations[len(locations)-1].Id,
				CreatedAt: lastCreatedAt,
			},
		})
	})

	// Keyset pagination with Redis as DB
	aMux.HandleHttpFunc(http.MethodGet, "/api/v4/locations", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		lastId := r.URL.Query().Get("last_id")

		limit, err := queryIntParam(r, "limit", 100)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		if limit == 0 {
			writeJSON(w, http.StatusOK, struct {
				Locations    []Location         `json:"locations"`
				NextLocation NextLocationCursor `json:"next_location_cursor"`
			}{Locations: []Location{}, NextLocation: NextLocationCursor{}})
			return
		}

		var start int64 = 0
		if lastId != "" {
			rankCmd := redisCache.client.ZRevRank(ctx, redisLocationsZKey, lastId)
			if err := rankCmd.Err(); err != nil {
				if err == redis.Nil {
					writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("last_id %s not found", lastId)})
					return
				}
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
				return
			}
			rank := rankCmd.Val()
			// start from the element after the one identified by lastId
			start = rank + 1
		}

		end := start + int64(limit) - 1
		membersCmd := redisCache.client.ZRevRange(ctx, redisLocationsZKey, start, end)
		if err := membersCmd.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		ids := membersCmd.Val()

		var locations []Location
		for _, id := range ids {
			hcmd := redisCache.client.HGetAll(ctx, "location:"+id)
			if err := hcmd.Err(); err != nil {
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
				return
			}
			data := hcmd.Val()
			loc := Location{
				Id:        data["id"],
				City:      data["city"],
				Country:   data["country"],
				CreatedAt: data["created_at"],
			}
			locations = append(locations, loc)
		}

		if len(locations) == 0 {
			writeJSON(w, http.StatusOK, struct {
				Locations    []Location         `json:"locations"`
				NextLocation NextLocationCursor `json:"next_location_cursor"`
			}{Locations: locations, NextLocation: NextLocationCursor{}})
			return
		}

		// Next cursor is the last returned location
		next := NextLocationCursor{
			Id:        locations[len(locations)-1].Id,
			CreatedAt: locations[len(locations)-1].CreatedAt,
		}

		writeJSON(w, http.StatusOK, struct {
			Locations    []Location         `json:"locations"`
			NextLocation NextLocationCursor `json:"next_location_cursor"`
		}{Locations: locations, NextLocation: next})
	})

	log.Println("Starting server on :8080")
	err = http.ListenAndServe(":8080", mux)
	if err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
