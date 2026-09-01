package hosts

import (
	"bytes"
	"testing"
)

const (
	testBeginMarker = "# BEGIN DOCKER CONTAINERS"
	testEndMarker   = "# END DOCKER CONTAINERS"
)

func TestLocateMarkers(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantState markerState
		wantErr   bool
	}{
		{
			name:      "no marker lines",
			content:   "127.0.0.1 localhost\n",
			wantState: noMarkers,
		},
		{
			name:      "one ordered BEGIN and END",
			content:   "127.0.0.1 localhost\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n",
			wantState: validMarkers,
		},
		{
			name:    "BEGIN without END",
			content: "# BEGIN DOCKER CONTAINERS\n",
			wantErr: true,
		},
		{
			name:    "END without BEGIN",
			content: "# END DOCKER CONTAINERS\n",
			wantErr: true,
		},
		{
			name:    "END before BEGIN",
			content: "# END DOCKER CONTAINERS\n# BEGIN DOCKER CONTAINERS\n",
			wantErr: true,
		},
		{
			name:    "two BEGIN lines",
			content: "# BEGIN DOCKER CONTAINERS\n# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n",
			wantErr: true,
		},
		{
			name:    "two END lines",
			content: "# BEGIN DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n# END DOCKER CONTAINERS\n",
			wantErr: true,
		},
		{
			name:      "comment containing marker substring",
			content:   "# saved # BEGIN DOCKER CONTAINERS for reference\n",
			wantState: noMarkers,
		},
		{
			name:      "CRLF marker lines",
			content:   "127.0.0.1 localhost\r\n# BEGIN DOCKER CONTAINERS\r\n# END DOCKER CONTAINERS\r\n",
			wantState: validMarkers,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotState, _, err := locateMarkers([]byte(tt.content), testBeginMarker, testEndMarker)
			if (err != nil) != tt.wantErr {
				t.Fatalf("locateMarkers() error = %v, want error %v", err, tt.wantErr)
			}
			if err == nil && gotState != tt.wantState {
				t.Fatalf("locateMarkers() state = %v, want %v", gotState, tt.wantState)
			}
		})
	}
}

func TestReplaceManagedSectionPreservesOutsideBytes(t *testing.T) {
	tests := []struct {
		name   string
		suffix []byte
	}{
		{
			name:   "suffix without a final newline",
			suffix: []byte("# END DOCKER CONTAINERS"),
		},
		{
			name:   "suffix with multiple final newlines",
			suffix: []byte("# END DOCKER CONTAINERS\n\n\n"),
		},
		{
			name:   "suffix with trailing content",
			suffix: []byte("# END DOCKER CONTAINERS\n# user-owned content\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix := []byte("prefix\r\n# BEGIN DOCKER CONTAINERS\r\n")
			input := append(append(append([]byte{}, prefix...), []byte("old entry\r\n")...), tt.suffix...)
			state, section, err := locateMarkers(input, testBeginMarker, testEndMarker)
			if err != nil {
				t.Fatalf("locateMarkers() error = %v", err)
			}
			if state != validMarkers {
				t.Fatalf("locateMarkers() state = %v, want %v", state, validMarkers)
			}

			body := []byte("172.18.0.2 radarr radarr.saltbox\n")
			got := replaceManagedSection(input, section, body)
			want := append(append(append([]byte{}, prefix...), body...), tt.suffix...)
			if !bytes.Equal(got, want) {
				t.Fatalf("replaceManagedSection() = %q, want %q", got, want)
			}
			if !bytes.Equal(got[:section.beginEnd], input[:section.beginEnd]) {
				t.Fatal("bytes before the managed section changed")
			}
			if !bytes.Equal(got[len(got)-len(tt.suffix):], tt.suffix) {
				t.Fatal("bytes from END marker onward changed")
			}
		})
	}
}
