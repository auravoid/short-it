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

var rybbitSiteID = os.Getenv(envRybbitSiteID)
var rybbitSiteKey = os.Getenv(envRybbitSiteKey)
var rybbitSiteURL = os.Getenv(envRybbitSiteURL)
var rybbitClient = &http.Client{Timeout: 3 * time.Second}

const bucketName = "urls"
const maxPageSize = 100

const envAppToken = "APP_TOKEN"
const envDBPath = "DB_PATH"
const envPort = "API_PORT"
const envAPIBaseURL = "API_URL"
const envWebUI = "WEB_UI"
const envWebUIPath = "WEB_UI_URL"
const envWebUIPort = "WEB_UI_PORT"

const envRybbitSiteID = "RYBBIT_SITE_ID"
const envRybbitSiteKey = "RYBBIT_SITE_KEY"
const envRybbitSiteURL = "RYBBIT_SITE_URL"

const defaultAPIPort = "8080"
const defaultWebUIPort = "8090"
const defaultDBPath = "short-it.db"

const webUIPage = `<!doctype html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>short-it UI</title>
	<style>
		body { font-family: sans-serif; max-width: 680px; margin: 2rem auto; padding: 0 1rem; }
		label { display: block; margin-top: 1rem; font-weight: 600; }
		input { width: 100%; padding: .6rem; margin-top: .35rem; box-sizing: border-box; }
		button { margin-top: 1rem; padding: .65rem 1rem; cursor: pointer; }
		pre { margin-top: 1rem; padding: .7rem; background: #f5f5f5; overflow-x: auto; }
	</style>
</head>
<body>
	<h1>Create short link</h1>
	<form id="create-form">
		<label>Token <input id="token" type="password" required></label>
		<label>URL <input id="url" type="url" placeholder="https://example.com" required></label>
		<label>Custom path (optional) <input id="custom_path" type="text" placeholder="my-link"></label>
		<button type="submit">Create</button>
	</form>
	<pre id="result"></pre>
	<script>
		const form = document.getElementById('create-form');
		const result = document.getElementById('result');
		form.addEventListener('submit', async (event) => {
			event.preventDefault();
			const payload = {
				token: document.getElementById('token').value,
				url: document.getElementById('url').value,
				custom_path: document.getElementById('custom_path').value
			};
			const response = await fetch('/api/create', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload)
			});
			const data = await response.json().catch(() => ({ error: 'unexpected response' }));
			result.textContent = JSON.stringify(data, null, 2);
		});
	</script>
</body>
</html>`

type KVPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type PaginatedResponse struct {
	Items []KVPair `json:"items"`
	Next  string   `json:"next,omitempty"`
}

type WebUICreateRequest struct {
	Token      string `json:"token"`
	URL        string `json:"url"`
	CustomPath string `json:"custom_path"`
}

func parseBoolEnv(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "true")
}

