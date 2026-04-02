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

type Config struct {
	AppToken        string
	DBPath          string
	APIPort         string
	APIBaseURL      string
	WebUIEnabled    bool
	WebUIPort       string
	RybbitSiteID    string
	RybbitSiteKey   string
	RybbitSiteURL   string
	AllowUnsafeURLs bool
}

var cfg Config
var rybbitClient = &http.Client{Timeout: 3 * time.Second}

const bucketName = "urls"
const maxPageSize = 100

const envAppToken = "APP_TOKEN"
const envDBPath = "DB_PATH"
const envPort = "API_PORT"
const envAPIBaseURL = "API_URL"
const envWebUI = "WEB_UI"
const envWebUIPort = "WEB_UI_PORT"
const envAllowUnsafeURLs = "ALLOW_UNSAFE_URLS"

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
        <label>URL <input id="url" type="text" placeholder="https://example.com or /relative-path" required></label>
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

func initConfig() {
	cfg.AppToken = os.Getenv(envAppToken)
	cfg.DBPath = envOrDefault(envDBPath, defaultDBPath)
	cfg.APIPort = envOrDefault(envPort, defaultAPIPort)
	cfg.APIBaseURL = normalizeBaseURL(os.Getenv(envAPIBaseURL))
	cfg.WebUIEnabled = parseBoolEnv(os.Getenv(envWebUI))
	cfg.WebUIPort = envOrDefault(envWebUIPort, defaultWebUIPort)
	cfg.RybbitSiteID = os.Getenv(envRybbitSiteID)
	cfg.RybbitSiteKey = os.Getenv(envRybbitSiteKey)
	cfg.RybbitSiteURL = os.Getenv(envRybbitSiteURL)
	cfg.AllowUnsafeURLs = parseBoolEnv(os.Getenv(envAllowUnsafeURLs))
}

func resolveAPIBaseURL(r *http.Request) string {
	// 1. Explicitly configured URL takes highest priority
	if cfg.APIBaseURL != "" {
		return cfg.APIBaseURL
	}

	// 2. Intelligently infer from the current request
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}

	host := r.Host
	if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
		host = fwdHost
	}

	// Strip the port the request came in on (e.g., Web UI port)
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		hostname = host
	}

	// If the API runs on standard HTTP/HTTPS ports, omit the port
	if (scheme == "http" && cfg.APIPort == "80") || (scheme == "https" && cfg.APIPort == "443") {
		return fmt.Sprintf("%s://%s", scheme, hostname)
	}

	// Reconstruct the URL pointing to the API port
	return fmt.Sprintf("%s://%s:%s", scheme, hostname, cfg.APIPort)
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
		// Renamed variable to avoid shadowing the "bytes" package
		buf := make([]byte, 6)
		if _, err := rand.Read(buf); err != nil {
			log.Printf("[keys] crypto/rand failed, retrying: %v", err)
			continue
		}
		for i, b := range buf {
			buf[i] = charset[b%byte(len(charset))]
		}
		key := string(buf)

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
		if auth != cfg.AppToken {
			writeJSONError(w, http.StatusUnauthorized, "Unauthorized: Invalid or missing Authorization header token.")
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
var customPathRegex = regexp.MustCompile(`^[a-zA-Z0-9._~/-]{1,128}$`)

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
	// Initialize slice so JSON responds with `[]` instead of `null` when empty
	items := make([]KVPair, 0)
	var nextCursor string

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}

		c := b.Cursor()
		var k, v []byte

		if cursor != "" {
			k, v = c.Seek([]byte(cursor))
		} else {
			k, v = c.First()
		}

		count := 0
		for ; k != nil; k, v = c.Next() {
			if limit > 0 && count >= limit {
				break
			}
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
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: Failed to parse JSON body (%v).", err))
		return
	}

	if targetURL == "" {
		writeJSONError(w, http.StatusBadRequest, "Bad Request: The 'url' field is required in the JSON body or 'URL' header.")
		return
	}

	if !cfg.AllowUnsafeURLs && !isValidStrictURL(targetURL) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: The provided URL '%s' is invalid, uses an unsupported scheme, or contains disallowed characters.", targetURL))
		return
	}

	key := generateRandomKey()

	if err := putURL(key, targetURL); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Internal Server Error: Failed to store the URL in the database (%v).", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

func handleListURLs(w http.ResponseWriter, r *http.Request) {
	cursor := r.Header.Get("Cursor")
	limitStr := r.Header.Get("Limit")
	limit := maxPageSize
	
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			if l == 0 {
				limit = 0 // 0 means return all
			} else if l > 0 && l <= maxPageSize {
				limit = l
			}
		}
	}

	items, nextCursor, err := listURLs(cursor, limit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Internal Server Error: Failed to retrieve URLs from the database (%v).", err))
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
		http.Error(w, "Method not allowed: This endpoint only supports GET requests to redirect to short links.", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		http.Error(w, "Bad Request: A short link key is required in the URL path.", http.StatusBadRequest)
		return
	}

	url, err := getURL(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("Not Found: The short link key '%s' does not exist.", path), http.StatusNotFound)
		return
	}

	handlePageView(r)
	http.Redirect(w, r, url, http.StatusFound)
}

