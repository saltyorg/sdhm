package hosts

import (
	"bytes"
	"fmt"
)

type markerState uint8

const (
	noMarkers markerState = iota
	validMarkers
)

type markerSection struct {
	beginStart int
	beginEnd   int
	endStart   int
	endEnd     int
}

// locateMarkers finds one complete, ordered marker pair without changing the
// source bytes. A trailing carriage return is ignored solely for matching a
// CRLF logical line.
func locateMarkers(data []byte, beginMarker, endMarker string) (markerState, markerSection, error) {
	var section markerSection
	beginFound := false
	endFound := false

	for lineStart := 0; lineStart < len(data); {
		lineEnd := len(data)
		lineNext := len(data)
		if newline := bytes.IndexByte(data[lineStart:], '\n'); newline >= 0 {
			lineEnd = lineStart + newline
			lineNext = lineEnd + 1
		}

		line := data[lineStart:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		switch string(line) {
		case beginMarker:
			if beginFound {
				return noMarkers, markerSection{}, fmt.Errorf("duplicate BEGIN marker")
			}
			beginFound = true
			section.beginStart = lineStart
			section.beginEnd = lineNext
		case endMarker:
			if endFound {
				return noMarkers, markerSection{}, fmt.Errorf("duplicate END marker")
			}
			endFound = true
			section.endStart = lineStart
			section.endEnd = lineNext
		}

		lineStart = lineNext
	}

	if !beginFound && !endFound {
		return noMarkers, markerSection{}, nil
	}
	if !beginFound {
		return noMarkers, markerSection{}, fmt.Errorf("END marker without BEGIN marker")
	}
	if !endFound {
		return noMarkers, markerSection{}, fmt.Errorf("BEGIN marker without END marker")
	}
	if section.endStart < section.beginStart {
		return noMarkers, markerSection{}, fmt.Errorf("END marker appears before BEGIN marker")
	}

	return validMarkers, section, nil
}

// replaceManagedSection preserves the marker lines and every byte outside the
// managed body while replacing only the bytes between the ordered markers.
func replaceManagedSection(data []byte, section markerSection, body []byte) []byte {
	replaced := make([]byte, 0, section.beginEnd+len(body)+len(data)-section.endStart)
	replaced = append(replaced, data[:section.beginEnd]...)
	replaced = append(replaced, body...)
	replaced = append(replaced, data[section.endStart:]...)
	return replaced
}