func envOrDefault(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func resolveAPIBaseURL(r *http.Request) string {
	if base := normalizeBaseURL(os.Getenv(envAPIBaseURL)); base != "" {
		return base
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	if host == "" {
		host = "localhost"
	}

	return fmt.Sprintf("http://%s:%s", host, envOrDefault(envPort, defaultAPIPort))
}

func resolveWebUIBaseURL(webUIPort string) string {
	if base := normalizeBaseURL(os.Getenv(envWebUIPath)); base != "" {
		return base
	}

	return fmt.Sprintf("http://localhost:%s", webUIPort)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[http] failed to encode response: %v", err)
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func readURLFromHeaderOrBody(r *http.Request) (string, error) {
	targetURL := strings.TrimSpace(r.Header.Get("URL"))
	if targetURL != "" {
		return targetURL, nil
	}

	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", err
	}

	return strings.TrimSpace(body["url"]), nil
}

func generateRandomKey() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for {
		bytes := make([]byte, 6)
		if _, err := rand.Read(bytes); err != nil {
			log.Printf("[keys] crypto/rand failed, retrying: %v", err)
			continue
		}
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
var customPathRegex = regexp.MustCompile(`^[a-zA-Z0-9._~-]{1,128}$`)

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

func handleCreateShortURL(w http.ResponseWriter, r *http.Request) {
	targetURL, err := readURLFromHeaderOrBody(r)
	if err != nil {
		http.Error(w, "Invalid JSON or missing URL", http.StatusBadRequest)
		return
	}

	if targetURL == "" {
		http.Error(w, "URL required", http.StatusBadRequest)
		return
	}

	if !isValidStrictURL(targetURL) {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	key := generateRandomKey()

	if err := putURL(key, targetURL); err != nil {
		http.Error(w, "Failed to store URL", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": key})
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

	writeJSON(w, http.StatusOK, response)
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

func handlePutCustomURL(w http.ResponseWriter, r *http.Request, path string) {
	targetURL, err := readURLFromHeaderOrBody(r)
	if err != nil {
		http.Error(w, "Invalid JSON or missing URL", http.StatusBadRequest)
		return
	}

	if targetURL == "" {
		http.Error(w, "URL required", http.StatusBadRequest)
		return
	}

	if !isValidStrictURL(targetURL) {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	if err := putURL(path, targetURL); err != nil {
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
		log.Printf("[rybbit] failed to marshal tracking payload: %v", err)
		return
	}

	go func(body []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(rybbitSiteURL, "/")+"/api/track", bytes.NewReader(body))
		if err != nil {
			log.Printf("[rybbit] failed to create request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+rybbitSiteKey)

		resp, err := rybbitClient.Do(req)
		if err != nil {
			log.Printf("[rybbit] request failed: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			log.Printf("[rybbit] tracking error status=%d body=%s", resp.StatusCode, string(b))
		}
	}(jsonData)
}

func isValidCustomPath(path string) bool {
	return customPathRegex.MatchString(path)
}

func handleWebUIPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(webUIPage))
}

func handleWebUICreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req WebUICreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	req.Token = strings.TrimSpace(req.Token)
	req.URL = strings.TrimSpace(req.URL)
	req.CustomPath = strings.Trim(strings.TrimSpace(req.CustomPath), "/")

	if req.Token != authToken {
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if !isValidStrictURL(req.URL) {
		writeJSONError(w, http.StatusBadRequest, "Invalid URL")
		return
	}

	key := req.CustomPath
	if key == "" {
		key = generateRandomKey()
	} else if !isValidCustomPath(key) {
		writeJSONError(w, http.StatusBadRequest, "Invalid custom path")
		return
	}

	if err := putURL(key, req.URL); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to store URL")
		return
	}

	shortURL := fmt.Sprintf("%s/%s", resolveAPIBaseURL(r), key)

	writeJSON(w, http.StatusOK, map[string]string{
		"key":       key,
		"short_url": shortURL,
	})
}

func main() {
	authToken = os.Getenv(envAppToken)
	if authToken == "" {
		log.Fatalf("%s environment variable must be set", envAppToken)
	}

	var err error
	dbPath := envOrDefault(envDBPath, defaultDBPath)
	db, err = bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		log.Fatalf("[startup] failed opening db at %s: %v", dbPath, err)
	}
	defer db.Close()

	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		return err
	})
	if err != nil {
		log.Fatalf("[startup] failed creating bucket %q: %v", bucketName, err)
	}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/", handleRequest)

	port := envOrDefault(envPort, defaultAPIPort)

	if parseBoolEnv(os.Getenv(envWebUI)) {
		webUIPort := envOrDefault(envWebUIPort, defaultWebUIPort)

		if webUIPort == port {
			log.Printf("[web-ui] enabled but %s (%s) matches %s (%s); skipping to avoid interfering with API", envWebUIPort, webUIPort, envPort, port)
		} else {
			webUIMux := http.NewServeMux()
			webUIMux.HandleFunc("/", handleWebUIPage)
			webUIMux.HandleFunc("/api/create", handleWebUICreate)

			go func() {
				log.Printf("[web-ui] listening on :%s (public %s)", webUIPort, resolveWebUIBaseURL(webUIPort))
				if err := http.ListenAndServe(":"+webUIPort, webUIMux); err != nil {
					log.Printf("[web-ui] server stopped: %v", err)
				}
			}()
		}
	}

	apiBase := normalizeBaseURL(os.Getenv(envAPIBaseURL))
	if apiBase == "" {
		apiBase = fmt.Sprintf("http://localhost:%s", port)
	}
	log.Printf("[api] listening on :%s (public %s)", port, apiBase)
	log.Fatal(http.ListenAndServe(":"+port, apiMux))
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
