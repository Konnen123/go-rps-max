package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	redis "github.com/redis/go-redis/v9"
)

const (
	locationsStreamKey = "locations:stream"
	consumerGroup      = "locations:group"
	consumerName       = "consumer-1"
)

// StreamLocationMessage represents a location message from the Redis Stream
type StreamLocationMessage struct {
	City      string
	Country   string
	Iso       string
	Latitude  string
	Longitude string
	Address   string
}

// startLocationConsumer starts a background consumer that reads from the Redis stream,
// writes new locations to the database, and updates the Redis cache.
func startLocationConsumer(db *sql.DB, redisCache *redisClient) {
	go func() {
		if err := createConsumerGroup(redisCache); err != nil {
			log.Printf("Warning: Could not create consumer group: %v. Continuing with regular reads...", err)
		}

		for {
			if err := consumeLocationMessages(db, redisCache); err != nil {
				log.Printf("Error in location consumer: %v", err)
				// Wait before retrying to avoid tight error loops
				time.Sleep(5 * time.Second)
			}
		}
	}()
}

// createConsumerGroup creates a consumer group for the locations stream.
// If the group already exists, it's a no-op.
func createConsumerGroup(redisCache *redisClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := redisCache.client.XGroupCreateMkStream(ctx, locationsStreamKey, consumerGroup, "$").Err()
	if err != nil {
		// Group already exists, this is fine
		if err.Error() == "BUSYGROUP Consumer Group name already exists" {
			return nil
		}
		return err
	}

	log.Printf("Created consumer group: %s", consumerGroup)
	return nil
}

// consumeLocationMessages reads from the locations stream, processes messages,
// and acknowledges them after successful processing.
func consumeLocationMessages(db *sql.DB, redisCache *redisClient) error {
	ctx := context.Background()

	// Read from the consumer group with a block timeout
	streams, err := redisCache.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    consumerGroup,
		Consumer: consumerName,
		Streams:  []string{locationsStreamKey, ">"},
		Count:    10,
		Block:    0, // Block indefinitely
	}).Result()

	if err != nil {
		if err == redis.Nil {
			return nil // No messages available
		}
		return err
	}

	for _, stream := range streams {
		for _, message := range stream.Messages {
			if err := processLocationMessage(db, redisCache, message.Values); err != nil {
				log.Printf("Error processing message %s: %v", message.ID, err)
				// Don't acknowledge on error, so it can be retried
				continue
			}

			// Acknowledge the message after successful processing
			if err := redisCache.client.XAck(ctx, locationsStreamKey, consumerGroup, message.ID).Err(); err != nil {
				log.Printf("Error acknowledging message %s: %v", message.ID, err)
			}
		}
	}

	return nil
}

// processLocationMessage processes a single location message from the stream.
// It writes the location to the database and updates the Redis cache.
func processLocationMessage(db *sql.DB, redisCache *redisClient, data map[string]interface{}) error {
	ctx := context.Background()

	// Extract and validate data from the message
	msg, err := parseStreamMessage(data)
	if err != nil {
		return err
	}

	// Insert into database
	query := "INSERT INTO locations (city, country, iso, latitude, longitude, address) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at"
	var id, createdAt string
	err = db.QueryRow(query, msg.City, msg.Country, msg.Iso, msg.Latitude, msg.Longitude, msg.Address).Scan(&id, &createdAt)
	if err != nil {
		return err
	}

	log.Printf("Inserted location into database: id=%s, city=%s, country=%s", id, msg.City, msg.Country)

	// Update Redis cache
	if err := updateLocationCache(ctx, redisCache, id, msg, createdAt); err != nil {
		log.Printf("Warning: Failed to update cache for location %s: %v", id, err)
		// Don't fail the entire operation if cache update fails
	}

	return nil
}

// parseStreamMessage extracts the location data from the stream message.
func parseStreamMessage(data map[string]interface{}) (*StreamLocationMessage, error) {
	msg := &StreamLocationMessage{
		City:      toString(data["city"]),
		Country:   toString(data["country"]),
		Iso:       toString(data["iso"]),
		Latitude:  toString(data["latitude"]),
		Longitude: toString(data["longitude"]),
		Address:   toString(data["address"]),
	}

	// Basic validation
	if msg.City == "" || msg.Country == "" {
		return nil, ErrInvalidLocationData
	}

	return msg, nil
}

// updateLocationCache adds the new location to the Redis cache.
func updateLocationCache(ctx context.Context, redisCache *redisClient, id string, msg *StreamLocationMessage, createdAt string) error {
	// Use current time as score so new locations appear at the top (highest score)
	newScore := float64(time.Now().UnixNano())

	// Use pipeline for atomic operations
	pipe := redisCache.client.Pipeline()
	pipe.ZAdd(ctx, redisLocationsZKey, redis.Z{Score: newScore, Member: id})
	locKey := "location:" + id
	pipe.HSet(ctx, locKey, map[string]interface{}{
		"id":         id,
		"city":       msg.City,
		"country":    msg.Country,
		"created_at": createdAt,
	})

	_, err := pipe.Exec(ctx)
	return err
}

// toString safely converts interface{} to string
func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
