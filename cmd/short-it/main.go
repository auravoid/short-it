package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.etcd.io/bbolt"
)

var db *bbolt.DB
var authToken string

const bucketName = "urls"
const maxPageSize = 100

type KVPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type PaginatedResponse struct {
	Items []KVPair `json:"items"`
	Next  string   `json:"next,omitempty"`
}

func generateRandomKey() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for {
		bytes := make([]byte, 6)
		rand.Read(bytes)
		for i, b := range bytes {
			bytes[i] = charset[b%byte(len(charset))]
		}
		key := string(bytes)

		// Check if key already exists
		if _, err := getURL(key); err != nil {
			// Key doesn't exist, it's safe to use
			return key
		}
	}
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != authToken {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func getURL(key string) (string, error) {
	var url string
	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}
		v := b.Get([]byte(key))
		if v == nil {
			return fmt.Errorf("key not found")
		}
		url = string(v)
		return nil
	})
	return url, err
}

func putURL(key, url string) error {
	return db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}
		return b.Put([]byte(key), []byte(url))
	})
}

func deleteURL(key string) error {
	return db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}
		return b.Delete([]byte(key))
	})
}

func listURLs(cursor string, limit int) ([]KVPair, string, error) {
	var items []KVPair
	var nextCursor string

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}

		c := b.Cursor()
		var k, v []byte

		if cursor != "" {
			c.Seek([]byte(cursor))
			k, v = c.Next()
		} else {
			k, v = c.First()
		}

		count := 0
		for ; k != nil && count < limit; k, v = c.Next() {
			items = append(items, KVPair{Key: string(k), Value: string(v)})
			count++
		}

		if k != nil {
			nextCursor = string(k)
		} else {
			nextCursor = ""
		}

		return nil
	})

	return items, nextCursor, err
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		handleCreateShortURL(w, r)
	case http.MethodGet:
		handleListURLs(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleCreateShortURL(w http.ResponseWriter, r *http.Request) {
	url := r.Header.Get("URL")
	if url == "" {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid JSON or missing URL", http.StatusBadRequest)
			return
		}
		url = body["url"]
	}

	if url == "" {
		http.Error(w, "URL required", http.StatusBadRequest)
		return
	}

	key := generateRandomKey()

	if err := putURL(key, url); err != nil {
		http.Error(w, "Failed to store URL", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"key": key})
}

func handleListURLs(w http.ResponseWriter, r *http.Request) {
	cursor := r.Header.Get("Cursor")
	limitStr := r.Header.Get("Limit")
	limit := maxPageSize
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= maxPageSize {
			limit = l
		}
	}

	items, nextCursor, err := listURLs(cursor, limit)
	if err != nil {
		http.Error(w, "Failed to list URLs", http.StatusInternalServerError)
		return
	}

	response := PaginatedResponse{
		Items: items,
	}
	if nextCursor != "" {
		response.Next = nextCursor
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleGetURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		http.Error(w, "Key required", http.StatusBadRequest)
		return
	}

	url, err := getURL(path)
	if err != nil {
		http.Error(w, "URL not found", http.StatusNotFound)
		return
	}

	http.Redirect(w, r, url, http.StatusFound)
}

func handleCustomURL(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		http.Error(w, "Path required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		handlePutCustomURL(w, r, path)
	case http.MethodDelete:
		handleDeleteURL(w, r, path)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func handlePutCustomURL(w http.ResponseWriter, r *http.Request, path string) {
	url := r.Header.Get("URL")
	if url == "" {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid JSON or missing URL", http.StatusBadRequest)
			return
		}
		url = body["url"]
	}

	if url == "" {
		http.Error(w, "URL required", http.StatusBadRequest)
		return
	}

	if err := putURL(path, url); err != nil {
		http.Error(w, "Failed to store URL", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func handleDeleteURL(w http.ResponseWriter, r *http.Request, path string) {
	if err := deleteURL(path); err != nil {
		http.Error(w, "Failed to delete URL", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func main() {
	authToken = os.Getenv("APP_TOKEN")
	if authToken == "" {
		log.Fatal("APP_TOKEN environment variable must be set")
	}

	var err error
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "short-it.db"
	}
	db, err = bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		return err
	})
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", handleRequest)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("Server starting on port %s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")

	if path == "" {
		switch r.Method {
		case http.MethodPost:
			authMiddleware(handleCreateShortURL)(w, r)
		case http.MethodGet:
			authMiddleware(handleListURLs)(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		handleGetURL(w, r)
	case http.MethodPut:
		authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			handlePutCustomURL(w, r, path)
		})(w, r)
	case http.MethodDelete:
		authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			handleDeleteURL(w, r, path)
		})(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