func handlePutCustomURL(w http.ResponseWriter, r *http.Request, path string) {
	if !isValidCustomPath(path) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: Invalid custom path '%s'. Paths must be 1-128 characters long and contain only alphanumeric characters, dots, underscores, tildes, or hyphens.", path))
		return
	}

	targetURL, err := readURLFromHeaderOrBody(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: Failed to parse JSON body (%v).", err))
		return
	}

	if targetURL == "" {
		writeJSONError(w, http.StatusBadRequest, "Bad Request: The 'url' field is required in the JSON body or 'URL' header.")
		return
	}

	if !cfg.AllowUnsafeURLs && !isValidStrictURL(targetURL) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: The provided URL '%s' is invalid, uses an unsupported scheme, or contains disallowed characters.", targetURL))
		return
	}

	if err := putURL(path, targetURL); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Internal Server Error: Failed to store the custom URL in the database (%v).", err))
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func handleDeleteURL(w http.ResponseWriter, r *http.Request, path string) {
	if err := deleteURL(path); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Internal Server Error: Failed to delete URL '%s' from the database (%v).", path, err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func handlePageView(r *http.Request) {
	if cfg.RybbitSiteID == "" || cfg.RybbitSiteKey == "" || cfg.RybbitSiteURL == "" {
		return
	}

	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			ip = h
		} else {
			ip = r.RemoteAddr
		}
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
		"site_id":    cfg.RybbitSiteID,
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

		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.RybbitSiteURL, "/")+"/api/track", bytes.NewReader(body))
		if err != nil {
			log.Printf("[rybbit] failed to create request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.RybbitSiteKey)

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
		http.Error(w, "Method not allowed: This endpoint only supports GET requests to load the Web UI.", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(webUIPage))
}

func handleWebUICreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed: This endpoint only supports POST requests to create short links.")
		return
	}

	var req WebUICreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: Invalid JSON body format (%v).", err))
		return
	}

	req.Token = strings.TrimSpace(req.Token)
	req.URL = strings.TrimSpace(req.URL)
	req.CustomPath = strings.Trim(strings.TrimSpace(req.CustomPath), "/")

	if req.Token != cfg.AppToken {
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized: The provided application token is invalid.")
		return
	}

	if !cfg.AllowUnsafeURLs && !isValidStrictURL(req.URL) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: The provided target URL '%s' is invalid, uses an unsupported scheme, or contains disallowed characters.", req.URL))
		return
	}

	key := req.CustomPath
	if key == "" {
		key = generateRandomKey()
	} else if !isValidCustomPath(key) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Bad Request: Invalid custom path '%s'. Paths must be 1-128 characters long and contain only alphanumeric characters, dots, underscores, tildes, or hyphens.", key))
		return
	} else {
		// Prevent accidental overwriting of existing keys
		if _, err := getURL(key); err == nil {
			writeJSONError(w, http.StatusConflict, fmt.Sprintf("Conflict: The custom path '%s' is already in use.", key))
			return
		}
	}

	if err := putURL(key, req.URL); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Internal Server Error: Failed to store the URL in the database (%v).", err))
		return
	}

	shortURL := fmt.Sprintf("%s/%s", resolveAPIBaseURL(r), key)

	writeJSON(w, http.StatusOK, map[string]string{
		"key":       key,
		"short_url": shortURL,
	})
}

func main() {
	initConfig()

	if cfg.AppToken == "" {
		log.Fatalf("%s environment variable must be set", envAppToken)
	}

	var err error
	db, err = bbolt.Open(cfg.DBPath, 0600, nil)
	if err != nil {
		log.Fatalf("[startup] failed opening db at %s: %v", cfg.DBPath, err)
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

	if cfg.WebUIEnabled {
		if cfg.WebUIPort == cfg.APIPort {
			log.Printf("[web-ui] enabled but %s (%s) matches %s (%s); skipping to avoid interfering with API", envWebUIPort, cfg.WebUIPort, envPort, cfg.APIPort)
		} else {
			webUIMux := http.NewServeMux()
			webUIMux.HandleFunc("/", handleWebUIPage)
			webUIMux.HandleFunc("/api/create", handleWebUICreate)

			go func() {
				log.Printf("[web-ui] listening on :%s", cfg.WebUIPort)
				if err := http.ListenAndServe(":"+cfg.WebUIPort, webUIMux); err != nil {
					log.Printf("[web-ui] server stopped: %v", err)
				}
			}()
		}
	}

	apiBase := cfg.APIBaseURL
	if apiBase == "" {
		apiBase = fmt.Sprintf("http://localhost:%s", cfg.APIPort)
	}
	log.Printf("[api] listening on :%s (public %s)", cfg.APIPort, apiBase)
	log.Fatal(http.ListenAndServe(":"+cfg.APIPort, apiMux))
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
			writeJSONError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method not allowed: Unsupported HTTP method '%s' on root endpoint. Use POST to create or GET to list.", r.Method))
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
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Sprintf("Method not allowed: Unsupported HTTP method '%s' for custom path endpoint. Use GET, PUT, or DELETE.", r.Method))
	}
}