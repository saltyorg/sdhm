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
	input := []byte("prefix\r\n# BEGIN DOCKER CONTAINERS\r\nold entry\r\n# END DOCKER CONTAINERS\r\nsuffix\n")
	state, section, err := locateMarkers(input, testBeginMarker, testEndMarker)
	if err != nil {
		t.Fatalf("locateMarkers() error = %v", err)
	}
	if state != validMarkers {
		t.Fatalf("locateMarkers() state = %v, want %v", state, validMarkers)
	}

	got := replaceManagedSection(input, section, []byte("172.18.0.2 radarr radarr.saltbox\n"))
	want := []byte("prefix\r\n# BEGIN DOCKER CONTAINERS\r\n172.18.0.2 radarr radarr.saltbox\n# END DOCKER CONTAINERS\r\nsuffix\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("replaceManagedSection() = %q, want %q", got, want)
	}
	if !bytes.Equal(got[:section.beginEnd], input[:section.beginEnd]) {
		t.Fatal("bytes before the managed section changed")
	}
	if !bytes.Equal(got[len(got)-len(input[section.endStart:]):], input[section.endStart:]) {
		t.Fatal("bytes from END marker onward changed")
	}
	if !bytes.Equal(bytes.TrimRight(got, "\n"), got[:len(got)-1]) {
		t.Fatal("result does not have exactly one trailing newline")
	}
}
