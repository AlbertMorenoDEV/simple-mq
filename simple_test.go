package main

import (
	"net/http"
	"testing"
)

func TestInitialState(t *testing.T) {
	resp, err := http.Get("http://localhost:8002/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("Expected status 204, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Expected Content-Type application/json, got %q", contentType)
	}
}
