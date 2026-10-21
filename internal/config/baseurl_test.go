package config

import "testing"

func TestLoadBaseURL(t *testing.T) {
	for _, tc := range []struct {
		env     string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"https://tend.example", "https://tend.example", false},
		{"https://tend.example/", "https://tend.example", false},
		{" http://tend.example:8080/tend/ ", "http://tend.example:8080/tend", false},
		{"tend.example", "", true},
		{"ftp://tend.example", "", true},
		{"https://", "", true},
		{"https://tend.example/?x=1", "", true},
		{"https://tend.example/#top", "", true},
	} {
		t.Setenv(EnvBaseURL, tc.env)
		got, err := LoadBaseURL()
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("TEND_BASE_URL=%q: got %q, %v; want %q, error %v", tc.env, got, err, tc.want, tc.wantErr)
		}
	}
}
