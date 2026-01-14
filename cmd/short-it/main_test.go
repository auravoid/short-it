package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

func setupTestDB(t *testing.T) *bbolt.DB {
	db, err := bbolt.Open("test_"+t.Name()+".db", 0600, nil)
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		return err
	})
	if err != nil {
		t.Fatalf("Failed to create bucket: %v", err)
	}

	return db
}

func teardownTestDB(db *bbolt.DB, t *testing.T) {
	db.Close()
	os.Remove("test_" + t.Name() + ".db")
}

func TestGenerateRandomKey(t *testing.T) {
	testDB := setupTestDB(t)
	defer teardownTestDB(testDB, t)

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	key1 := generateRandomKey()
	if len(key1) != 6 {
		t.Errorf("Expected key length 6, got %d", len(key1))
	}

	validChars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for _, char := range key1 {
		if !strings.ContainsRune(validChars, char) {
			t.Errorf("Key contains invalid character: %c", char)
		}
	}

	// Test collision prevention
	err := putURL(key1, "test-url")
	if err != nil {
		t.Fatalf("Failed to put test URL: %v", err)
	}

	key2 := generateRandomKey()
	if key1 == key2 {
		t.Error("Generated keys should not collide")
	}
}

func TestDatabaseOperations(t *testing.T) {
	testDB := setupTestDB(t)
	defer teardownTestDB(testDB, t)

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	testKey := "testkey"
	testURL := "https://example.com"

	// Test put
	err := putURL(testKey, testURL)
	if err != nil {
		t.Fatalf("Failed to put URL: %v", err)
	}

	// Test get
	retrievedURL, err := getURL(testKey)
	if err != nil {
		t.Fatalf("Failed to get URL: %v", err)
	}
	if retrievedURL != testURL {
		t.Errorf("Expected %s, got %s", testURL, retrievedURL)
	}

	// Test delete
	err = deleteURL(testKey)
	if err != nil {
		t.Fatalf("Failed to delete URL: %v", err)
	}

	_, err = getURL(testKey)
	if err == nil {
		t.Error("URL should not exist after deletion")
	}
}

func TestAuthMiddleware(t *testing.T) {
	originalToken := authToken
	authToken = "test-token"
	defer func() { authToken = originalToken }()

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	// Test valid token
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "test-token")
	w := httptest.NewRecorder()
	authMiddleware(testHandler).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	// Test invalid token
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Authorization", "wrong-token")
	w2 := httptest.NewRecorder()
	authMiddleware(testHandler).ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w2.Code)
	}
}

func TestCreateShortURL(t *testing.T) {
	testDB := setupTestDB(t)
	defer teardownTestDB(testDB, t)

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	// Test with URL in header
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("URL", "https://example.com")
	w := httptest.NewRecorder()
	handleCreateShortURL(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}

	var response map[string]string
	err := json.NewDecoder(w.Body).Decode(&response)
	if err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if _, exists := response["key"]; !exists {
		t.Error("Response should contain 'key' field")
	}

	// Test without URL
	req2 := httptest.NewRequest("POST", "/", nil)
	w2 := httptest.NewRecorder()
	handleCreateShortURL(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", w2.Code)
	}
}

func TestGetURL(t *testing.T) {
	testDB := setupTestDB(t)
	defer teardownTestDB(testDB, t)

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	testKey := "testkey"
	testURL := "https://example.com"
	err := putURL(testKey, testURL)
	if err != nil {
		t.Fatalf("Failed to put test URL: %v", err)
	}

	// Test valid key
	req := httptest.NewRequest("GET", "/"+testKey, nil)
	w := httptest.NewRecorder()
	handleGetURL(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("Expected 302, got %d", w.Code)
	}
	location := w.Header().Get("Location")
	if location != testURL {
		t.Errorf("Expected redirect to %s, got %s", testURL, location)
	}

	// Test invalid key
	req2 := httptest.NewRequest("GET", "/nonexistent", nil)
	w2 := httptest.NewRecorder()
	handleGetURL(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("Expected 404, got %d", w2.Code)
	}
}

