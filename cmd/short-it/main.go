package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

var db *bbolt.DB
var authToken string

var rybbitSiteID = os.Getenv("RYBBIT_SITE_ID")
var rybbitSiteKey = os.Getenv("RYBBIT_SITE_KEY")
var rybbitSiteURL = os.Getenv("RYBBIT_SITE_URL")
var rybbitClient = &http.Client{Timeout: 3 * time.Second}

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

var domainRegex = regexp.MustCompile(`^[a-zA-Z0-9\.-]+$`)

func isValidStrictURL(s string) bool {
	// Avoid large strings
	if len(s) > 2048 {
		return false
	}
	// Disallow raw characters, these should already be encoded
	if strings.ContainsAny(s, "<>\"'`") {
		return false
	}

	u, err := url.ParseRequestURI(s)
	if err != nil || u.Host == "" {
		return false
	}

	// Only allow http and https schemes
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}

	// Reject URLs with user info (username:password@)
	if u.User != nil {
		return false
	}

	hostname := u.Hostname()
	if hostname == "localhost" {
		return true
	}

	if ip := net.ParseIP(hostname); ip != nil {
		return true
	}

	// Make sure hostname is a valid domain name
	if !domainRegex.MatchString(hostname) {
		return false
	}
	if !strings.Contains(hostname, ".") {
		return false
	}
	parts := strings.Split(hostname, ".")
	tld := parts[len(parts)-1]
	if len(tld) < 2 {
		return false
	}

	return true
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

	if !isValidStrictURL(url) {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
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

	handlePageView(r)
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

	if !isValidStrictURL(url) {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
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

func handlePageView(r *http.Request) {
	if rybbitSiteID == "" || rybbitSiteKey == "" || rybbitSiteURL == "" {
		return
	}

	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip = strings.Split(r.RemoteAddr, ":")[0]
	} else {
		ip = strings.TrimSpace(strings.Split(ip, ",")[0])
	}

	hostname := r.Host
	if hostname == "" {
		if h := r.URL.Hostname(); h != "" {
			hostname = h
		}
	}

	data := map[string]string{
		"site_id":    rybbitSiteID,
		"type":       "pageview",
		"pathname":   r.URL.Path,
		"hostname":   hostname,
		"referrer":   r.Referer(),
		"language":   r.Header.Get("Accept-Language"),
		"user_agent": r.UserAgent(),
		"ip_address": ip,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}

	go func(body []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(rybbitSiteURL, "/")+"/api/track", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+rybbitSiteKey)

		resp, err := rybbitClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			log.Printf("Rybbit tracking error: %s", string(b))
		}
	}(jsonData)
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
