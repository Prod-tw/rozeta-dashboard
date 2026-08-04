package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRoomTokens(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
	}{
		{
			name:    "English header",
			content: "account,User ID,Token\nTR409-2@coscup.org,user-a,token-a\n",
			want:    map[string]string{"TR409-2": "token-a"},
		},
		{
			name:    "Chinese account header",
			content: "帳號,User ID,Token\nRB105@coscup.org,user-b,token-b\n",
			want:    map[string]string{"RB105": "token-b"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTokenCSV(t, test.content)
			tokens, err := loadRoomTokens(path)
			if err != nil {
				t.Fatalf("loadRoomTokens() error = %v", err)
			}
			for roomName, want := range test.want {
				if got := tokens[roomName]; got != want {
					t.Fatalf("token for %s = %q, want %q", roomName, got, want)
				}
			}
		})
	}
}

func TestLoadRoomTokensRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantError string
	}{
		{name: "missing header", content: "room,user,token-a\n", wantError: "headers"},
		{name: "wrong user ID header", content: "account,Identifier,Token\nroom,user,token-a\n", wantError: "headers"},
		{name: "empty token", content: "account,User ID,Token\nroom,user,\n", wantError: "line 2"},
		{name: "empty user ID", content: "account,User ID,Token\nroom,,token-a\n", wantError: "line 2"},
		{name: "short row", content: "account,User ID,Token\nroom,user\n", wantError: "line 2"},
		{name: "extra field", content: "account,User ID,Token\nroom,user,token,extra\n", wantError: "line 2"},
		{name: "duplicate room", content: "account,User ID,Token\nroom@coscup.org,user,one\nroom@coscup.org,user,two\n", wantError: "duplicates room"},
		{name: "duplicate token ownership", content: "account,User ID,Token\nroom-a,user-a,same\nroom-b,user-b,same\n", wantError: "reuses token"},
		{name: "duplicate account ownership", content: "account,User ID,Token\nroom-a,user-a,one\nroom-b,user-a,two\n", wantError: "reuses user ID"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadRoomTokens(writeTokenCSV(t, test.content))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("loadRoomTokens() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func writeTokenCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write CSV: %v", err)
	}
	return path
}