func TestPutCustomURL(t *testing.T) {
	testDB := setupTestDB(t)
	defer teardownTestDB(testDB, t)

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	// Test valid PUT
	req := httptest.NewRequest("PUT", "/custom", nil)
	req.Header.Set("URL", "https://custom.com")
	w := httptest.NewRecorder()
	handlePutCustomURL(w, req, "custom")
	if w.Code != http.StatusCreated {
		t.Errorf("Expected 201, got %d", w.Code)
	}

	// Verify URL was stored
	retrievedURL, err := getURL("custom")
	if err != nil {
		t.Fatalf("Failed to get stored URL: %v", err)
	}
	if retrievedURL != "https://custom.com" {
		t.Errorf("Expected 'https://custom.com', got '%s'", retrievedURL)
	}

	// Test PUT without URL
	req2 := httptest.NewRequest("PUT", "/custom2", nil)
	w2 := httptest.NewRecorder()
	handlePutCustomURL(w2, req2, "custom2")
	if w2.Code != http.StatusBadRequest {
		t.Errorf("Expected 400, got %d", w2.Code)
	}
}

func TestDeleteURL(t *testing.T) {
	testDB := setupTestDB(t)
	defer teardownTestDB(testDB, t)

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	testKey := "deleteme"
	err := putURL(testKey, "https://delete.com")
	if err != nil {
		t.Fatalf("Failed to put test URL: %v", err)
	}

	// Test DELETE
	req := httptest.NewRequest("DELETE", "/"+testKey, nil)
	w := httptest.NewRecorder()
	handleDeleteURL(w, req, testKey)
	if w.Code != http.StatusNoContent {
		t.Errorf("Expected 204, got %d", w.Code)
	}

	// Verify deletion
	_, err = getURL(testKey)
	if err == nil {
		t.Error("URL should not exist after deletion")
	}
}

func TestListURLs(t *testing.T) {
	testDB := setupTestDB(t)
	defer teardownTestDB(testDB, t)

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	// Add test data
	testData := map[string]string{
		"key1": "url1",
		"key2": "url2",
		"key3": "url3",
	}
	for key, url := range testData {
		err := putURL(key, url)
		if err != nil {
			t.Fatalf("Failed to put test data: %v", err)
		}
	}

	// Test listing all
	items, nextCursor, err := listURLs("", 100)
	if err != nil {
		t.Fatalf("Failed to list URLs: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("Expected 3 items, got %d", len(items))
	}
	if nextCursor != "" {
		t.Error("Next cursor should be empty when all items fit")
	}

	// Test pagination
	items2, nextCursor2, err := listURLs("", 2)
	if err != nil {
		t.Fatalf("Failed to list URLs with pagination: %v", err)
	}
	if len(items2) != 2 {
		t.Errorf("Expected 2 items, got %d", len(items2))
	}
	// With 3 total items and limit 2, we should have a next cursor
	t.Logf("Next cursor: '%s'", nextCursor2)
	if nextCursor2 == "" {
		t.Error("Next cursor should not be empty with pagination when more items exist")
	}
}

func TestIsValidStrictURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		// Valid cases
		{"http://google.com", true},
		{"https://sub.domain.co.uk", true},
		{"http://localhost", true},
		{"http://127.0.0.1", true},
		{"http://[::1]", true},

		// Invalid / Malicious cases
		{"javascript:alert(1)", false},
		{"http://foo.com/?q=<script>", false},
		{"http://user:pass@evil.com", false},
		{"http://internal", false},
		{"http://google.c", false},
		{"ftp://google.com", false},
		{"http://exa mple.com", false},
		{"http://ex$ample.com", false},
	}

	for _, tt := range tests {
		if got := isValidStrictURL(tt.url); got != tt.want {
			t.Errorf("isValidStrictURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func BenchmarkGenerateRandomKey(b *testing.B) {
	testDB := setupTestDB(&testing.T{})
	defer teardownTestDB(testDB, &testing.T{})

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		generateRandomKey()
	}
}

func BenchmarkGetURL(b *testing.B) {
	testDB := setupTestDB(&testing.T{})
	defer teardownTestDB(testDB, &testing.T{})

	originalDB := db
	db = testDB
	defer func() { db = originalDB }()

	// Setup test data
	testKey := "benchmark"
	testURL := "https://example.com"
	putURL(testKey, testURL)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		getURL(testKey)
	}
}
