package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CurrentFormatVersion is the version written to the format marker.
const CurrentFormatVersion = 4

// FormatMarker is the content of format.gdb.
type FormatMarker struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	// Conversion is "in-progress" while a conversion between formats runs.
	Conversion string `json:"conversion,omitempty"`
	// Target is the format a running conversion converts to.
	Target string `json:"target,omitempty"`
}

// ConversionInProgress is the value of FormatMarker.Conversion while a
// conversion runs.
const ConversionInProgress = "in-progress"

// NewFormatMarker returns the marker of a finished current-format directory.
func NewFormatMarker() FormatMarker {
	return FormatMarker{Format: "genesisdb", Version: CurrentFormatVersion}
}

// IsConverting reports whether a conversion was started and not finished.
func (m FormatMarker) IsConverting() bool {
	return m.Conversion == ConversionInProgress
}

// ConversionTarget returns the target format of a running conversion.
func (m FormatMarker) ConversionTarget() Format {
	if m.Target == FormatLegacy.String() {
		return FormatLegacy
	}
	return FormatCurrent
}

// Encode returns the marker as one JSON line.
func (m FormatMarker) Encode() []byte {
	encoded, _ := json.Marshal(m)
	return append(encoded, '\n')
}

// ErrNoFormatMarker reports that a data directory has no format marker.
var ErrNoFormatMarker = errors.New("no format marker")

// ReadFormatMarker reads format.gdb of a data directory.
func ReadFormatMarker(dataDirectory string) (FormatMarker, error) {
	content, err := os.ReadFile(filepath.Join(dataDirectory, FormatMarkerFile))
	if errors.Is(err, os.ErrNotExist) {
		return FormatMarker{}, ErrNoFormatMarker
	}
	if err != nil {
		return FormatMarker{}, err
	}
	var marker FormatMarker
	if err := json.Unmarshal(content, &marker); err != nil || marker.Format != "genesisdb" {
		return FormatMarker{}, fmt.Errorf("%s is not a valid GenesisDB format marker", filepath.Join(dataDirectory, FormatMarkerFile))
	}
	return marker, nil
}
